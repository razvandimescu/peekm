package main

import (
	"os"
	"strings"
	"testing"
)

func TestEncodePiProjectDir(t *testing.T) {
	got := encodePiProjectDir("/Users/rd/projects/peekm")
	want := "--Users-rd-projects-peekm--"
	if got != want {
		t.Errorf("encodePiProjectDir = %q, want %q", got, want)
	}
}

func TestPiSessionIDFromName(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"2026-08-03T13-31-25-550Z_019fc7d2-71ee-7da3-8497-cf2834825a90.jsonl", "019fc7d2-71ee-7da3-8497-cf2834825a90"},
		{"noseparator.jsonl", ""},
		{"prefix_not-a-uuid.jsonl", ""},
		{"prefix_019fc7d2-71ee-7da3-8497-cf2834825a90.txt", ""},
	}
	for _, tt := range tests {
		if got := piSessionIDFromName(tt.name); got != tt.want {
			t.Errorf("piSessionIDFromName(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// piFixturePath reuses the transcript package's fixture — one pi session
// sample to keep in sync with the format.
const piFixturePath = "transcript/testdata/pi-session.jsonl"

func renderedTranscriptText(turns []transcriptTurn) string {
	var all strings.Builder
	for _, turn := range turns {
		for _, b := range turn.Blocks {
			all.WriteString(string(b.HTML))
			all.WriteString(b.Text)
			all.WriteString(b.ToolSummary)
		}
	}
	return all.String()
}

func findToolUseBlock(turns []transcriptTurn, toolName string) *contentBlock {
	for i := range turns {
		for j := range turns[i].Blocks {
			if turns[i].Blocks[j].Type == "tool_use" && turns[i].Blocks[j].ToolName == toolName {
				return &turns[i].Blocks[j]
			}
		}
	}
	return nil
}

func firstAssistantModel(turns []transcriptTurn) string {
	for _, turn := range turns {
		if turn.Role == "assistant" && turn.Model != "" {
			return turn.Model
		}
	}
	return ""
}

func TestParsePiTranscript(t *testing.T) {
	turns, err := parseTranscript(piFixturePath) // via dispatch, exercises sniffing too
	if err != nil {
		t.Fatal(err)
	}

	text := renderedTranscriptText(turns)
	for _, want := range []string{"fix the bug", "On it.", "Done.", "go test ./..."} {
		if !strings.Contains(text, want) {
			t.Errorf("transcript missing %q", want)
		}
	}
	if strings.Contains(text, "abandoned branch reply") {
		t.Error("abandoned branch entry leaked into active-branch transcript")
	}

	// edit toolCall renders as an Edit tool_use with the toolResult paired in
	editBlock := findToolUseBlock(turns, "Edit")
	if editBlock == nil {
		t.Fatal("Edit tool_use block missing")
	}
	if editBlock.Result == nil {
		t.Error("toolResult not paired with its toolCall")
	}

	if model := firstAssistantModel(turns); model != "gpt-5.6-sol" {
		t.Errorf("model = %q, want gpt-5.6-sol", model)
	}
}

func TestExtractSessionSummaryPi(t *testing.T) {
	if got := extractSessionSummary(piFixturePath); got != "fix the bug" {
		t.Errorf("summary = %q, want %q", got, "fix the bug")
	}
}

func TestInstallPiExtension(t *testing.T) {
	agentDir := t.TempDir()

	created, err := installPiExtension(agentDir, 7777)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("first install should report created")
	}
	content, err := os.ReadFile(piExtensionPath(agentDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "[7777, 6419, 8080, 3000]") {
		t.Error("configured port not baked into extension")
	}

	// unchanged content → no-op, not "created"
	created, err = installPiExtension(agentDir, 7777)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("re-install of identical content should not report created")
	}

	// port change rewrites but is still not a first install
	created, err = installPiExtension(agentDir, 8888)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("rewrite should not report created")
	}
	content, _ = os.ReadFile(piExtensionPath(agentDir))
	if !strings.Contains(string(content), "[8888, 6419, 8080, 3000]") {
		t.Error("extension not rewritten with new port")
	}
}
