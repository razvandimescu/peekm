package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	ttmpl "text/template"
	"time"
)

// standupMarkdownTmpl renders the Save artifact — plain markdown that survives a
// Slack/GitHub paste and re-enters peekm as a whitelisted, shareable file.
var standupMarkdownTmpl = ttmpl.Must(ttmpl.New("standup-md").Parse(standupMarkdownSource))

const standupMarkdownSource = `# Standup — {{.Label}}
{{if not .IsEmpty}}{{.TotalActive}} active · {{.TotalCommits}} commits · {{.ProjectCount}} projects
{{range .Headline}}
## {{.Name}}{{if .Branch}} ` + "`{{.Branch}}`" + `{{end}} — {{range $i, $m := .Metrics}}{{if $i}} · {{end}}{{$m}}{{end}}{{if or .Insertions .Deletions}} · +{{.Insertions}} −{{.Deletions}}{{end}}
{{range .HeadCommits}}- {{.Subject}}
{{end}}{{if .MoreCommits}}- …{{len .MoreCommits}} more
{{end}}{{if and (not .HeadCommits) .EvidenceFiles}}Edited: {{range $i, $f := .EvidenceFiles}}{{if $i}}, {{end}}{{$f}}{{end}}{{if gt .MoreFiles 0}} +{{.MoreFiles}} more{{end}}
{{end}}{{if and (not .HeadCommits) (not .EvidenceFiles) .Prompt}}_{{.Prompt}}_
{{end}}
_{{len .Sessions}} session{{if ne (len .Sessions) 1}}s{{end}} · {{.SessionRange}}_
{{end}}{{if .Tail}}
### Also touched
{{range .Tail}}- {{.TailSummary}}
{{end}}{{end}}{{else}}No sessions this day.
{{end}}`

// standupIdleGap is the maximum gap between two records still counted as active
// work. It is a const, not a flag — see docs/STANDUP_SUMMARY.md ("A configurable
// idle threshold" cut list).
const standupIdleGap = 5 * time.Minute

// standupHeadlineCap bounds the number of full project blocks; the rest fold
// into the "Also touched" tail.
const standupHeadlineCap = 5

// standupTailEditThreshold: a project with no commits and fewer than this many
// edits is a tail entry, not a headline block.
const standupTailEditThreshold = 10

var standupEditTools = map[string]bool{"Edit": true, "Write": true, "NotebookEdit": true}

// standupFileRowsShown caps the visible rows of the "files AI edited" list; the
// remainder is marked Extra and hides behind "+N more".
const standupFileRowsShown = 8

// depBumpRe matches dependency-bump commit subjects, collapsed to a count so a
// bot commit never occupies an evidence bullet.
var depBumpRe = regexp.MustCompile(`(?i)^(chore\(deps\)|build\(deps\)|bump )`)

// ---- types ----

type standupCommit struct {
	Short   string
	Subject string
}

type standupSession struct {
	ID    string
	Range string // "14:21–22:27"
}

// fileStat accumulates one file's edit activity across a day, first per
// transcript slice, then merged per project.
type fileStat struct {
	edits     int
	tools     map[string]int // tool name → call count
	last      time.Time
	toolID    string // tool_use id of the latest edit, anchors the transcript view
	sessionID string // session owning the latest edit (set on merge)
}

// standupFile is one rendered row of the "files AI edited" disclosure.
type standupFile struct {
	Dir         string // root-relative directory prefix incl. trailing "/", dimmed
	Base        string
	AbsPath     string
	ViewPath    string // non-empty when whitelisted markdown → links to /view
	SessionID   string
	ToolID      string
	Edits       int
	Tools       string // "Edit, Write" — dominant first
	Last        string // "16:41"
	Dot         string // dominant tool's ribbonClassNames entry, colors the row dot
	Extra       bool   // beyond standupFileRowsShown, hidden until "+N more"
	Uncommitted bool   // AI-edited but still dirty in the working tree (today only)

	rel   string    // root-relative slash path, keys the dirty lookup
	lastT time.Time // sort key; Last is its display form
}

