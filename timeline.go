package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func findGitRoot(startDir string) (string, error) {
	dir := startDir
	for {
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("not a git repository (or any parent up to /)")
		}
		dir = parent
	}
}

// Timeline template data types

type timelineTemplateData struct {
	baseTemplateData
	TreeHTML   template.HTML
	Title      string
	Subtitle   string
	BrowsePath string
	Groups     []timelineDayGroup
	RepoInfo   *repoInfo
}

type repoInfo struct {
	Name   string // e.g. "peekm"
	Branch string // e.g. "main"
	Remote string // e.g. "github.com/razvandimescu/peekm"
}

type sessionStats struct {
	FileCount int
	EditCount int
	Duration  string
	Tools     string // e.g. "Edit: 14, Write: 3"
}

type timelineSession struct {
	SessionID      string // truncated 8 chars
	FullSessionID  string
	Summary        string // first user prompt (truncated)
	Project        string // project name from CWD (e.g. "peekm")
	Duration       string // e.g. "12m", "< 1s"
	FileCount      int
	EditCount      int
	Tools          []string // unique tool names
	HasTranscript  bool
	IsActive       bool   // newest event or heartbeat < 5min ago
	LastTool       string // most recent tool call (from heartbeat)
	LastToolAgo    string // relative time of last tool call (for tooltip)
	LastToolDetail string // tool input summary (e.g. command, file path, pattern)
	SessionType    string // "edit" or "conversation"
	Source         string // harness badge: "" (Claude Code, unbadged) or "pi"
	Events         []timelineEntry
	Ribbon         []ribbonCell // spatial activity strip (chronological)
	newestTime     time.Time
	oldestTime     time.Time
}

// ribbonCell is one column of a session's activity ribbon: a tool-colored bar
// whose height encodes relative edit volume at that point in the session.
type ribbonCell struct {
	Tool   string // "write" | "edit" | "bash" | "other" (CSS class suffix)
	Height int    // 16..100 (percent of ribbon height)
}

type timelineDayGroup struct {
	Label    string
	Sessions []timelineSession
}

type timelineEntry struct {
	FilePath   string // relative to browseDir (for display + view links)
	AbsPath    string // absolute path (for copy button)
	ToolName   string
	TimeAgo    string
	TimeISO    string
	IsViewable bool      // true if file is in the markdown whitelist
	EditCount  int       // 1 = single event, >1 = aggregated
	PlanTitle  string    // non-empty for plan files (extracted from content)
	oldestTime time.Time // unexported, used for time range computation
	newestTime time.Time // unexported, used for time range computation
}

func dayLabel(t time.Time) string {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	yesterday := today.AddDate(0, 0, -1)
	switch {
	case !t.Before(today):
		return "Today"
	case !t.Before(yesterday):
		return "Yesterday"
	default:
		return t.Format("Jan 2, 2006")
	}
}

func formatSessionDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return "< 1s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
}

type transcriptInfo struct {
	hasTranscript bool
	summary       string
}

func buildTranscriptCache(events []SessionEvent) map[string]transcriptInfo {
	cache := make(map[string]transcriptInfo)
	for _, evt := range events {
		if evt.SessionID == "" {
			continue
		}
		if _, checked := cache[evt.SessionID]; !checked {
			path := resolveTranscriptPath(evt.SessionID)
			cache[evt.SessionID] = transcriptInfo{
				hasTranscript: path != "",
				summary:       extractSessionSummary(path),
			}
		}
	}
	return cache
}

// dirWithinBase reports whether dir is baseDir or lives under it.
func dirWithinBase(dir, baseDir string) bool {
	return filepath.Clean(dir) == filepath.Clean(baseDir) || isWithinDir(dir, baseDir)
}

