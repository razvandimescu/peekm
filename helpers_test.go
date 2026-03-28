package main

import (
	"strings"
	"testing"
	"time"
)

func TestTruncateSessionID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"abcdefghijklmnop", "abcdefgh"},
		{"abcdefgh", "abcdefgh"},
		{"short", "short"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := truncateSessionID(tt.input); got != tt.want {
			t.Errorf("truncateSessionID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseTimestamp(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantOK bool
	}{
		{"RFC3339", "2026-03-28T10:00:00Z", true},
		{"RFC3339Nano", "2026-03-28T10:00:00.123456789Z", true},
		{"RFC3339 with offset", "2026-03-28T10:00:00+02:00", true},
		{"empty", "", false},
		{"invalid", "not-a-timestamp", false},
		{"date only", "2026-03-28", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, ok := parseTimestamp(tt.input)
			if ok != tt.wantOK {
				t.Errorf("parseTimestamp(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if ok && ts.IsZero() {
				t.Error("expected non-zero time on success")
			}
		})
	}
}

func TestParseTimestampOrNow(t *testing.T) {
	ts := parseTimestampOrNow("2026-03-28T10:00:00Z")
	if ts.Year() != 2026 {
		t.Errorf("expected 2026, got %d", ts.Year())
	}

	before := time.Now()
	ts = parseTimestampOrNow("invalid")
	after := time.Now()
	if ts.Before(before) || ts.After(after) {
		t.Error("expected parseTimestampOrNow with invalid input to return ~now")
	}
}

func TestCoalesce(t *testing.T) {
	t.Run("overwrites with non-empty", func(t *testing.T) {
		s := "original"
		coalesce(&s, "replacement")
		if s != "replacement" {
			t.Errorf("expected replacement, got %q", s)
		}
	})

	t.Run("keeps original on empty alt", func(t *testing.T) {
		s := "original"
		coalesce(&s, "")
		if s != "original" {
			t.Errorf("expected original, got %q", s)
		}
	})

	t.Run("fills empty target", func(t *testing.T) {
		s := ""
		coalesce(&s, "filled")
		if s != "filled" {
			t.Errorf("expected filled, got %q", s)
		}
	})
}

func TestFormatTimeAgo(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name  string
		input time.Time
		want  string
	}{
		{"just now", now.Add(-30 * time.Second), "just now"},
		{"1m ago", now.Add(-90 * time.Second), "1m ago"},
		{"5m ago", now.Add(-5 * time.Minute), "5m ago"},
		{"1h ago", now.Add(-90 * time.Minute), "1h ago"},
		{"3h ago", now.Add(-3 * time.Hour), "3h ago"},
		{"yesterday", now.Add(-36 * time.Hour), "yesterday"},
		{"old date", time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC), "Jan 15"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatTimeAgo(tt.input); got != tt.want {
				t.Errorf("formatTimeAgo() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatTimeRange(t *testing.T) {
	now := time.Now()
	t.Run("same label collapses", func(t *testing.T) {
		got := formatTimeRange(now.Add(-10*time.Second), now.Add(-20*time.Second))
		if got != "just now" {
			t.Errorf("expected collapsed label, got %q", got)
		}
	})

	t.Run("different labels show range", func(t *testing.T) {
		got := formatTimeRange(now.Add(-2*time.Minute), now.Add(-30*time.Minute))
		if !strings.Contains(got, " - ") {
			t.Errorf("expected range with separator, got %q", got)
		}
	})
}