type standupProject struct {
	Name          string
	Branch        string
	Commits       []standupCommit // real (non-dep-bump) commits
	HeadCommits   []standupCommit // first 2, rendered inline
	MoreCommits   []standupCommit // remainder, behind <details>
	DepBumps      int
	Insertions    int
	Deletions     int
	Edits         int
	Files         int
	ActiveStr     string
	Sessions      []standupSession
	SessionRange  string        // combined earliest–latest, e.g. "08:13–18:34"
	EvidenceFiles []string      // top edited basenames (evidence rung 2)
	MoreFiles     int           // distinct files beyond the shown basenames
	Prompt        string        // opening prompt (evidence rung 3)
	FileRows      []standupFile // per-file rows for the disclosure, most-edited first
	MoreFileRows  int           // rows beyond standupFileRowsShown, hidden until expanded
	Metrics       []string      // non-zero metric segments for the headline rail
	TailSummary   string        // one-line metric summary for the "Also touched" strip
	Ribbon        []ribbonCell  // time-bucketed activity rhythm for the day
	OutputTokens  int           // deduped assistant output tokens (live view only, never exported)
	CacheWrite    int           // cache_creation tokens, tooltip breakdown only
	CacheRead     int           // cache_read tokens, tooltip breakdown only

	active time.Duration // ranking key, not rendered
}

// toolEvent is one write/edit/bash tool call, kept with its timestamp so the
// activity ribbon can bucket work by clock time.
type toolEvent struct {
	Time  time.Time
	Class int // ribbonClassNames index (0 write, 1 edit, 2 bash)
}

// standupRibbonCells is the fixed column count of the per-project activity ribbon.
const standupRibbonCells = 28

type standupDay struct {
	Date               string // YYYY-MM-DD
	Label              string // "Monday, 21 July 2026"
	Headline           []standupProject
	Tail               []standupProject
	TotalActive        string
	TotalCommits       int
	ProjectCount       int
	PrevDate           string
	NextDate           string // empty when the target is today (no forward step)
	IsEmpty            bool
	NearestActiveDate  string // for the empty-state link
	NearestActiveLabel string
}

type standupTemplateData struct {
	baseTemplateData
	TreeHTML   template.HTML
	BrowsePath string
	Day        standupDay
}

// ---- transcript day slicing ----

type standupRecord struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		ID    string `json:"id"`
		Usage *struct {
			OutputTokens        int `json:"output_tokens"`
			CacheCreationTokens int `json:"cache_creation_input_tokens"`
			CacheReadTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type standupBlock struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Name  string `json:"name"`
	ID    string `json:"id"` // tool_use id, kept for transcript anchors
	Input struct {
		FilePath string `json:"file_path"`
	} `json:"input"`
}

// daySlice holds the per-day metrics extracted from one transcript.
type daySlice struct {
	stamps     []time.Time
	edits      int
	files      map[string]*fileStat // edited files only — reads never register
	prompts    []string
	tools      []toolEvent // write/edit/bash calls, for the activity ribbon
	outTokens  int
	cacheWrite int
	cacheRead  int
}

// standupContent decodes a record's message.content, which is either a bare
// string (user prompt) or an array of typed blocks.
func standupContent(raw json.RawMessage) (text string, blocks []standupBlock) {
	if len(raw) == 0 {
		return
	}
	if raw[0] == '"' {
		_ = json.Unmarshal(raw, &text)
		return
	}
	_ = json.Unmarshal(raw, &blocks)
	return
}

