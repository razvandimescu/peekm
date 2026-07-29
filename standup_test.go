package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestActiveTime(t *testing.T) {
	base := mustTime("2026-07-20T10:00:00Z")
	tests := []struct {
		name string
		gaps []time.Duration // gaps between consecutive stamps
		want time.Duration
	}{
		{"single stamp", nil, 0},
		{"gap under threshold", []time.Duration{4 * time.Minute}, 4 * time.Minute},
		{"gap exactly at threshold", []time.Duration{5 * time.Minute}, 5 * time.Minute},
		{"gap over threshold dropped", []time.Duration{6 * time.Minute}, 0},
		{"mixed", []time.Duration{2 * time.Minute, 30 * time.Minute, 3 * time.Minute}, 5 * time.Minute},
	}
	for _, tt := range tests {
		stamps := []time.Time{base}
		cur := base
		for _, g := range tt.gaps {
			cur = cur.Add(g)
			stamps = append(stamps, cur)
		}
		if got := activeTime(stamps); got != tt.want {
			t.Errorf("%s: activeTime = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestActiveTimeUnordered(t *testing.T) {
	base := mustTime("2026-07-20T10:00:00Z")
	// Shuffled: sorted order is base, +4m, +40m → a 4m gap (counted) then a 36m
	// gap (dropped). Only correct if activeTime sorts before summing.
	stamps := []time.Time{base.Add(4 * time.Minute), base, base.Add(40 * time.Minute)}
	if got := activeTime(stamps); got != 4*time.Minute {
		t.Errorf("activeTime(unordered) = %v, want 4m", got)
	}
}

func TestRankProjectsInversion(t *testing.T) {
	// The exact inversion the dry run found: a low-time, high-commit project
	// must outrank a high-time, zero-commit one.
	projects := []standupProject{
		{Name: "high-time", Edits: 60, active: 120 * time.Minute},
		{Name: "high-commit", Commits: []standupCommit{{Short: "abc", Subject: "x"}}, Edits: 2, active: time.Minute},
	}
	rankProjects(projects)
	if projects[0].Name != "high-commit" {
		t.Errorf("ranking = %s first, want high-commit (commits outrank time)", projects[0].Name)
	}
}

func TestRankProjectsTiebreak(t *testing.T) {
	projects := []standupProject{
		{Name: "few-edits", Edits: 5, active: 10 * time.Minute},
		{Name: "many-edits", Edits: 50, active: time.Minute},
	}
	rankProjects(projects)
	if projects[0].Name != "many-edits" {
		t.Errorf("with no commits, edits should break the tie; got %s first", projects[0].Name)
	}
}

func TestSplitHeadlineTail(t *testing.T) {
	// 6 commit-bearing projects: cap at 5 headline, 1 overflows to tail.
	var projects []standupProject
	for i := 0; i < 6; i++ {
		projects = append(projects, standupProject{Commits: []standupCommit{{Subject: "c"}}})
	}
	// plus a discussion-only (no commits, few edits) → tail
	projects = append(projects, standupProject{Name: "chatty", Edits: 2})
	var sd standupDay
	splitHeadlineTail(&sd, projects)
	if len(sd.Headline) != standupHeadlineCap {
		t.Errorf("headline = %d, want cap %d", len(sd.Headline), standupHeadlineCap)
	}
	if len(sd.Tail) != 2 {
		t.Errorf("tail = %d, want 2 (overflow + discussion)", len(sd.Tail))
	}
}

func TestSplitHeadlineTailEditThreshold(t *testing.T) {
	projects := []standupProject{
		{Name: "heavy", Edits: standupTailEditThreshold},     // qualifies (>=10)
		{Name: "light", Edits: standupTailEditThreshold - 1}, // folds to tail
	}
	var sd standupDay
	splitHeadlineTail(&sd, projects)
	if len(sd.Headline) != 1 || sd.Headline[0].Name != "heavy" {
		t.Errorf("headline should hold only 'heavy'; got %+v", sd.Headline)
	}
	if len(sd.Tail) != 1 || sd.Tail[0].Name != "light" {
		t.Errorf("tail should hold only 'light'; got %+v", sd.Tail)
	}
}

func TestFillEvidenceLadder(t *testing.T) {
	// Rung 1: commits win.
	p := standupProject{Commits: []standupCommit{{Subject: "a"}, {Subject: "b"}, {Subject: "c"}}}
	fillEvidence(&p)
	if len(p.HeadCommits) != 2 || len(p.MoreCommits) != 1 {
		t.Errorf("commits rung: head=%d more=%d, want 2/1", len(p.HeadCommits), len(p.MoreCommits))
	}

	// Rung 2: no commits, edits → first FileRows basenames (already most-edited
	// first) + MoreFiles.
	p2 := standupProject{Edits: 5, FileRows: []standupFile{
		{Base: "y.go"}, {Base: "z.go"}, {Base: "w.go"}, {Base: "x.go"},
	}}
	fillEvidence(&p2)
	if len(p2.EvidenceFiles) != 3 || p2.MoreFiles != 1 {
		t.Errorf("edits rung: files=%v more=%d, want 3 files +1", p2.EvidenceFiles, p2.MoreFiles)
	}
	if p2.EvidenceFiles[0] != "y.go" || p2.EvidenceFiles[1] != "z.go" || p2.EvidenceFiles[2] != "w.go" {
		t.Errorf("evidence files = %v, want FileRows order [y.go z.go w.go]", p2.EvidenceFiles)
	}
	if len(p2.HeadCommits) != 0 {
		t.Error("edits rung must not populate commits")
	}

	// Rung 3: no commits, no edits → neither commits nor files (prompt used as-is).
	p3 := standupProject{Prompt: "let's investigate"}
	fillEvidence(&p3)
	if len(p3.HeadCommits) != 0 || len(p3.EvidenceFiles) != 0 {
		t.Error("discussion rung must render nothing but the prompt")
	}
}

func TestTailSummaryNeverZero(t *testing.T) {
	tests := []struct {
		p    standupProject
		want string
	}{
		{standupProject{Name: "a", ActiveStr: "0.7h", Commits: []standupCommit{{}, {}}}, "a 0.7h · 2 commits"},
		{standupProject{Name: "b", ActiveStr: "0.4h", Commits: []standupCommit{{}}, DepBumps: 1}, "b 0.4h · 1 commit, 1 dep bump"},
		{standupProject{Name: "c", ActiveStr: "1.3h", Edits: 2}, "c 1.3h · 2 edits"},
		{standupProject{Name: "d", ActiveStr: "1.2h"}, "d 1.2h · discussion"},
	}
	for _, tt := range tests {
		if got := tailSummary(&tt.p); got != tt.want {
			t.Errorf("tailSummary = %q, want %q", got, tt.want)
		}
	}
}

func TestHeadlineMetricsNoZeros(t *testing.T) {
	p := standupProject{ActiveStr: "5.5h", Edits: 60, Files: 27}
	got := strings.Join(headlineMetrics(&p), " · ")
	if got != "5.5h active · 60 edits · 27 files" {
		t.Errorf("metrics = %q", got)
	}
	// Zero edits/files must not appear.
	p2 := standupProject{ActiveStr: "0.3h"}
	if got := strings.Join(headlineMetrics(&p2), " · "); got != "0.3h active" {
		t.Errorf("metrics with zeros = %q, want just active", got)
	}
}

func TestStandupTargetDay(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	yesterday := today.AddDate(0, 0, -1)

	if got := standupTargetDay(""); !got.Equal(yesterday) {
		t.Errorf("empty → %v, want yesterday %v", got, yesterday)
	}
	if got := standupTargetDay("2020-01-15"); got.Format("2006-01-02") != "2020-01-15" {
		t.Errorf("explicit date → %v", got)
	}
	// Future dates clamp to today.
	future := today.AddDate(0, 0, 5).Format("2006-01-02")
	if got := standupTargetDay(future); !got.Equal(today) {
		t.Errorf("future → %v, want today %v", got, today)
	}
}

func TestStandupMarkdownRoundTrips(t *testing.T) {
	sd := standupDay{
		Label: "Monday, 20 July 2026", TotalActive: "16.2h", TotalCommits: 22, ProjectCount: 8,
		Headline: []standupProject{{
			Name: "peekm", Branch: "main", Metrics: []string{"5.5h active", "60 edits"},
			Insertions: 951, Deletions: 258,
			HeadCommits: []standupCommit{{Subject: "fix: a thing"}},
			MoreCommits: []standupCommit{{Subject: "y"}},
			Sessions:    []standupSession{{}, {}}, SessionRange: "14:21–22:27",
		}},
		Tail: []standupProject{{TailSummary: "gopdf 0.7h · 2 commits"}},
	}
	var b strings.Builder
	if err := standupMarkdownTmpl.Execute(&b, sd); err != nil {
		t.Fatalf("markdown execute: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		"# Standup — Monday, 20 July 2026",
		"16.2h active · 22 commits · 8 projects",
		"## peekm `main` — 5.5h active · 60 edits · +951 −258",
		"- fix: a thing",
		"- …1 more",
		"_2 sessions · 14:21–22:27_",
		"### Also touched",
		"- gopdf 0.7h · 2 commits",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, out)
		}
	}
}

func TestSliceTranscriptDayMidnight(t *testing.T) {
	// A transcript spanning midnight must contribute only its same-day records.
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	loc := time.Now().Location()
	day := time.Date(2026, 7, 20, 0, 0, 0, 0, loc)
	lines := []string{
		recordLine(day.Add(23*time.Hour), "user", `"work on it"`),                                // in-day
		recordLine(day.Add(23*time.Hour+2*time.Minute), "assistant", toolUse("Edit", "/a/x.go")), // in-day
		recordLine(day.Add(25*time.Hour), "user", `"next day"`),                                  // next day, excluded
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ds := sliceTranscriptDay(path, "2026-07-20")
	if ds == nil {
		t.Fatal("expected a slice for 2026-07-20")
	}
	if len(ds.stamps) != 2 {
		t.Errorf("stamps = %d, want 2 (next-day record excluded)", len(ds.stamps))
	}
	if ds.edits != 1 {
		t.Errorf("edits = %d, want 1", ds.edits)
	}
	if fs := ds.files["/a/x.go"]; fs == nil || fs.edits != 1 || fs.tools["Edit"] != 1 {
		t.Errorf("files[/a/x.go] = %+v, want 1 Edit", ds.files["/a/x.go"])
	}
	if len(ds.prompts) != 1 || ds.prompts[0] != "work on it" {
		t.Errorf("prompts = %v, want [work on it]", ds.prompts)
	}
}

func recordLine(ts time.Time, typ, content string) string {
	return `{"type":"` + typ + `","timestamp":"` + ts.Format(time.RFC3339) +
		`","message":{"content":` + content + `}}`
}

func usageLine(ts time.Time, id string, out, cacheWrite, cacheRead int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":"%s","message":{"id":"%s","usage":{"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d},"content":[{"type":"text","text":"x"}]}}`,
		ts.Format(time.RFC3339), id, out, cacheWrite, cacheRead)
}

func TestSliceTranscriptDayTokenDedup(t *testing.T) {
	// Claude Code writes one record per content block, each repeating the same
	// usage — a message ID seen five times must be counted exactly once.
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	day := time.Date(2026, 7, 20, 10, 0, 0, 0, time.Now().Location())
	var lines []string
	for i := 0; i < 5; i++ {
		lines = append(lines, usageLine(day.Add(time.Duration(i)*time.Minute), "msg_a", 464, 43269, 100))
	}
	lines = append(lines,
		usageLine(day.Add(10*time.Minute), "msg_b", 100, 0, 7),
		recordLine(day.Add(11*time.Minute), "user", `"no usage on user records"`),
	)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ds := sliceTranscriptDay(path, "2026-07-20")
	if ds == nil {
		t.Fatal("expected a slice for 2026-07-20")
	}
	if ds.outTokens != 564 {
		t.Errorf("outTokens = %d, want 564 (464 counted once + 100)", ds.outTokens)
	}
	if ds.cacheWrite != 43269 {
		t.Errorf("cacheWrite = %d, want 43269", ds.cacheWrite)
	}
	if ds.cacheRead != 107 {
		t.Errorf("cacheRead = %d, want 107", ds.cacheRead)
	}
}

func TestFormatCompact(t *testing.T) {
	cases := map[int]string{
		0:        "0",
		840:      "840",
		999:      "999",
		1000:     "1k",
		4200:     "4.2k",
		9999:     "10k",
		98000:    "98k",
		295400:   "295k",
		999999:   "999k",
		1000000:  "1M",
		1200000:  "1.2M",
		13546517: "13.5M",
	}
	for n, want := range cases {
		if got := formatCompact(n); got != want {
			t.Errorf("formatCompact(%d) = %q, want %q", n, got, want)
		}
	}
}

func toolUse(name, filePath string) string {
	return `[{"type":"tool_use","name":"` + name + `","input":{"file_path":"` + filePath + `"}}]`
}

func TestComputeGitCommitsAuthorAndDepBumps(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	day := "2026-07-20"
	date := "2026-07-20T12:00:00"

	gitRun := func(env []string, args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	commit := func(msg string) {
		env := []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
		os.WriteFile(filepath.Join(dir, "f"), []byte(msg), 0644)
		gitRun(env, "add", "-A")
		gitRun(env, "commit", "-m", msg)
	}

	gitRun(nil, "init")
	gitRun(nil, "config", "user.email", "me@example.com")
	gitRun(nil, "config", "user.name", "Me")
	commit("feat: a real change")
	commit("chore(deps): bump goldmark")

	cr := computeGitCommits(dir, day)
	if len(cr.commits) != 1 {
		t.Errorf("real commits = %d, want 1 (dep bump collapsed)", len(cr.commits))
	}
	if cr.depBumps != 1 {
		t.Errorf("depBumps = %d, want 1", cr.depBumps)
	}
	if cr.insertions == 0 {
		t.Error("expected non-zero insertions from --shortstat")
	}

	// Author filter is absolute: a mismatched identity yields no commits.
	gitRun(nil, "config", "user.email", "nobody@example.com")
	other := computeGitCommits(dir, day)
	if len(other.commits) != 0 {
		t.Errorf("mismatched author should yield 0 commits, got %d", len(other.commits))
	}
}

func TestBuildFileRowsRanking(t *testing.T) {
	a := &projectAccum{root: "/repo", files: map[string]*fileStat{
		"/repo/theme/app.js": {edits: 7, tools: map[string]int{"Edit": 7}, last: time.Date(2026, 7, 20, 16, 38, 0, 0, time.Local), toolID: "tu_2", sessionID: "s2"},
		"/repo/main.go":      {edits: 12, tools: map[string]int{"Edit": 9, "Write": 3}, last: time.Date(2026, 7, 20, 16, 41, 0, 0, time.Local), toolID: "tu_1", sessionID: "s1"},
		"/repo/new.go":       {edits: 4, tools: map[string]int{"Write": 4}, last: time.Date(2026, 7, 20, 14, 57, 0, 0, time.Local), toolID: "tu_3", sessionID: "s1"},
	}}
	rows := buildFileRows(a, "2026-07-20") // past day: no dirty lookup
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if rows[0].Base != "main.go" || rows[1].Base != "app.js" || rows[2].Base != "new.go" {
		t.Errorf("order = %s, %s, %s; want most-edited first", rows[0].Base, rows[1].Base, rows[2].Base)
	}
	if rows[0].Tools != "Edit, Write" || rows[0].Dot != "edit" {
		t.Errorf("main.go tools=%q dot=%q, want dominant Edit first", rows[0].Tools, rows[0].Dot)
	}
	if rows[2].Dot != "write" {
		t.Errorf("new.go dot=%q, want write", rows[2].Dot)
	}
	if rows[1].Dir != "theme/" || rows[1].Base != "app.js" {
		t.Errorf("app.js dir=%q base=%q, want theme/ split", rows[1].Dir, rows[1].Base)
	}
	if rows[0].SessionID != "s1" || rows[0].ToolID != "tu_1" {
		t.Errorf("main.go anchor = %s/%s, want s1/tu_1", rows[0].SessionID, rows[0].ToolID)
	}
}

func TestBuildFileRowsViewPathBrowseRelative(t *testing.T) {
	origFiles, origDir := markdownFiles, browseDir
	markdownFiles, browseDir = []string{"/repo/docs/notes.md"}, "/repo"
	defer func() { markdownFiles, browseDir = origFiles, origDir }()

	a := &projectAccum{root: "/repo", files: map[string]*fileStat{
		"/repo/docs/notes.md": {edits: 2, tools: map[string]int{"Edit": 2}},
		"/repo/main.go":       {edits: 1, tools: map[string]int{"Edit": 1}},
	}}
	rows := buildFileRows(a, "2026-07-20")
	for _, row := range rows {
		switch row.Base {
		case "notes.md":
			if row.ViewPath != "docs/notes.md" {
				t.Errorf("notes.md ViewPath = %q, want browse-relative docs/notes.md", row.ViewPath)
			}
		case "main.go":
			if row.ViewPath != "" {
				t.Errorf("main.go ViewPath = %q, want empty (not whitelisted)", row.ViewPath)
			}
		}
	}
}

func TestDirtyFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "committed.go"), []byte("a"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "dirty.go"), []byte("b"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "committed.go"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	// The outside-repo path would fail the whole git call if not skipped.
	dirty := dirtyFiles(dir, []string{"sub/dirty.go", "committed.go", "/outside/repo.md"})
	if !dirty["sub/dirty.go"] || !dirty["committed.go"] {
		t.Errorf("dirty = %v, want sub/dirty.go and committed.go", dirty)
	}
	if len(dirtyFiles(dir, []string{"/outside/repo.md"})) != 0 {
		t.Error("all-absolute rels must yield no dirty files, not a git error")
	}
}
