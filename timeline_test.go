package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAssignSessionsToDays_SortsByDateDescending(t *testing.T) {
	now := time.Now()
	threeDaysAgo := now.AddDate(0, 0, -3)
	fiveDaysAgo := now.AddDate(0, 0, -5)

	// Feed sessions in wrong order: old, older, today
	sessions := []timelineSession{
		{SessionID: "old", newestTime: threeDaysAgo},
		{SessionID: "older", newestTime: fiveDaysAgo},
		{SessionID: "today", newestTime: now},
	}

	groups := assignSessionsToDays(sessions)

	if len(groups) != 3 {
		t.Fatalf("expected 3 day groups, got %d", len(groups))
	}
	if groups[0].Label != "Today" {
		t.Errorf("first group should be Today, got %q", groups[0].Label)
	}
	// Verify descending order: each group's time should be after the next
	for i := 0; i < len(groups)-1; i++ {
		a := groups[i].Sessions[0].newestTime
		b := groups[i+1].Sessions[0].newestTime
		if a.Before(b) {
			t.Errorf("group %d (%s) is older than group %d (%s)", i, groups[i].Label, i+1, groups[i+1].Label)
		}
	}
}

func TestAssignSessionsToDays_MultipleSessions_SameDay(t *testing.T) {
	// Use midday to avoid crossing midnight when subtracting
	now := time.Now()
	midday := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location())
	sessions := []timelineSession{
		{SessionID: "a", newestTime: midday.Add(-1 * time.Hour)},
		{SessionID: "b", newestTime: midday.Add(-2 * time.Hour)},
	}

	groups := assignSessionsToDays(sessions)

	if len(groups) != 1 {
		t.Fatalf("expected 1 day group, got %d", len(groups))
	}
	if len(groups[0].Sessions) != 2 {
		t.Errorf("expected 2 sessions in group, got %d", len(groups[0].Sessions))
	}
}