// sliceTranscriptDay parses a transcript keeping only records stamped on the
// target day (local), collecting timestamps, edit counts, touched files, and
// user prompts. Returns nil when the transcript contributes nothing that day.
func sliceTranscriptDay(path string, dayStr string) *daySlice {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	ds := &daySlice{files: map[string]*fileStat{}}
	seen := map[string]bool{} // message IDs whose usage is already counted
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var rec standupRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		t, ok := parseTimestamp(rec.Timestamp)
		if !ok || t.Local().Format("2006-01-02") != dayStr {
			continue
		}
		ds.stamps = append(ds.stamps, t)
		addUsage(ds, &rec, seen)

		text, blocks := standupContent(rec.Message.Content)
		if rec.Type == "user" {
			if p := standupPromptLine(text, blocks); p != "" {
				ds.prompts = append(ds.prompts, p)
			}
		}
		for _, b := range blocks {
			if b.Type != "tool_use" {
				continue
			}
			if standupEditTools[b.Name] {
				ds.edits++
				recordFileEdit(ds, &b, t)
			}
			// The ribbon shows shipping rhythm: write/edit/bash only, not reads.
			if cls := ribbonToolClass(b.Name); cls != 3 {
				ds.tools = append(ds.tools, toolEvent{Time: t, Class: cls})
			}
		}
	}
	if len(ds.stamps) == 0 {
		return nil
	}
	return ds
}

// recordFileEdit folds one edit-tool call into the slice's per-file stats,
// keeping the latest call's tool_use id as the transcript anchor.
func recordFileEdit(ds *daySlice, b *standupBlock, t time.Time) {
	fp := b.Input.FilePath
	if fp == "" {
		return
	}
	fs := ds.files[fp]
	if fs == nil {
		fs = &fileStat{tools: map[string]int{}}
		ds.files[fp] = fs
	}
	fs.edits++
	fs.tools[b.Name]++
	if t.After(fs.last) {
		fs.last = t
		fs.toolID = b.ID
	}
}

// addUsage sums a record's token usage once per assistant message. Claude Code
// writes one JSONL record per content block, each repeating the same usage —
// summing without deduping by message ID over-counts ~2.5x.
func addUsage(ds *daySlice, rec *standupRecord, seen map[string]bool) {
	u := rec.Message.Usage
	if rec.Type != "assistant" || u == nil || rec.Message.ID == "" || seen[rec.Message.ID] {
		return
	}
	seen[rec.Message.ID] = true
	ds.outTokens += u.OutputTokens
	ds.cacheWrite += u.CacheCreationTokens
	ds.cacheRead += u.CacheReadTokens
}

// standupPromptLine returns the first line of a user prompt, filtered of system
// noise, or "" when the message is noise or empty.
func standupPromptLine(text string, blocks []standupBlock) string {
	s := text
	if s == "" {
		for _, b := range blocks {
			if b.Type == "text" {
				s = b.Text
				break
			}
		}
	}
	s = strings.TrimSpace(s)
	if s == "" || isSystemNoise(s) {
		return ""
	}
	return truncateString(strings.SplitN(s, "\n", 2)[0], 110)
}

// activeTime sums the gaps between consecutive timestamps, counting only gaps
// no longer than standupIdleGap. This is the plan's core metric — wall-clock is
// meaningless because sessions span midnight and sit idle.
func activeTime(stamps []time.Time) time.Duration {
	if len(stamps) < 2 {
		return 0
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })
	var total time.Duration
	for i := 1; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap <= standupIdleGap {
			total += gap
		}
	}
	return total
}

// ---- git commits (the outcome) ----

type commitResult struct {
	commits    []standupCommit
	depBumps   int
	insertions int
	deletions  int
}

var (
	commitCacheMu sync.Mutex
	commitCache   = map[string]*commitResult{} // keyed by root+day; past days are immutable
)

// gitCommitsForDay returns the authored commits for one repo on one day,
// memoised per (repo, day) so prev/next clicking doesn't re-exec git per project
// per keypress. The author filter is absolute (repo's user.email); an unset
// identity yields no commits rather than someone else's pulled-in work.
func gitCommitsForDay(root, dayStr string) *commitResult {
	key := root + "\x00" + dayStr
	commitCacheMu.Lock()
	if r, ok := commitCache[key]; ok {
		commitCacheMu.Unlock()
		return r
	}
	commitCacheMu.Unlock()

	r := computeGitCommits(root, dayStr)
	commitCacheMu.Lock()
	commitCache[key] = r
	commitCacheMu.Unlock()
	return r
}

