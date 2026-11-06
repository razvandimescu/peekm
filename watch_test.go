package main

import (
	"os"
	"path/filepath"
	"slices"
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

// An unreadable directory or an entry that cannot be watched must not stop
// the watch on the rest of the tree.
func TestDirWatchSkipsBadEntries(t *testing.T) {
	dir := t.TempDir()
	setBrowseDir(t, dir)
	locked := filepath.Join(dir, "locked")
	for _, d := range []string{locked, filepath.Join(dir, "sub")} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	// kqueue (macOS, BSD) cannot watch a directory that holds a symlink loop.
	if err := os.Symlink("loop", filepath.Join(dir, "loop")); err != nil {
		t.Fatal(err)
	}
	ch := sseClient(t)

	var wm watcherManager
	if err := wm.watchDirectory(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wm.close)

	path := filepath.Join(dir, "sub", "new.md")
	mustWrite(t, path, "# new\n")
	t.Cleanup(func() { removeFromWhitelist(path) })
	if msgs := collect(ch, 500*time.Millisecond); len(msgs) != 1 || !strings.Contains(msgs[0], `"type":"file_added","path":"sub/new.md"`) {
		t.Fatalf("got %q, want one file_added for sub/new.md", msgs)
	}
}

// kqueue holds a file descriptor per entry of each watched directory, so the
// directories to watch stop before their entries pass the limit.
func TestCollectDirectoriesStopsAtLimit(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(dir, sub, "x.md"), "x")
	}

	var wm watcherManager
	// dir holds 2 entries (a, b); a and b hold 1 each
	got := wm.collectDirectories(dir, 3)
	if want := []string{dir, filepath.Join(dir, "a")}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}