func TestDayLabel(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 14, 0, 0, 0, now.Location())
	yesterday := today.AddDate(0, 0, -1)
	lastWeek := today.AddDate(0, 0, -7)

	tests := []struct {
		name  string
		input time.Time
		want  string
	}{
		{"today morning", today.Add(-6 * time.Hour), "Today"},
		{"today now", today, "Today"},
		{"yesterday", yesterday, "Yesterday"},
		{"last week", lastWeek, lastWeek.Format("Jan 2, 2006")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dayLabel(tt.input); got != tt.want {
				t.Errorf("dayLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatSessionDuration(t *testing.T) {
	tests := []struct {
		input time.Duration
		want  string
	}{
		{0, "< 1s"},
		{500 * time.Millisecond, "< 1s"},
		{time.Second, "1s"},
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m"},
		{5*time.Minute + 30*time.Second, "5m"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h"},
		{time.Hour + 30*time.Minute, "1h30m"},
		{2 * time.Hour, "2h"},
		{2*time.Hour + 15*time.Minute, "2h15m"},
	}
	for _, tt := range tests {
		if got := formatSessionDuration(tt.input); got != tt.want {
			t.Errorf("formatSessionDuration(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseGitOriginURL(t *testing.T) {
	writeConfig := func(t *testing.T, content string) string {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "config")
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("ssh url", func(t *testing.T) {
		path := writeConfig(t, `[remote "origin"]
	url = git@github.com:user/repo.git
	fetch = +refs/heads/*:refs/remotes/origin/*
`)
		if got := parseGitOriginURL(path); got != "github.com/user/repo" {
			t.Errorf("got %q, want github.com/user/repo", got)
		}
	})

	t.Run("https url", func(t *testing.T) {
		path := writeConfig(t, `[remote "origin"]
	url = https://github.com/user/repo.git
`)
		if got := parseGitOriginURL(path); got != "github.com/user/repo" {
			t.Errorf("got %q, want github.com/user/repo", got)
		}
	})

	t.Run("no origin", func(t *testing.T) {
		path := writeConfig(t, `[remote "upstream"]
	url = https://github.com/user/repo.git
`)
		if got := parseGitOriginURL(path); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		if got := parseGitOriginURL("/nonexistent/config"); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("multiple remotes", func(t *testing.T) {
		path := writeConfig(t, `[remote "upstream"]
	url = https://github.com/upstream/repo.git
[remote "origin"]
	url = git@github.com:me/myrepo.git
[branch "main"]
	remote = origin
`)
		if got := parseGitOriginURL(path); got != "github.com/me/myrepo" {
			t.Errorf("got %q, want github.com/me/myrepo", got)
		}
	})
}

func TestBuildRibbon(t *testing.T) {
	t.Run("empty yields nil", func(t *testing.T) {
		if got := buildRibbon(nil); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("one cell per event with tool classes and normalized heights", func(t *testing.T) {
		events := []timelineEntry{
			{ToolName: "Write", EditCount: 1},
			{ToolName: "Edit", EditCount: 5},
			{ToolName: "Bash", EditCount: 2},
			{ToolName: "Grep", EditCount: 1},
		}
		got := buildRibbon(events)
		if len(got) != 4 {
			t.Fatalf("got %d cells, want 4", len(got))
		}
		wantTools := []string{"write", "edit", "bash", "other"}
		for i, c := range got {
			if c.Tool != wantTools[i] {
				t.Errorf("cell %d tool = %q, want %q", i, c.Tool, wantTools[i])
			}
			if c.Height < 16 || c.Height > 100 {
				t.Errorf("cell %d height = %d, out of [16,100]", i, c.Height)
			}
		}
		// Busiest column (Edit:5) must be the tallest.
		if got[1].Height != 100 {
			t.Errorf("busiest cell height = %d, want 100", got[1].Height)
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		events := []timelineEntry{
			{ToolName: "Edit", EditCount: 3},
			{ToolName: "Write", EditCount: 3},
			{ToolName: "Bash", EditCount: 1},
		}
		a := buildRibbon(events)
		b := buildRibbon(events)
		if len(a) != len(b) {
			t.Fatalf("non-deterministic length")
		}
		for i := range a {
			if a[i] != b[i] {
				t.Errorf("cell %d differs: %+v vs %+v", i, a[i], b[i])
			}
		}
	})

	t.Run("buckets long sessions to the cap", func(t *testing.T) {
		events := make([]timelineEntry, ribbonMaxCells*3)
		for i := range events {
			events[i] = timelineEntry{ToolName: "Edit", EditCount: 1}
		}
		got := buildRibbon(events)
		if len(got) > ribbonMaxCells {
			t.Errorf("got %d cells, want <= %d", len(got), ribbonMaxCells)
		}
	})
}

// writeTranscript builds a JSONL transcript whose first record carries the
// summary text and whose records span first..first+count minutes.
func writeTranscript(t *testing.T, path, summary string, first time.Time, count int) {
	t.Helper()
	var b strings.Builder
	for i := 0; i < count; i++ {
		ts := first.Add(time.Duration(i) * time.Minute).UTC().Format(time.RFC3339)
		text := "filler"
		if i == 0 {
			text = summary
		}
		fmt.Fprintf(&b, `{"type":"user","timestamp":%q,"message":{"content":%q}}`+"\n", ts, text)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverTranscriptSessions_ScansProjectsBelowBaseDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	baseDir := filepath.Join(home, "projects")
	projectDir := filepath.Join(baseDir, "rinkt_bot_runner")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Claude encodes both '/' and '_' as '-'.
	encoded := strings.ReplaceAll(strings.ReplaceAll(projectDir, "/", "-"), "_", "-")
	sessionsDir := filepath.Join(home, ".claude", "projects", encoded)
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 7, 21, 12, 15, 0, 0, time.UTC)
	writeTranscript(t, filepath.Join(sessionsDir, "addc8743-7126-4ec7-ae29-8531989162e8.jsonl"),
		"investigate the flaky test", start, 3)

	// Browsing the parent must still surface the subdirectory's sessions.
	got := discoverTranscriptSessions(baseDir, nil)
	if len(got) != 1 {
		t.Fatalf("got %d sessions, want 1", len(got))
	}
	if got[0].Project != "rinkt_bot_runner" {
		t.Errorf("Project = %q, want rinkt_bot_runner", got[0].Project)
	}
	if got[0].Summary != "investigate the flaky test" {
		t.Errorf("Summary = %q", got[0].Summary)
	}
	if !got[0].oldestTime.Equal(start) {
		t.Errorf("oldestTime = %v, want %v", got[0].oldestTime, start)
	}

	// Known sessions are skipped so edit-backed entries aren't duplicated.
	known := map[string]bool{"addc8743-7126-4ec7-ae29-8531989162e8": true}
	if got := discoverTranscriptSessions(baseDir, known); len(got) != 0 {
		t.Errorf("got %d sessions for known ID, want 0", len(got))
	}

	// A project outside baseDir stays out.
	if got := discoverTranscriptSessions(filepath.Join(home, "elsewhere"), nil); len(got) != 0 {
		t.Errorf("got %d sessions outside baseDir, want 0", len(got))
	}
}

func TestExtractTranscriptMeta_BoundedHeadAndTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	start := time.Date(2026, 7, 21, 12, 15, 0, 0, time.UTC)
	// More records than the head scan reads: the closing timestamp must still
	// come from the tail rather than from wherever the head stopped.
	count := transcriptHeadRecords + 40
	writeTranscript(t, path, "first prompt", start, count)

	summary, first, last := extractTranscriptMeta(path)
	if summary != "first prompt" {
		t.Errorf("summary = %q", summary)
	}
	if !first.Equal(start) {
		t.Errorf("first = %v, want %v", first, start)
	}
	want := start.Add(time.Duration(count-1) * time.Minute)
	if !last.Equal(want) {
		t.Errorf("last = %v, want %v", last, want)
	}
}