func gitOutput(root string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func computeGitCommits(root, dayStr string) *commitResult {
	r := &commitResult{}
	email := gitOutput(root, "config", "user.email")
	if email == "" {
		return r // author filter is absolute — no identity, no commits
	}
	since := dayStr + " 00:00:00"
	until := dayStr + " 23:59:59"
	// One pass yields both subjects and churn: --shortstat appends a
	// " N files changed, X insertions(+), Y deletions(-)" line after each commit.
	// --all spans every branch so your own commits on a branch you've since
	// switched away from still count; the absolute --author=<email> filter keeps
	// a pulled-in teammate commit (their email) out regardless.
	out := gitOutput(root, "log", "--all", "--no-merges", "--since="+since, "--until="+until,
		"--author="+email, "--shortstat", "--pretty=format:%h%x09%s")
	for _, line := range strings.Split(out, "\n") {
		if parts := strings.SplitN(line, "\t", 2); len(parts) == 2 {
			if depBumpRe.MatchString(parts[1]) {
				r.depBumps++
			} else {
				r.commits = append(r.commits, standupCommit{Short: parts[0], Subject: parts[1]})
			}
			continue
		}
		if m := insertionsRe.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			r.insertions += n
		}
		if m := deletionsRe.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			r.deletions += n
		}
	}
	return r
}

var (
	insertionsRe = regexp.MustCompile(`(\d+) insertion`)
	deletionsRe  = regexp.MustCompile(`(\d+) deletion`)
)

// ---- assembly ----

type projectAccum struct {
	name         string
	branch       string
	root         string
	active       time.Duration
	edits        int
	files        map[string]*fileStat
	sessions     []standupSession
	sessionStart time.Time
	sessionEnd   time.Time
	prompt       string
	promptTime   time.Time
	tools        []toolEvent
	outTokens    int
	cacheWrite   int
	cacheRead    int
}

// collectProjectSlices walks every transcript under browseDir, gates each on a
// cheap head/tail read, fully parses the survivors, and groups their per-day
// metrics by git repo root.
func collectProjectSlices(browseDir, dayStr string) map[string]*projectAccum {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	projectsDir := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}

	accums := map[string]*projectAccum{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projectDir := resolveProjectDir(entry.Name())
		if projectDir == "" || !dirWithinBase(projectDir, browseDir) {
			continue
		}
		root := projectDir
		if gr, err := findGitRoot(projectDir); err == nil {
			root = gr
		}
		sessionsDir := filepath.Join(projectsDir, entry.Name())
		addSessionsForProject(accums, sessionsDir, projectDir, root, dayStr)
	}
	return accums
}

func addSessionsForProject(accums map[string]*projectAccum, sessionsDir, projectDir, root, dayStr string) {
	files, err := os.ReadDir(sessionsDir)
	if err != nil {
		return
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(sessionsDir, f.Name())
		_, first, last := extractTranscriptMeta(path) // cheap gate
		if !spansDay(first, last, dayStr) {
			continue
		}
		slice := sliceTranscriptDay(path, dayStr)
		if slice == nil {
			continue
		}
		mergeSlice(accums, root, projectDir, strings.TrimSuffix(f.Name(), ".jsonl"), slice)
	}
}

func spansDay(first, last time.Time, dayStr string) bool {
	if first.IsZero() || last.IsZero() {
		return false
	}
	return first.Local().Format("2006-01-02") <= dayStr && dayStr <= last.Local().Format("2006-01-02")
}

