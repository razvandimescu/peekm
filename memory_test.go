package main

import (
	"testing"
	"time"
)

func TestFormatNumber(t *testing.T) {
	tests := []struct {
		input int
		want  string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1,000"},
		{1234, "1,234"},
		{10000, "10,000"},
		{999999, "999,999"},
		{1000000, "1,000,000"},
		{1234567, "1,234,567"},
	}
	for _, tt := range tests {
		if got := formatNumber(tt.input); got != tt.want {
			t.Errorf("formatNumber(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMemoryFileLess(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.AddDate(0, 0, 10)

	claude := memoryFile{path: "/p/CLAUDE.md", mod: older}
	memory := memoryFile{path: "/p/MEMORY.md", mod: older}
	fresh := memoryFile{path: "/p/z_fresh.md", mod: newer}
	stale := memoryFile{path: "/p/a_stale.md", mod: older}
	tieA := memoryFile{path: "/p/a_tie.md", mod: older}
	tieB := memoryFile{path: "/p/b_tie.md", mod: older}

	tests := []struct {
		name string
		a, b memoryFile
		want bool
	}{
		{"CLAUDE.md pinned before newer file", claude, fresh, true},
		{"CLAUDE.md before MEMORY.md", claude, memory, true},
		{"MEMORY.md pinned before newer file", memory, fresh, true},
		{"newer file before older", fresh, stale, true},
		{"older file not before newer", stale, fresh, false},
		{"equal mtime falls back to name", tieA, tieB, true},
		{"name fallback is asymmetric", tieB, tieA, false},
	}
	for _, tt := range tests {
		if got := memoryFileLess(tt.a, tt.b); got != tt.want {
			t.Errorf("%s: memoryFileLess(%s, %s) = %v, want %v",
				tt.name, tt.a.path, tt.b.path, got, tt.want)
		}
	}
}
