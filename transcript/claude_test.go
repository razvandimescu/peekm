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
	if sess.Usage != nil {
		t.Errorf("usage reported for a transcript that has none: %+v", sess.Usage)
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

// usageFixture covers the three shapes the real corpus produces: a normal
// assistant record, a later record whose usage is all zeroes (Claude Code
// emits these occasionally and they must not blank the readout), and a final
// record carrying usage but no renderable content.
const usageFixture = `{"type":"user","timestamp":"2026-10-24T09:00:00.000Z","message":{"role":"user","content":"hi"}}
{"type":"assistant","timestamp":"2026-10-24T09:00:05.000Z","message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"one"}],"usage":{"input_tokens":2,"cache_creation_input_tokens":40000,"cache_read_input_tokens":4000}}}
{"type":"assistant","timestamp":"2026-10-24T09:01:00.000Z","message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"two"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":1000,"cache_read_input_tokens":100000}}}
{"type":"assistant","timestamp":"2026-10-24T09:02:00.000Z","message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"three"}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
{"type":"assistant","timestamp":"2026-10-24T09:03:00.000Z","message":{"role":"assistant","model":"claude-opus-5","content":[],"usage":{"input_tokens":5,"cache_creation_input_tokens":2000,"cache_read_input_tokens":120000}}}
`

func TestParseClaudeUsage(t *testing.T) {
	sess, err := ParseClaude(strings.NewReader(usageFixture))
	if err != nil {
		t.Fatalf("ParseClaude: %v", err)
	}
	if sess.Usage == nil {
		t.Fatal("no usage parsed")
	}
	// The last record carries usage but no blocks; it is still the newest
	// prompt size, so it must win over the renderable record before it.
	if got, want := sess.Usage.ContextTokens, 122005; got != want {
		t.Errorf("ContextTokens = %d, want %d", got, want)
	}
	if got, want := sess.Usage.At.Format("15:04:05"), "09:03:00"; got != want {
		t.Errorf("At = %s, want %s", got, want)
	}
	// ...and that content-free record must not become a turn: the three
	// assistant records with text merge into one, leaving user + assistant.
	if got, want := len(sess.Turns), 2; got != want {
		t.Errorf("turns = %d, want %d", got, want)
	}
}