func mergeSlice(accums map[string]*projectAccum, root, projectDir, sessionID string, slice *daySlice) {
	a := accums[root]
	if a == nil {
		a = &projectAccum{name: filepath.Base(root), root: root, files: map[string]*fileStat{}}
		if ri := detectRepoInfo(root); ri != nil {
			a.branch = ri.Branch
		}
		accums[root] = a
	}

	a.active += activeTime(slice.stamps) // sorts slice.stamps ascending
	a.edits += slice.edits
	a.tools = append(a.tools, slice.tools...)
	a.outTokens += slice.outTokens
	a.cacheWrite += slice.cacheWrite
	a.cacheRead += slice.cacheRead
	for fp, fs := range slice.files {
		af := a.files[fp]
		if af == nil {
			af = &fileStat{tools: map[string]int{}}
			a.files[fp] = af
		}
		af.edits += fs.edits
		for tool, n := range fs.tools {
			af.tools[tool] += n
		}
		if fs.last.After(af.last) {
			af.last = fs.last
			af.toolID = fs.toolID
			af.sessionID = sessionID
		}
	}
	start, end := slice.stamps[0], slice.stamps[len(slice.stamps)-1]
	a.sessions = append(a.sessions, standupSession{
		ID:    truncateSessionID(sessionID),
		Range: start.Local().Format("15:04") + "–" + end.Local().Format("15:04"),
	})
	if a.sessionStart.IsZero() || start.Before(a.sessionStart) {
		a.sessionStart = start
	}
	if end.After(a.sessionEnd) {
		a.sessionEnd = end
	}
	// Opening intent: the earliest session's first prompt.
	if len(slice.prompts) > 0 && (a.promptTime.IsZero() || start.Before(a.promptTime)) {
		a.prompt = slice.prompts[0]
		a.promptTime = start
	}
}

func buildStandupDay(browseDir string, day time.Time) standupDay {
	dayStr := day.Format("2006-01-02")
	accums := collectProjectSlices(browseDir, dayStr)

	projects := make([]standupProject, 0, len(accums))
	for _, a := range accums {
		projects = append(projects, finalizeProject(a, dayStr))
	}
	rankProjects(projects)

	sd := standupDay{
		Date:  dayStr,
		Label: day.Format("Monday, 2 January 2006"),
	}
	for _, p := range projects {
		sd.TotalCommits += len(p.Commits)
	}
	sd.ProjectCount = len(projects)
	sd.TotalActive = formatActive(sumActive(projects))
	splitHeadlineTail(&sd, projects)

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	sd.PrevDate = day.AddDate(0, 0, -1).Format("2006-01-02")
	if next := day.AddDate(0, 0, 1); !next.After(today) {
		sd.NextDate = next.Format("2006-01-02")
	}

	sd.IsEmpty = len(sd.Headline) == 0 && len(sd.Tail) == 0
	if sd.IsEmpty {
		if nd, ok := nearestActiveDay(browseDir, day); ok {
			sd.NearestActiveDate = nd.Format("2006-01-02")
			sd.NearestActiveLabel = nd.Format("Monday, 2 January")
		}
	}
	return sd
}

func finalizeProject(a *projectAccum, dayStr string) standupProject {
	p := standupProject{
		Name:         a.name,
		Branch:       a.branch,
		Edits:        a.edits,
		Files:        len(a.files),
		Prompt:       a.prompt,
		Sessions:     a.sessions,
		OutputTokens: a.outTokens,
		CacheWrite:   a.cacheWrite,
		CacheRead:    a.cacheRead,
		active:       a.active,
		ActiveStr:    formatActive(a.active),
		SessionRange: a.sessionStart.Local().Format("15:04") + "–" + a.sessionEnd.Local().Format("15:04"),
	}
	sort.Slice(p.Sessions, func(i, j int) bool { return p.Sessions[i].Range < p.Sessions[j].Range })

	cr := gitCommitsForDay(a.root, dayStr)
	p.Commits = cr.commits
	p.DepBumps = cr.depBumps
	p.Insertions = cr.insertions
	p.Deletions = cr.deletions

	p.FileRows = buildFileRows(a, dayStr)
	if len(p.FileRows) > standupFileRowsShown {
		p.MoreFileRows = len(p.FileRows) - standupFileRowsShown
	}
	fillEvidence(&p)
	p.Metrics = headlineMetrics(&p)
	p.TailSummary = tailSummary(&p)
	p.Ribbon = buildStandupRibbon(a.tools)
	return p
}

