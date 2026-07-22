package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsSystemNoise(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"<local-command-caveat>some caveat</local-command-caveat>", true},
		{"<command-name>/clear</command-name>", true},
		{"<local-command-stdout>output</local-command-stdout>", true},
		{"  [Request interrupted by user", true},
		{"[Request interrupted", true},
		{"hello world", false},
		{"", false},
		{"some text with [Request interrupted in the middle", false},
	}
	for _, tt := range tests {
		if got := isSystemNoise(tt.input); got != tt.want {
			t.Errorf("isSystemNoise(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestExtractPlanTitle(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Implement the following plan\n\n# Plan: Add caching layer\n\nDetails...", "Add caching layer"},
		{"Implement the following plan\n\n# Refactor auth\n\nDetails...", "Refactor auth"},
		{"no heading here", ""},
		{"", ""},
		{"# Just a heading", "Just a heading"},
		{"some preamble\n# Plan: Nested heading", "Nested heading"},
	}
	for _, tt := range tests {
		if got := extractPlanTitle(tt.input); got != tt.want {
			t.Errorf("extractPlanTitle(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestExtractUserText(t *testing.T) {
	tests := []struct {
		name  string
		input json.RawMessage
		want  string
	}{
		{"string content", json.RawMessage(`"hello world"`), "hello world"},
		{"array with text block", json.RawMessage(`[{"type":"text","text":"from array"}]`), "from array"},
		{"array skips non-text", json.RawMessage(`[{"type":"image","text":""},{"type":"text","text":"found"}]`), "found"},
		{"empty string", json.RawMessage(`""`), ""},
		{"empty array", json.RawMessage(`[]`), ""},
		{"null", json.RawMessage(`null`), ""},
		{"invalid json", json.RawMessage(`{{{`), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractUserText(tt.input); got != tt.want {
				t.Errorf("extractUserText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHumanizeToolName(t *testing.T) {
	tests := []struct {
		input      string
		wantName   string
		wantServer string
	}{
		{"mcp__slack__send_message", "send_message", "slack"},
		{"mcp__github__create_pr", "create_pr", "github"},
		{"mcp__server", "mcp__server", ""}, // only one part after mcp__
		{"Bash", "Bash", ""},
		{"Read", "Read", ""},
		{"", "", ""},
		{"mcp__a__b__c", "b__c", "a"},
	}
	for _, tt := range tests {
		name, server := humanizeToolName(tt.input)
		if name != tt.wantName || server != tt.wantServer {
			t.Errorf("humanizeToolName(%q) = (%q, %q), want (%q, %q)",
				tt.input, name, server, tt.wantName, tt.wantServer)
		}
	}
}

func TestToolIcon(t *testing.T) {
	known := []string{"Bash", "Read", "Edit", "Write", "Glob", "Grep", "WebFetch", "WebSearch",
		"TaskCreate", "TaskUpdate", "TaskList", "TaskGet", "NotebookEdit", "Agent"}
	unknownIcon := toolIcon("UnknownTool")

	for _, name := range known {
		icon := toolIcon(name)
		if icon == unknownIcon {
			t.Errorf("toolIcon(%q) returned the default icon, expected a specific one", name)
		}
		if icon == "" {
			t.Errorf("toolIcon(%q) returned empty string", name)
		}
	}
}

func TestMarkCollapsible(t *testing.T) {
	long := strings.Repeat("x", 1600)
	veryLong := strings.Repeat("x", 2600)
	tall := strings.Repeat("line\n", 35)
	tests := []struct {
		name      string
		text      string
		assistant bool
		want      bool
	}{
		{"user over 1500", long, false, true},
		{"user under 1500", "short", false, false},
		{"assistant 1600 chars stays open", long, true, false},
		{"assistant over 2500 chars", veryLong, true, true},
		{"assistant over 30 lines", tall, true, true},
		{"assistant short", "short", true, false},
	}
	for _, tt := range tests {
		var b contentBlock
		markCollapsible(&b, tt.text, tt.assistant)
		if b.Collapsible != tt.want {
			t.Errorf("%s: Collapsible = %v, want %v", tt.name, b.Collapsible, tt.want)
		}
	}
}

func TestExpandFinalTurn(t *testing.T) {
	collapsed := func(role string) transcriptTurn {
		return transcriptTurn{Role: role, Collapsible: true,
			Blocks: []contentBlock{{Type: "text", Collapsible: true}}}
	}

	t.Run("ends on assistant: only last turn expands", func(t *testing.T) {
		turns := expandFinalTurn([]transcriptTurn{collapsed("user"), collapsed("assistant"), collapsed("assistant")})
		if !turns[0].Collapsible || !turns[1].Collapsible {
			t.Error("earlier turns should keep Collapsible")
		}
		if turns[2].Collapsible || turns[2].Blocks[0].Collapsible {
			t.Error("final turn should be fully expanded")
		}
	})

	t.Run("ends on user: preceding assistant turn expands too", func(t *testing.T) {
		turns := expandFinalTurn([]transcriptTurn{collapsed("assistant"), collapsed("assistant"), collapsed("user")})
		if !turns[0].Collapsible {
			t.Error("earlier assistant turn should keep Collapsible")
		}
		if turns[1].Collapsible || turns[1].Blocks[0].Collapsible {
			t.Error("last assistant turn before trailing user message should expand")
		}
		if turns[2].Collapsible {
			t.Error("final user turn should expand")
		}
	})

	if out := expandFinalTurn(nil); out != nil {
		t.Error("nil turns should pass through")
	}
}

func TestMarkTurnCollapsible(t *testing.T) {
	text := func(chars, lines int) contentBlock {
		return contentBlock{Type: "text", textChars: chars, textLines: lines}
	}
	tool := contentBlock{Type: "tool_use"}
	tests := []struct {
		name string
		turn transcriptTurn
		want bool
	}{
		{"assistant many short blocks accumulate chars", transcriptTurn{Role: "assistant",
			Blocks: []contentBlock{text(1000, 5), text(1000, 5), text(1000, 5)}}, true},
		{"assistant tool rows push over line budget", transcriptTurn{Role: "assistant",
			Blocks: []contentBlock{text(200, 2), tool, tool, tool, tool, tool, tool, tool, tool, tool, tool, tool, tool, tool, tool, tool}}, true},
		{"assistant modest turn stays open", transcriptTurn{Role: "assistant",
			Blocks: []contentBlock{text(800, 8), tool, tool}}, false},
		{"user pasted log collapses", transcriptTurn{Role: "user",
			Blocks: []contentBlock{text(1600, 10)}}, true},
		{"user short stays open", transcriptTurn{Role: "user",
			Blocks: []contentBlock{text(300, 3)}}, false},
	}
	for _, tt := range tests {
		out := markTurnCollapsible([]transcriptTurn{tt.turn})
		if out[0].Collapsible != tt.want {
			t.Errorf("%s: Collapsible = %v, want %v", tt.name, out[0].Collapsible, tt.want)
		}
	}
}

func TestDiffOps(t *testing.T) {
	toStr := func(ops []diffOp) string {
		var b []byte
		for _, op := range ops {
			b = append(b, op.kind, ' ')
			b = append(b, op.text...)
			b = append(b, '\n')
		}
		return string(b)
	}
	tests := []struct {
		name string
		a, b []string
		want string
	}{
		{
			name: "shared context kept, single line changed",
			a:    []string{"func f() {", "\treturn 1", "}"},
			b:    []string{"func f() {", "\treturn 2", "}"},
			want: "  func f() {\n- \treturn 1\n+ \treturn 2\n  }\n",
		},
		{
			name: "pure addition (write)",
			a:    nil,
			b:    []string{"line a", "line b"},
			want: "+ line a\n+ line b\n",
		},
		{
			name: "pure removal",
			a:    []string{"gone"},
			b:    nil,
			want: "- gone\n",
		},
		{
			name: "insertion between context",
			a:    []string{"a", "c"},
			b:    []string{"a", "b", "c"},
			want: "  a\n+ b\n  c\n",
		},
	}
	for _, tt := range tests {
		if got := toStr(diffOps(tt.a, tt.b)); got != tt.want {
			t.Errorf("%s: diffOps() =\n%q\nwant\n%q", tt.name, got, tt.want)
		}
	}
}

func TestLineDiffHTMLCap(t *testing.T) {
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, "x")
	}
	html := lineDiffHTML("", strings.Join(lines, "\n"), 60)
	if !strings.Contains(html, "40 more lines") {
		t.Errorf("expected overflow note '40 more lines', got: %s", html)
	}
}

func TestTruncateString(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 5, "hello..."},
		{"", 5, ""},
		{"abc", 0, "..."},
		// Multi-byte: "héllo" has 5 runes
		{"héllo", 5, "héllo"},
		{"héllo world", 5, "héllo..."},
		// CJK characters (3 bytes each, 1 rune each)
		{"你好世界test", 4, "你好世界..."},
	}
	for _, tt := range tests {
		if got := truncateString(tt.input, tt.maxLen); got != tt.want {
			t.Errorf("truncateString(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
		}
	}
}

func TestPairToolResults(t *testing.T) {
	t.Run("pairs matching tool_use and tool_result", func(t *testing.T) {
		turns := []transcriptTurn{
			{Role: "assistant", Blocks: []contentBlock{
				{Type: "tool_use", ToolID: "t1", ToolName: "Bash"},
			}},
			{Role: "user", Blocks: []contentBlock{
				{Type: "tool_result", ToolID: "t1", Text: "output"},
			}},
		}
		result := pairToolResults(turns)
		if result[0].Blocks[0].Result == nil {
			t.Error("tool_use should have paired result")
		}
		if len(result[1].Blocks) != 0 {
			t.Errorf("tool_result should be removed from user turn, got %d blocks", len(result[1].Blocks))
		}
	})

	t.Run("unmatched tool_result stays", func(t *testing.T) {
		turns := []transcriptTurn{
			{Role: "user", Blocks: []contentBlock{
				{Type: "tool_result", ToolID: "orphan", Text: "no match"},
			}},
		}
		result := pairToolResults(turns)
		if len(result[0].Blocks) != 1 {
			t.Error("unmatched tool_result should remain")
		}
	})

	t.Run("empty turns", func(t *testing.T) {
		result := pairToolResults(nil)
		if len(result) != 0 {
			t.Errorf("expected empty, got %d turns", len(result))
		}
	})
}

func TestMergeConsecutiveTurns(t *testing.T) {
	t.Run("merges same role", func(t *testing.T) {
		turns := []transcriptTurn{
			{Role: "user", Blocks: []contentBlock{{Type: "text", Text: "a"}}},
			{Role: "user", Blocks: []contentBlock{{Type: "text", Text: "b"}}},
			{Role: "assistant", Blocks: []contentBlock{{Type: "text", Text: "c"}}},
		}
		result := mergeConsecutiveTurns(turns)
		if len(result) != 2 {
			t.Fatalf("expected 2 turns, got %d", len(result))
		}
		if len(result[0].Blocks) != 2 {
			t.Errorf("first turn should have 2 blocks, got %d", len(result[0].Blocks))
		}
	})

	t.Run("no merge needed", func(t *testing.T) {
		turns := []transcriptTurn{
			{Role: "user", Blocks: []contentBlock{{Type: "text"}}},
			{Role: "assistant", Blocks: []contentBlock{{Type: "text"}}},
		}
		result := mergeConsecutiveTurns(turns)
		if len(result) != 2 {
			t.Fatalf("expected 2 turns, got %d", len(result))
		}
	})

	t.Run("empty input", func(t *testing.T) {
		result := mergeConsecutiveTurns(nil)
		if len(result) != 0 {
			t.Errorf("expected empty, got %d", len(result))
		}
	})
}

func TestRemoveEmptyTurns(t *testing.T) {
	turns := []transcriptTurn{
		{Role: "user", Blocks: []contentBlock{{Type: "text"}}},
		{Role: "assistant", Blocks: nil},
		{Role: "user", Blocks: []contentBlock{}},
		{Role: "assistant", Blocks: []contentBlock{{Type: "text"}}},
	}
	result := removeEmptyTurns(turns)
	if len(result) != 2 {
		t.Fatalf("expected 2 non-empty turns, got %d", len(result))
	}
	if result[0].Role != "user" || result[1].Role != "assistant" {
		t.Errorf("unexpected roles: %s, %s", result[0].Role, result[1].Role)
	}
}