// discoverTranscriptSessions scans Claude transcript files for sessions
// not already tracked via events.jsonl (i.e. conversation-only sessions).
// Every project under baseDir is scanned, matching how eventsForDir walks the
// subtree — browsing a parent folder must still surface its projects' sessions.
func discoverTranscriptSessions(baseDir string, knownSessionIDs map[string]bool) []timelineSession {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	projectsDir := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}

	var sessions []timelineSession
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projectDir := resolveProjectDir(entry.Name())
		if projectDir == "" || !dirWithinBase(projectDir, baseDir) {
			continue
		}
		sessions = append(sessions, transcriptSessionsIn(
			filepath.Join(projectsDir, entry.Name()), projectDir, knownSessionIDs)...)
	}
	// Sort newest first for merge
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].newestTime.After(sessions[j].newestTime)
	})
	return sessions
}

// transcriptSessionsIn builds a session per transcript in one project's session directory.
func transcriptSessionsIn(sessionsDir, projectDir string, knownSessionIDs map[string]bool) []timelineSession {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil
	}

	var sessions []timelineSession
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		sessionID := strings.TrimSuffix(entry.Name(), ".jsonl")
		if knownSessionIDs[sessionID] {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}
		modTime := info.ModTime()
		transcriptPath := filepath.Join(sessionsDir, entry.Name())
		summary, firstTS, lastTS := extractTranscriptMeta(transcriptPath)
		oldest, newest := modTime, modTime
		if !firstTS.IsZero() {
			oldest = firstTS
		}
		if !lastTS.IsZero() {
			newest = lastTS
		}

		sessions = append(sessions, timelineSession{
			SessionID:     truncateSessionID(sessionID),
			FullSessionID: sessionID,
			Summary:       summary,
			Project:       filepath.Base(projectDir),
			HasTranscript: true,
			SessionType:   "conversation",
			newestTime:    newest,
			oldestTime:    oldest,
			Duration:      formatSessionDuration(newest.Sub(oldest)),
		})
	}
	return sessions
}

func parseTranscriptTimestamp(raw json.RawMessage) (time.Time, bool) {
	var ts struct {
		Timestamp string `json:"timestamp"`
	}
	if json.Unmarshal(raw, &ts) != nil {
		return time.Time{}, false
	}
	return parseTimestamp(ts.Timestamp)
}

// transcriptHeadRecords bounds the head scan: summary and the opening timestamp
// both live in the first handful of records.
const transcriptHeadRecords = 50

// transcriptTailBytes is the window read from the end of a transcript to find
// its closing timestamp.
const transcriptTailBytes = 64 << 10

var transcriptTimestampRe = regexp.MustCompile(`"timestamp":"([^"]+)"`)

// extractTranscriptMeta extracts summary, first timestamp, and last timestamp
// from a transcript JSONL. Transcripts run to hundreds of megabytes in
// aggregate, so it reads a bounded head and tail rather than the whole file.
func extractTranscriptMeta(path string) (summary string, first, last time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	for i := 0; i < transcriptHeadRecords; i++ {
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			break
		}
		if t, ok := parseTranscriptTimestamp(raw); ok && first.IsZero() {
			first = t
		}
		if summary == "" {
			summary = extractSummaryFromRaw(raw)
		}
	}
	return summary, first, lastTranscriptTimestamp(f)
}

// lastTranscriptTimestamp scans the tail of an open transcript for the newest
// timestamp, matching on the raw text so a truncated leading record is harmless.
func lastTranscriptTimestamp(f *os.File) time.Time {
	info, err := f.Stat()
	if err != nil {
		return time.Time{}
	}
	offset := info.Size() - transcriptTailBytes
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return time.Time{}
	}

	matches := transcriptTimestampRe.FindAllSubmatch(buf, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		if t, ok := parseTimestamp(string(matches[i][1])); ok {
			return t
		}
	}
	return time.Time{}
}

type sessionBuild struct {
	session *timelineSession
	files   map[string]bool
	tools   map[string]bool
}