// buildStandupRibbon turns a project's chronological write/edit/bash calls into
// fixed activity columns. Buckets are by event index, not clock time — the work
// day is mostly idle wall-clock, so time-bucketing would read as grey; index
// buckets show the shape of the shipping instead (matching the timeline ribbon).
// Cell colour is by precedence (write > edit > bash) so a burst of editing reads
// as editing even when bash calls out-number it; height tracks density.
func buildStandupRibbon(events []toolEvent) []ribbonCell {
	n := len(events)
	if n == 0 {
		return nil
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Time.Before(events[j].Time) })

	cells := min(n, standupRibbonCells)
	counts := make([][3]int, cells)
	totals := make([]int, cells)
	for i, e := range events {
		bi := i * cells / n
		counts[bi][e.Class]++
		totals[bi]++
	}
	maxTotal := 1
	for _, t := range totals {
		if t > maxTotal {
			maxTotal = t
		}
	}

	out := make([]ribbonCell, cells)
	for i := range out {
		dom := 2
		for cls := 0; cls < 3; cls++ {
			if counts[i][cls] > 0 {
				dom = cls
				break
			}
		}
		out[i] = ribbonCell{Tool: ribbonClassNames[dom], Height: 40 + totals[i]*60/maxTotal}
	}
	return out
}

// fillEvidence applies the evidence ladder: commits, else edited basenames (the
// markdown export's "Edited:" line — the live view renders FileRows), else the
// opening prompt already on p.Prompt. FileRows must be built first: its ranking
// is the single source of "top files".
func fillEvidence(p *standupProject) {
	if len(p.Commits) > 0 {
		if len(p.Commits) > 2 {
			p.HeadCommits = p.Commits[:2]
			p.MoreCommits = p.Commits[2:]
		} else {
			p.HeadCommits = p.Commits
		}
		return
	}
	for _, row := range p.FileRows[:min(len(p.FileRows), 3)] {
		p.EvidenceFiles = append(p.EvidenceFiles, row.Base)
	}
	p.MoreFiles = len(p.FileRows) - len(p.EvidenceFiles)
}

// buildFileRows turns a project's per-file stats into rendered rows, most-edited
// first. Uncommitted markers come from the working tree, so only today qualifies
// — past days are immutable but the tree is not, hence no caching.
func buildFileRows(a *projectAccum, dayStr string) []standupFile {
	if len(a.files) == 0 {
		return nil
	}
	rows := make([]standupFile, 0, len(a.files))
	for fp, fs := range a.files {
		rel := fp
		if r, err := filepath.Rel(a.root, fp); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
		row := standupFile{
			AbsPath:   fp,
			Base:      filepath.Base(rel),
			Edits:     fs.edits,
			Tools:     toolMix(fs.tools),
			Last:      fs.last.Local().Format("15:04"),
			Dot:       dominantToolClass(fs.tools),
			SessionID: fs.sessionID,
			ToolID:    fs.toolID,
			rel:       filepath.ToSlash(rel),
			lastT:     fs.last,
		}
		if d := filepath.Dir(rel); d != "." && d != string(filepath.Separator) {
			row.Dir = filepath.ToSlash(d) + "/"
		}
		if isWhitelistedFile(fp) {
			// /view/ resolves against browseDir — absolute paths 404.
			row.ViewPath = getRelativePath(fp)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Edits != rows[j].Edits {
			return rows[i].Edits > rows[j].Edits
		}
		if !rows[i].lastT.Equal(rows[j].lastT) {
			return rows[i].lastT.After(rows[j].lastT)
		}
		return rows[i].Base < rows[j].Base
	})
	for i := range rows {
		rows[i].Extra = i >= standupFileRowsShown
	}
	if dayStr == time.Now().Format("2006-01-02") {
		rels := make([]string, len(rows))
		for i, r := range rows {
			rels[i] = r.rel
		}
		dirty := dirtyFiles(a.root, rels)
		for i := range rows {
			rows[i].Uncommitted = dirty[rows[i].rel]
		}
	}
	return rows
}

