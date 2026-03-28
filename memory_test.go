package main

import "testing"

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

func TestMemoryFilePriority(t *testing.T) {
	claude := memoryFilePriority("CLAUDE.md")
	memory := memoryFilePriority("MEMORY.md")
	other := memoryFilePriority("feedback.md")

	if claude >= memory {
		t.Errorf("CLAUDE.md priority (%q) should be < MEMORY.md (%q)", claude, memory)
	}
	if memory >= other {
		t.Errorf("MEMORY.md priority (%q) should be < feedback.md (%q)", memory, other)
	}
}
