package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sseClient(t *testing.T) chan string {
	t.Helper()
	ch := make(chan string, 64)
	clientsMutex.Lock()
	clients[ch] = true
	clientsMutex.Unlock()
	t.Cleanup(func() {
		clientsMutex.Lock()
		delete(clients, ch)
		clientsMutex.Unlock()
	})
	return ch
}

// collect returns the messages that arrive within d.
func collect(ch chan string, d time.Duration) []string {
	var msgs []string
	timeout := time.After(d)
	for {
		select {
		case msg := <-ch:
			msgs = append(msgs, msg)
		case <-timeout:
			return msgs
		}
	}
}

// Saves replace the file (temp + rename); every save must still reach the
// page, with the path the page has in its URL.
func TestOpenFileWatchSurvivesSaves(t *testing.T) {
	dir := t.TempDir()
	setBrowseDir(t, dir)
	path := filepath.Join(dir, "plan.md")
	mustWrite(t, path, "- [ ] a\n")
	ch := sseClient(t)

	var wm watcherManager
	if err := wm.watch(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wm.close)

	for i, content := range []string{"- [x] a\n", "- [ ] a\n"} {
		if err := atomicWriteFile(path, content); err != nil {
			t.Fatal(err)
		}
		msgs := collect(ch, 500*time.Millisecond)
		if len(msgs) == 0 || !strings.Contains(msgs[0], `"type":"file_modified","path":"plan.md"`) {
			t.Fatalf("save %d: got %q, want file_modified for plan.md", i+1, msgs)
		}
	}
}

// The tree watcher must not report a save as the file being removed and
// added again; a real delete still is one.
func TestDirWatchIgnoresSaves(t *testing.T) {
	dir := t.TempDir()
	setBrowseDir(t, dir)
	path := filepath.Join(dir, "notes.md")
	mustWrite(t, path, "- [ ] a\n")
	addToWhitelist(path)
	t.Cleanup(func() { removeFromWhitelist(path) })
	ch := sseClient(t)

	var wm watcherManager
	if err := wm.watchDirectory(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wm.close)

	if err := atomicWriteFile(path, "- [x] a\n"); err != nil {
		t.Fatal(err)
	}
	if msgs := collect(ch, 500*time.Millisecond); len(msgs) > 0 || !isWhitelistedFile(path) {
		t.Fatalf("save: got %q, whitelisted=%v; want no events and still whitelisted", msgs, isWhitelistedFile(path))
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if msgs := collect(ch, 500*time.Millisecond); len(msgs) != 1 || !strings.Contains(msgs[0], `"type":"file_removed","path":"notes.md"`) {
		t.Fatalf("delete: got %q, want one file_removed", msgs)
	}
}