// toolMix renders a file's tool usage as "Edit, Write", dominant tool first.
func toolMix(tools map[string]int) string {
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if tools[names[i]] != tools[names[j]] {
			return tools[names[i]] > tools[names[j]]
		}
		return names[i] < names[j]
	})
	return strings.Join(names, ", ")
}

// dominantToolClass classifies a file's most-used tool through the shared
// ribbon mapping, so the row dot and the activity ribbon can never disagree.
func dominantToolClass(tools map[string]int) string {
	best, bestN := "", 0
	for name, n := range tools {
		if n > bestN || (n == bestN && name < best) {
			best, bestN = name, n
		}
	}
	return ribbonClassNames[ribbonToolClass(best)]
}

// dirtyFiles returns which of the given root-relative paths have uncommitted
// changes, per `git status --porcelain -uall` limited to those pathspecs (a
// handful of stats instead of a full-tree walk). Rename targets included.
// Absolute rels (edits outside root, e.g. ~/.claude writes) are skipped — one
// outside-repo pathspec fails the whole git call. Status codes are cut by
// trimming to the first space — gitOutput trims the leading space off the
// first line, so a fixed-column parse would be off by one.
func dirtyFiles(root string, rels []string) map[string]bool {
	out := map[string]bool{}
	var specs []string
	for _, rel := range rels {
		if !filepath.IsAbs(rel) {
			specs = append(specs, rel)
		}
	}
	if len(specs) == 0 {
		return out
	}
	args := append([]string{"status", "--porcelain", "-uall", "--"}, specs...)
	for _, line := range strings.Split(gitOutput(root, args...), "\n") {
		_, p, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		p = strings.TrimSpace(p)
		if i := strings.Index(p, " -> "); i >= 0 {
			p = p[i+4:]
		}
		out[strings.Trim(p, `"`)] = true
	}
	return out
}

// headlineMetrics builds the per-project rail, appending only non-zero segments
// (never render a zero).
func headlineMetrics(p *standupProject) []string {
	segs := []string{p.ActiveStr + " active"}
	if p.Edits > 0 {
		segs = append(segs, fmt.Sprintf("%d %s", p.Edits, plural(p.Edits, "edit")))
	}
	if p.Files > 0 {
		segs = append(segs, fmt.Sprintf("%d %s", p.Files, plural(p.Files, "file")))
	}
	if p.DepBumps > 0 {
		segs = append(segs, fmt.Sprintf("%d dep %s", p.DepBumps, plural(p.DepBumps, "bump")))
	}
	return segs
}

// tailSummary is the one-liner for the "Also touched" strip. An honest finding,
// never "0 commits": a discussion-only project reads "extras_gpt 1.3h · discussion".
func tailSummary(p *standupProject) string {
	var detail string
	switch {
	case len(p.Commits) > 0:
		detail = fmt.Sprintf("%d %s", len(p.Commits), plural(len(p.Commits), "commit"))
	case p.Edits > 0:
		detail = fmt.Sprintf("%d %s", p.Edits, plural(p.Edits, "edit"))
	default:
		detail = "discussion"
	}
	if p.DepBumps > 0 {
		detail += fmt.Sprintf(", %d dep %s", p.DepBumps, plural(p.DepBumps, "bump"))
	}
	return fmt.Sprintf("%s %s · %s", p.Name, p.ActiveStr, detail)
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// rankProjects orders by evidence — commits, then edits, then active time —
// never by wall-clock (the dry run's central correction).
func rankProjects(projects []standupProject) {
	sort.SliceStable(projects, func(i, j int) bool {
		a, b := projects[i], projects[j]
		if len(a.Commits) != len(b.Commits) {
			return len(a.Commits) > len(b.Commits)
		}
		if a.Edits != b.Edits {
			return a.Edits > b.Edits
		}
		return a.active > b.active
	})
}

// splitHeadlineTail promotes commit-bearing or heavily-edited projects to full
// blocks (capped), folding the rest into the "Also touched" strip.
func splitHeadlineTail(sd *standupDay, projects []standupProject) {
	for _, p := range projects {
		isHeadline := len(p.Commits) > 0 || p.Edits >= standupTailEditThreshold
		if isHeadline && len(sd.Headline) < standupHeadlineCap {
			sd.Headline = append(sd.Headline, p)
		} else {
			sd.Tail = append(sd.Tail, p)
		}
	}
}

func sumActive(projects []standupProject) time.Duration {
	var total time.Duration
	for _, p := range projects {
		total += p.active
	}
	return total
}

func formatActive(d time.Duration) string {
	return fmt.Sprintf("%.1fh", d.Hours())
}

// formatCompact renders token magnitudes tersely: 840, 4.2k, 98k, 1.2M.
func formatCompact(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 10000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
	case n < 1000000:
		return strconv.Itoa(n/1000) + "k"
	default:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e6), ".0") + "M"
	}
}

