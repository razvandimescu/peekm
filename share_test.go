package main

import "testing"

func TestFirstMarkdownH1(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"plain h1", "# Launch plan\n\nbody", "Launch plan"},
		{"leading prose", "intro\n\n# Real title\n", "Real title"},
		{"strips inline markers", "# `peekm` **v0.2**\n", "peekm v0.2"},
		{"ignores h2", "## Not this\n# This one\n", "This one"},
		{"skips fenced hash", "```\n# not a heading\n```\n# heading\n", "heading"},
		{"no heading", "just text\n", ""},
		{"trailing closing hashes", "# Title ##\n", "Title"},
	}
	for _, tt := range tests {
		if got := firstMarkdownH1([]byte(tt.src)); got != tt.want {
			t.Errorf("firstMarkdownH1(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestSplitHeading(t *testing.T) {
	tests := []struct {
		title    string
		wantHead string
		wantTail string
	}{
		{"peekm v0.2 — launch plan", "peekm v0.2 — ", "launch plan"},
		{"Overview: the plan", "Overview: ", "the plan"},
		{"a - b - c", "a - b - ", "c"},
		{"SingleWord", "SingleWord", ""},
		{"no-separator-here", "no-separator-here", ""},
	}
	for _, tt := range tests {
		head, tail := splitHeading(tt.title)
		if head != tt.wantHead || tail != tt.wantTail {
			t.Errorf("splitHeading(%q) = (%q, %q), want (%q, %q)", tt.title, head, tail, tt.wantHead, tt.wantTail)
		}
	}
}

func TestReadingMinutes(t *testing.T) {
	tests := []struct {
		name  string
		words int
		want  int
	}{
		{"empty", 0, 1},
		{"under a minute", 120, 1},
		{"exactly 200", 200, 1},
		{"401 words", 401, 2},
		{"long doc", 2000, 10},
	}
	for _, tt := range tests {
		src := make([]byte, 0, tt.words*2)
		for i := 0; i < tt.words; i++ {
			src = append(src, 'x', ' ')
		}
		if got := readingMinutes(src); got != tt.want {
			t.Errorf("readingMinutes(%s, %d words) = %d, want %d", tt.name, tt.words, got, tt.want)
		}
	}
}
