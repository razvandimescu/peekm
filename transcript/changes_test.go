package transcript

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// changesFixture writes Claude Code transcript lines and checkpoint backups
// the way Claude Code lays them out on disk.
type changesFixture struct {
	t       *testing.T
	history string
	lines   []string
	clock   time.Time
}

func newChangesFixture(t *testing.T) *changesFixture {
	return &changesFixture{t: t, history: t.TempDir(), clock: time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)}
}

func (f *changesFixture) tick() string {
	f.clock = f.clock.Add(time.Second)
	return f.clock.Format(time.RFC3339Nano)
}

func (f *changesFixture) add(line map[string]any) {
	b, err := json.Marshal(line)
	if err != nil {
		f.t.Fatal(err)
	}
	f.lines = append(f.lines, string(b))
}

func (f *changesFixture) result(r map[string]any) {
	r["structuredPatch"] = []any{}
	f.add(map[string]any{"type": "user", "timestamp": f.tick(), "toolUseResult": r})
}

// edit records an Edit result; original is nil for files over ~10 KB.
func (f *changesFixture) edit(path, old, new string, original *string) {
	f.result(map[string]any{"filePath": path, "oldString": old, "newString": new, "replaceAll": false, "originalFile": original})
}

func (f *changesFixture) write(path, kind, content string) {
	f.result(map[string]any{"filePath": path, "type": kind, "content": content, "originalFile": nil})
}

func (f *changesFixture) backup(path string, version int, content string) string {
	name := fmt.Sprintf("%s@v%d", backupHash(path), version)
	if err := os.WriteFile(filepath.Join(f.history, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return name
}

// checkpoint records the backup Claude Code takes of an edited file at a
// user prompt; its key is relative to cwd, as Claude Code writes it.
func (f *changesFixture) checkpoint(path string, version int, content string) {
	name := f.backup(path, version, content)
	at := f.tick()
	f.add(map[string]any{"type": "file-history-snapshot", "snapshot": map[string]any{
		"trackedFileBackups": map[string]any{filepath.Base(path): map[string]any{"backupFileName": name, "version": version, "backupTime": at}},
	}})
}

func (f *changesFixture) changes() map[string]FileChange {
	f.t.Helper()
	path := filepath.Join(f.t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(f.lines, "\n")+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	list, err := ClaudeChanges(path, f.history)
	if err != nil {
		f.t.Fatal(err)
	}
	byPath := map[string]FileChange{}
	for _, c := range list {
		byPath[c.Path] = c
	}
	return byPath
}

func ptr(s string) *string { return &s }

func TestClaudeChangesNetDiff(t *testing.T) {
	f := newChangesFixture(t)
	f.edit("/p/small.go", "a", "b", ptr("a\nx\n"))
	f.edit("/p/small.go", "x", "y", ptr("b\nx\n"))
	f.backup("/p/large.go", 1, "one\ntwo\n")
	f.edit("/p/large.go", "two", "2", nil)
	f.write("/p/new.md", "create", "hello\n")
	f.edit("/p/new.md", "hello", "hi", nil)
	f.edit("/p/reverted.go", "on", "off", ptr("on\n"))
	f.edit("/p/reverted.go", "off", "on", ptr("off\n"))

	got := f.changes()
	want := map[string]FileChange{
		"/p/small.go": {Path: "/p/small.go", Before: "a\nx\n", After: "b\ny\n"},
		"/p/large.go": {Path: "/p/large.go", Before: "one\ntwo\n", After: "one\n2\n"},
		"/p/new.md":   {Path: "/p/new.md", After: "hi\n", Created: true},
	}
	if len(got) != len(want) {
		t.Errorf("got %d files, want %d (net-zero file must be dropped): %+v", len(got), len(want), got)
	}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("%s = %+v, want %+v", p, got[p], w)
		}
	}
}

func TestClaudeChangesResyncs(t *testing.T) {
	f := newChangesFixture(t)
	// A Bash command rewrote the file between prompts; the checkpoint at the
	// next prompt carries the true content, so the replay picks it up.
	f.backup("/p/fmt.go", 1, "a  =  1\n")
	f.edit("/p/fmt.go", "1", "2", nil)
	f.checkpoint("/p/fmt.go", 2, "a = 2\n")
	f.edit("/p/fmt.go", "a = 2", "a = 3", nil)

	// An edit that no longer matches loses track until the next checkpoint.
	f.backup("/p/drift.go", 1, "x\n")
	f.edit("/p/drift.go", "missing", "y", nil)
	f.checkpoint("/p/drift.go", 2, "z\n")
	f.edit("/p/drift.go", "z", "w", nil)

	got := f.changes()
	if c := got["/p/fmt.go"]; c.Partial || c.After != "a = 3\n" {
		t.Errorf("fmt.go = %+v, want After from checkpoint plus later edit", c)
	}
	if c := got["/p/drift.go"]; c.Partial || c.Before != "x\n" || c.After != "w\n" {
		t.Errorf("drift.go = %+v, want recovered at checkpoint", c)
	}
}

func TestClaudeChangesPartial(t *testing.T) {
	f := newChangesFixture(t)
	f.edit("/p/nobase.go", "a", "b", nil) // no OriginalFile, no backup
	f.backup("/p/lost.go", 1, "a\n")
	f.edit("/p/lost.go", "a", "b", nil)
	f.edit("/p/lost.go", "a", "c", nil) // "a" is gone: changed outside the edits
	f.backup("/p/twice.go", 1, "a a\n")
	f.edit("/p/twice.go", "a", "b", nil) // not unique, so Claude Code could not have applied it

	got := f.changes()
	for _, p := range []string{"/p/nobase.go", "/p/lost.go", "/p/twice.go"} {
		if c := got[p]; !c.Partial || c.Before != "" || c.After != "" {
			t.Errorf("%s = %+v, want Partial with no content", p, c)
		}
	}
}

func TestClaudeChangesIgnoresUnsafeBackupNames(t *testing.T) {
	f := newChangesFixture(t)
	outside := filepath.Join(filepath.Dir(f.history), "outside")
	if err := os.WriteFile(outside, []byte("leaked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.edit("/p/a.go", "a", "b", ptr("a\n"))
	f.add(map[string]any{"type": "file-history-snapshot", "snapshot": map[string]any{
		"trackedFileBackups": map[string]any{"a.go": map[string]any{
			"backupFileName": backupHash("/p/a.go") + "@v2/../../outside", "backupTime": f.tick(),
		}},
	}})
	if c := f.changes()["/p/a.go"]; c.After != "b\n" {
		t.Errorf("a.go = %+v, want the traversal checkpoint ignored", c)
	}
}