// nearestActiveDay finds the most recent day on or before `before`-1 with any
// transcript activity, for the empty-state "jump to a real day" affordance.
func nearestActiveDay(browseDir string, before time.Time) (time.Time, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return time.Time{}, false
	}
	projectsDir := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return time.Time{}, false
	}
	cutoff := before // strictly before the target day
	var best time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projectDir := resolveProjectDir(entry.Name())
		if projectDir == "" || !dirWithinBase(projectDir, browseDir) {
			continue
		}
		sessionsDir := filepath.Join(projectsDir, entry.Name())
		files, err := os.ReadDir(sessionsDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			_, _, last := extractTranscriptMeta(filepath.Join(sessionsDir, f.Name()))
			if last.IsZero() {
				continue
			}
			d := time.Date(last.Year(), last.Month(), last.Day(), 0, 0, 0, 0, last.Location())
			if d.Before(cutoff) && d.After(best) {
				best = d
			}
		}
	}
	return best, !best.IsZero()
}

// ---- handlers ----

func serveStandup(w http.ResponseWriter, r *http.Request) {
	fileMutex.RLock()
	browse := browseDir
	fileMutex.RUnlock()

	day := standupTargetDay(r.URL.Query().Get("d"))
	data := standupTemplateData{
		baseTemplateData: newBaseTemplateData(),
		TreeHTML:         template.HTML(sidebarTreeHTML(r)),
		BrowsePath:       browse,
		Day:              buildStandupDay(browse, day),
	}
	renderTemplatePair(w, r, standupTmpl, standupPartialTmpl, data)
}

// standupTargetDay resolves the requested day: an explicit ?d=YYYY-MM-DD
// (bounded at today), else calendar yesterday, local time.
func standupTargetDay(q string) time.Time {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if q != "" {
		if t, err := time.ParseInLocation("2006-01-02", q, now.Location()); err == nil {
			if t.After(today) {
				return today
			}
			return t
		}
	}
	return today.AddDate(0, 0, -1)
}

// handleStandupSave writes standup-YYYY-MM-DD.md into the browse dir and
// registers it into the whitelist so it is immediately viewable and shareable.
func handleStandupSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Date string `json:"date"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	fileMutex.RLock()
	browse := browseDir
	fileMutex.RUnlock()

	day := standupTargetDay(req.Date)
	sd := buildStandupDay(browse, day)

	var buf bytes.Buffer
	if err := standupMarkdownTmpl.Execute(&buf, sd); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}

	name := "standup-" + sd.Date + ".md"
	abs := filepath.Join(browse, name)
	if err := atomicWriteFile(abs, buf.String()); err != nil {
		http.Error(w, "write error", http.StatusInternalServerError)
		return
	}
	handleMarkdownCreated(abs) // whitelist + live file_added broadcast

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"path": name})
}