func appendOrMergeEntry(sb *sessionBuild, evt SessionEvent, baseDir string) {
	sb.files[evt.FilePath] = true
	sb.tools[evt.ToolName] = true
	sb.session.EditCount++

	if sb.session.newestTime.IsZero() || evt.Timestamp.After(sb.session.newestTime) {
		sb.session.newestTime = evt.Timestamp
	}
	if sb.session.oldestTime.IsZero() || evt.Timestamp.Before(sb.session.oldestTime) {
		sb.session.oldestTime = evt.Timestamp
	}

	if n := len(sb.session.Events); n > 0 {
		prev := &sb.session.Events[n-1]
		if prev.AbsPath == evt.FilePath && prev.ToolName == evt.ToolName {
			prev.EditCount++
			prev.oldestTime = evt.Timestamp
			prev.TimeAgo = formatTimeRange(prev.newestTime, evt.Timestamp)
			return
		}
	}

	sb.session.Events = append(sb.session.Events, timelineEntry{
		FilePath:   tildeRelPath(evt.FilePath, baseDir),
		AbsPath:    evt.FilePath,
		ToolName:   evt.ToolName,
		TimeAgo:    formatTimeAgo(evt.Timestamp),
		TimeISO:    evt.Timestamp.Format(time.RFC3339),
		IsViewable: isWhitelistedFile(evt.FilePath),
		EditCount:  1,
		PlanTitle:  evt.PlanTitle,
		newestTime: evt.Timestamp,
		oldestTime: evt.Timestamp,
	})
}

// ribbonClassNames are the four ribbon color groups, indexed by ribbonToolClass.
var ribbonClassNames = [4]string{"write", "edit", "bash", "other"}

// ribbonToolClass maps a tool name to a ribbonClassNames index.
func ribbonToolClass(tool string) int {
	switch tool {
	case "Write":
		return 0
	case "Edit", "MultiEdit", "NotebookEdit":
		return 1
	case "Bash":
		return 2
	default:
		return 3
	}
}

// ribbonMaxCells caps a ribbon's width so long sessions stay legible; events
// beyond this are bucketed proportionally rather than truncated.
const ribbonMaxCells = 48

// buildRibbon turns a session's chronological (aggregated) events into a fixed
// set of activity columns. Cell height is normalized to the busiest column;
// cell color is the dominant tool in that column. Deterministic output.
//
// Bucket indices i*cells/n are surjective onto [0,cells), so every column gets
// at least one event — no empty cells, no clamp needed (b.total <= maxTotal).
func buildRibbon(events []timelineEntry) []ribbonCell {
	n := len(events)
	if n == 0 {
		return nil
	}
	cells := min(n, ribbonMaxCells)
	counts := make([][4]int, cells)
	totals := make([]int, cells)
	for i, e := range events {
		edits := e.EditCount
		if edits < 1 {
			edits = 1
		}
		bi := i * cells / n
		counts[bi][ribbonToolClass(e.ToolName)] += edits
		totals[bi] += edits
	}

	maxTotal := 1
	for _, t := range totals {
		if t > maxTotal {
			maxTotal = t
		}
	}

	out := make([]ribbonCell, cells)
	for i := range out {
		dom, domN := 0, -1
		for cls, c := range counts[i] {
			if c > domN {
				dom, domN = cls, c
			}
		}
		out[i] = ribbonCell{Tool: ribbonClassNames[dom], Height: 16 + totals[i]*84/maxTotal}
	}
	return out
}

