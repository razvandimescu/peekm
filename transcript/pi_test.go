package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// piFixturePath is the shared pi session fixture, also used by the
// main-package render tests.
const piFixturePath = "testdata/pi-session.jsonl"

func sessionText(sess *Session) string {
	var all []byte
	for _, turn := range sess.Turns {
		for _, b := range turn.Blocks {
			all = append(all, b.Text...)
		}
	}
	return string(all)
}

func TestParsePiFile(t *testing.T) {
	sess, err := ParseFile(piFixturePath) // via dispatch, exercises sniffing too
	if err != nil {
		t.Fatal(err)
	}

	checkPiSessionMeta(t, sess)
	checkPiActiveBranch(t, sess)
	checkPiEditCall(t, sess)
	checkPiBashCall(t, sess)
}

func checkPiSessionMeta(t *testing.T, sess *Session) {
	t.Helper()
	if sess.Harness != HarnessPi {
		t.Errorf("harness = %q, want %q", sess.Harness, HarnessPi)
	}
	if sess.ID != "019fc7d2-71ee-7da3-8497-cf2834825a90" {
		t.Errorf("session ID = %q, want header id", sess.ID)
	}
	if sess.CWD != "/tmp/proj" {
		t.Errorf("cwd = %q, want /tmp/proj", sess.CWD)
	}
	if sess.Turns[1].Model != "gpt-5.6-sol" {
		t.Errorf("model = %q, want gpt-5.6-sol", sess.Turns[1].Model)
	}
}

func checkPiActiveBranch(t *testing.T, sess *Session) {
	t.Helper()
	text := sessionText(sess)
	for _, want := range []string{"fix the bug", "On it.", "Done."} {
		if !strings.Contains(text, want) {
			t.Errorf("transcript missing %q", want)
		}
	}
	if strings.Contains(text, "abandoned branch reply") {
		t.Error("abandoned branch entry leaked into active-branch transcript")
	}
}

// checkPiEditCall: edit toolCall normalizes to Edit with Claude key names and
// pairs its toolResult.
func checkPiEditCall(t *testing.T, sess *Session) {
	t.Helper()
	edit := findToolCall(sess, "Edit")
	if edit == nil {
		t.Fatal("Edit tool call missing")
	}
	if edit.RawName != "edit" {
		t.Errorf("RawName = %q, want edit", edit.RawName)
	}
	if edit.Input["file_path"] != "main.go" || edit.Input["old_string"] != "a" {
		t.Errorf("input not normalized: %v", edit.Input)
	}
	if edit.Result == nil || edit.Result.Text != "ok" {
		t.Errorf("toolResult not paired: %+v", edit.Result)
	}
}

// checkPiBashCall: a user "!" command models as a Bash call carrying its output.
func checkPiBashCall(t *testing.T, sess *Session) {
	t.Helper()
	bash := findToolCall(sess, "Bash")
	if bash == nil {
		t.Fatal("bashExecution block missing")
	}
	if bash.RawName != "bashExecution" || bash.Input["command"] != "go test ./..." {
		t.Errorf("unexpected bash call: %+v", bash)
	}
	if bash.Result == nil || bash.Result.Text != "PASS" || bash.Result.IsError {
		t.Errorf("unexpected bash result: %+v", bash.Result)
	}
}

func TestNormalizePiToolInput(t *testing.T) {
	// write: path → file_path
	name, m := normalizePiToolInput("write", map[string]any{"path": "a.md", "content": "x"})
	if name != "Write" || m["file_path"] != "a.md" || m["path"] != nil {
		t.Errorf("write: got name=%q map=%v", name, m)
	}

	// grep keeps its path key (Claude Grep uses "path" too)
	name, m = normalizePiToolInput("grep", map[string]any{"pattern": "foo", "path": "src"})
	if name != "Grep" || m["path"] != "src" {
		t.Errorf("grep: got name=%q map=%v", name, m)
	}

	// single edit → Edit with old_string/new_string
	name, m = normalizePiToolInput("edit", map[string]any{
		"path":  "a.go",
		"edits": []any{map[string]any{"oldText": "old", "newText": "new"}},
	})
	if name != "Edit" || m["old_string"] != "old" || m["new_string"] != "new" || m["edits"] != nil {
		t.Errorf("single edit: got name=%q map=%v", name, m)
	}

	// multiple edits → MultiEdit with converted key names
	name, m = normalizePiToolInput("edit", map[string]any{
		"path": "a.go",
		"edits": []any{
			map[string]any{"oldText": "o1", "newText": "n1"},
			map[string]any{"oldText": "o2", "newText": "n2"},
		},
	})
	edits, ok := m["edits"].([]any)
	if name != "MultiEdit" || !ok || len(edits) != 2 {
		t.Fatalf("multi edit: got name=%q map=%v", name, m)
	}
	if first := edits[0].(map[string]any); first["old_string"] != "o1" {
		t.Errorf("multi edit keys not converted: %v", first)
	}

	// unknown tool name is capitalized, args pass through
	name, _ = normalizePiToolInput("mytool", map[string]any{"x": 1})
	if name != "Mytool" {
		t.Errorf("unknown tool: got %q", name)
	}
}

func TestIsPiSessionFile(t *testing.T) {
	path := piFixturePath
	if !IsPiSessionFile(path) {
		t.Error("pi fixture not detected as pi session")
	}

	claude := filepath.Join(t.TempDir(), "claude.jsonl")
	os.WriteFile(claude, []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0644)
	if IsPiSessionFile(claude) {
		t.Error("claude transcript misdetected as pi session")
	}
}

func TestPiSessionCwd(t *testing.T) {
	if got := PiSessionCwd(piFixturePath); got != "/tmp/proj" {
		t.Errorf("cwd = %q, want /tmp/proj", got)
	}
}
