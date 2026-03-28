package main

import (
	"os"
	"path/filepath"
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