func groupEventsBySession(events []SessionEvent, baseDir string) []timelineSession {
	transcriptCache := buildTranscriptCache(events)

	sessionMap := make(map[string]*sessionBuild)
	var sessionOrder []string

	for _, evt := range events {
		sid := evt.SessionID
		if sid == "" {
			sid = "_unknown"
		}
		sb, exists := sessionMap[sid]
		if !exists {
			info := transcriptCache[sid]
			sb = &sessionBuild{
				session: &timelineSession{
					SessionID:     truncateSessionID(sid),
					FullSessionID: sid,
					Summary:       info.summary,
					HasTranscript: info.hasTranscript,
					SessionType:   "edit",
				},
				files: make(map[string]bool),
				tools: make(map[string]bool),
			}
			if sid == "_unknown" {
				sb.session.SessionID = "unknown"
				sb.session.FullSessionID = ""
			}
			sessionMap[sid] = sb
			sessionOrder = append(sessionOrder, sid)
		}
		if sb.session.Project == "" && evt.CWD != "" {
			sb.session.Project = filepath.Base(evt.CWD)
		}
		if sb.session.Source == "" && evt.Src != "" {
			sb.session.Source = evt.Src
		}
		appendOrMergeEntry(sb, evt, baseDir)
	}

	sessions := make([]timelineSession, 0, len(sessionOrder))
	for _, sid := range sessionOrder {
		sb := sessionMap[sid]
		s := sb.session
		s.FileCount = len(sb.files)
		s.Duration = formatSessionDuration(s.newestTime.Sub(s.oldestTime))

		toolNames := make([]string, 0, len(sb.tools))
		for t := range sb.tools {
			toolNames = append(toolNames, t)
		}
		sort.Strings(toolNames)
		s.Tools = toolNames
		s.Ribbon = buildRibbon(s.Events)

		sessions = append(sessions, *s)
	}
	return sessions
}

func assignSessionsToDays(sessions []timelineSession) []timelineDayGroup {
	bucketMap := make(map[string]*timelineDayGroup)

	for i := range sessions {
		label := dayLabel(sessions[i].newestTime)
		if _, exists := bucketMap[label]; !exists {
			bucketMap[label] = &timelineDayGroup{Label: label}
		}
		bucketMap[label].Sessions = append(bucketMap[label].Sessions, sessions[i])
	}

	groups := make([]timelineDayGroup, 0, len(bucketMap))
	for _, g := range bucketMap {
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Sessions[0].newestTime.After(groups[j].Sessions[0].newestTime)
	})
	return groups
}

func markActiveSessions(groups []timelineDayGroup) {
	now := time.Now()
	for i := range groups {
		for j := range groups[i].Sessions {
			s := &groups[i].Sessions[j]
			if now.Sub(s.newestTime) < 5*time.Minute {
				s.IsActive = true
			}
			if s.FullSessionID != "" {
				if hb, ok := globalHeartbeats.get(s.FullSessionID); ok {
					if now.Sub(hb.Timestamp) < 5*time.Minute {
						s.IsActive = true
						s.LastTool = hb.ToolName
						s.LastToolAgo = formatTimeAgo(hb.Timestamp)
						s.LastToolDetail = hb.Detail
					}
				}
			}
		}
	}
}

func buildSessionTimeline(events []SessionEvent, baseDir string, discoverConversations bool) []timelineDayGroup {
	editSessions := groupEventsBySession(events, baseDir)

	sessions := editSessions
	if discoverConversations {
		knownIDs := make(map[string]bool, len(editSessions))
		for _, s := range editSessions {
			if s.FullSessionID != "" {
				knownIDs[s.FullSessionID] = true
			}
		}
		convSessions := discoverTranscriptSessions(baseDir, knownIDs)
		convSessions = mergeSessionsByTime(convSessions, discoverPiSessions(baseDir, knownIDs))
		sessions = mergeSessionsByTime(editSessions, convSessions)
	}

	groups := assignSessionsToDays(sessions)
	markActiveSessions(groups)
	return groups
}

// mergeSessionsByTime interleaves two session slices by newestTime (newest first).
func mergeSessionsByTime(a, b []timelineSession) []timelineSession {
	if len(b) == 0 {
		return a
	}
	merged := make([]timelineSession, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if !a[i].newestTime.Before(b[j].newestTime) {
			merged = append(merged, a[i])
			i++
		} else {
			merged = append(merged, b[j])
			j++
		}
	}
	merged = append(merged, a[i:]...)
	merged = append(merged, b[j:]...)
	return merged
}

