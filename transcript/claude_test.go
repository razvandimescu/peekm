package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const claudeFixture = `{"type":"summary","summary":"ignored"}
{"type":"user","timestamp":"2026-08-03T10:00:00.000Z","message":{"role":"user","content":"hello"}}
{"type":"user","isMeta":true,"message":{"role":"user","content":"meta noise"}}
{"type":"assistant","timestamp":"2026-08-03T10:00:05.000Z","message":{"role":"assistant","model":"claude-fable-5","content":[{"type":"thinking","thinking":"pondering"},{"type":"text","text":"Hi there."},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"main.go"}}]}}
{"type":"user","timestamp":"2026-08-03T10:00:06.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"file contents"}]}}
{"type":"assistant","timestamp":"2026-08-03T10:00:10.000Z","message":{"role":"assistant","model":"claude-fable-5","content":[{"type":"text","text":"Done."}]}}
`

func writeClaudeFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "0199aaaa-bbbb-cccc-dddd-eeeeffff0000.jsonl")
	if err := os.WriteFile(path, []byte(claudeFixture), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func findToolCall(sess *Session, name string) *ToolCall {
	for _, turn := range sess.Turns {
		for _, b := range turn.Blocks {
			if b.Kind == KindToolCall && b.Tool.Name == name {
				return b.Tool
			}
		}
	}
	return nil
}

func TestParseClaudeFile(t *testing.T) {
	sess, err := ParseFile(writeClaudeFixture(t))
	if err != nil {
		t.Fatal(err)
	}

	if sess.Harness != HarnessClaudeCode {
		t.Errorf("harness = %q, want %q", sess.Harness, HarnessClaudeCode)
	}
	if sess.ID != "0199aaaa-bbbb-cccc-dddd-eeeeffff0000" {
		t.Errorf("session ID = %q, want file stem", sess.ID)
	}

	// summary + meta lines skipped; paired tool_result empties its turn;
	// remaining assistant lines merge → [user, assistant]
	if len(sess.Turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(sess.Turns))
	}
	if sess.Turns[0].Role != "user" || sess.Turns[0].Blocks[0].Text != "hello" {
		t.Errorf("unexpected first turn: %+v", sess.Turns[0])
	}
	if strings.Contains(sess.Turns[0].Blocks[0].Text, "meta noise") {
		t.Error("meta line leaked into transcript")
	}
	if sess.Turns[1].Model != "claude-fable-5" {
		t.Errorf("model = %q", sess.Turns[1].Model)
	}
	if sess.Turns[0].Timestamp.IsZero() {
		t.Error("timestamp not parsed")
	}

	read := findToolCall(sess, "Read")
	if read == nil {
		t.Fatal("Read tool call missing")
	}
	if read.Input["file_path"] != "main.go" {
		t.Errorf("input = %v", read.Input)
	}
	if read.Result == nil || read.Result.Text != "file contents" {
		t.Errorf("result not paired: %+v", read.Result)
	}
}