func serveTimeline(w http.ResponseWriter, r *http.Request) {
	// Redirect legacy /timeline?session=X to /transcript?session=X
	if sessionID := r.URL.Query().Get("session"); sessionID != "" {
		http.Redirect(w, r, "/transcript?session="+url.QueryEscape(sessionID), http.StatusFound)
		return
	}

	fileMutex.RLock()
	currentBrowseDir := browseDir
	fileMutex.RUnlock()

	var events []SessionEvent
	if globalEventLog != nil {
		events = globalEventLog.eventsForDir(currentBrowseDir)
	}

	groups := buildSessionTimeline(events, currentBrowseDir, true)

	data := timelineTemplateData{
		baseTemplateData: newBaseTemplateData(),
		TreeHTML:         template.HTML(sidebarTreeHTML(r)),
		Title:            "AI Timeline",
		Subtitle:         fmt.Sprintf("Session history for %s", currentBrowseDir),
		BrowsePath:       currentBrowseDir,
		Groups:           groups,
		RepoInfo:         detectRepoInfo(currentBrowseDir),
	}

	renderTemplatePair(w, r, timelineTmpl, timelinePartialTmpl, data)
}

func computeSessionStats(events []SessionEvent) *sessionStats {
	if len(events) == 0 {
		return nil
	}
	files := make(map[string]bool)
	tools := make(map[string]int)
	var earliest, latest time.Time
	for _, evt := range events {
		files[evt.FilePath] = true
		tools[evt.ToolName]++
		if earliest.IsZero() || evt.Timestamp.Before(earliest) {
			earliest = evt.Timestamp
		}
		if evt.Timestamp.After(latest) {
			latest = evt.Timestamp
		}
	}

	durStr := formatSessionDuration(latest.Sub(earliest))

	// Format tool breakdown: "Edit: 14, Write: 3"
	var toolNames []string
	for t := range tools {
		toolNames = append(toolNames, t)
	}
	sort.Strings(toolNames)
	var toolParts []string
	for _, t := range toolNames {
		toolParts = append(toolParts, fmt.Sprintf("%s: %d", t, tools[t]))
	}

	return &sessionStats{
		FileCount: len(files),
		EditCount: len(events),
		Duration:  durStr,
		Tools:     strings.Join(toolParts, ", "),
	}
}

func detectRepoInfo(dir string) *repoInfo {
	d, err := findGitRoot(dir)
	if err != nil {
		return nil
	}

	ri := &repoInfo{Name: filepath.Base(d)}

	// Read current branch from .git/HEAD
	if head, err := os.ReadFile(filepath.Join(d, ".git", "HEAD")); err == nil {
		s := strings.TrimSpace(string(head))
		if strings.HasPrefix(s, "ref: refs/heads/") {
			ri.Branch = strings.TrimPrefix(s, "ref: refs/heads/")
		}
	}

	// Read remote URL from git config
	ri.Remote = parseGitOriginURL(filepath.Join(d, ".git", "config"))

	return ri
}

func parseGitOriginURL(configPath string) string {
	cfg, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(cfg), "\n")
	inOrigin := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == `[remote "origin"]` {
			inOrigin = true
			continue
		}
		if inOrigin && strings.HasPrefix(trimmed, "[") {
			break
		}
		if inOrigin && strings.HasPrefix(trimmed, "url = ") {
			remote := strings.TrimPrefix(trimmed, "url = ")
			remote = strings.TrimSuffix(remote, ".git")
			if strings.HasPrefix(remote, "git@") {
				remote = strings.TrimPrefix(remote, "git@")
				remote = strings.Replace(remote, ":", "/", 1)
			} else if strings.HasPrefix(remote, "https://") {
				remote = strings.TrimPrefix(remote, "https://")
			}
			return remote
		}
	}
	return ""
}
