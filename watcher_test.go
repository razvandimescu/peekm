package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// withWatchedTree builds a tree under a temp $HOME, whitelists the collectable
// files as the startup scan would and starts dirWatcher on it, returning the root.
func withWatchedTree(t *testing.T, dirs []string, files []string) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var whitelist []string
	for _, f := range files {
		p := filepath.Join(home, f)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if isCollectableFile(f) {
			whitelist = append(whitelist, p)
		}
	}

	fileMutex.Lock()
	oldBrowseDir, oldFiles := browseDir, markdownFiles
	browseDir, markdownFiles = home, whitelist
	fileMutex.Unlock()
	t.Cleanup(func() {
		dirWatcher.close()
		fileMutex.Lock()
		browseDir, markdownFiles = oldBrowseDir, oldFiles
		fileMutex.Unlock()
	})

	if err := dirWatcher.watchDirectory(home); err != nil {
		t.Fatal(err)
	}
	return home
}

func watchedRel(t *testing.T, root string) []string {
	t.Helper()
	dirWatcher.mu.Lock()
	paths := dirWatcher.current.WatchList()
	dirWatcher.mu.Unlock()
	var rel []string
	for _, p := range paths {
		r, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatal(err)
		}
		rel = append(rel, r)
	}
	sort.Strings(rel)
	return rel
}

func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestWatchDirectory_WatchesOnlyMarkdownDirsAndAncestors(t *testing.T) {
	root := withWatchedTree(t,
		[]string{"a/b", "src/pkg", "docs", "notes"},
		[]string{"a/b/notes.md", "src/pkg/main.go", "docs/guide.md", "README.md", "notes/todo.txt"})

	assertStrings(t, watchedRel(t, root), []string{".", "a", "a/b", "docs"})
}

func TestAdoptHookMarkdown(t *testing.T) {
	root := withWatchedTree(t,
		[]string{"src", "node_modules/pkg", "docs"},
		[]string{"docs/guide.md"})

	write := func(rel string) string {
		p := filepath.Join(root, rel)
		if err := os.WriteFile(p, []byte("# hi"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	adopted := write("src/NOTES.md")
	adoptHookMarkdown(adopted)
	if !isWhitelistedFile(adopted) {
		t.Error("markdown in unwatched dir was not whitelisted")
	}
	assertStrings(t, watchedRel(t, root), []string{".", "docs", "src"})

	excluded := write("node_modules/pkg/README.md")
	adoptHookMarkdown(excluded)
	if isWhitelistedFile(excluded) {
		t.Error("markdown in excluded dir was whitelisted")
	}

	adoptHookMarkdown(filepath.Join(root, "src", "missing.md"))
	if isWhitelistedFile(filepath.Join(root, "src", "missing.md")) {
		t.Error("nonexistent file was whitelisted")
	}
}

func TestHandleDirCreated_WatchesNewDirAndMarkdownSubdirs(t *testing.T) {
	root := withWatchedTree(t,
		[]string{"docs", "new/deep", "new/empty", "node_modules/pkg"},
		[]string{"docs/guide.md"})
	plan := filepath.Join(root, "new", "deep", "plan.md")
	if err := os.WriteFile(plan, []byte("# plan"), 0o644); err != nil {
		t.Fatal(err)
	}

	handleDirCreated(filepath.Join(root, "new"))
	handleDirCreated(filepath.Join(root, "node_modules"))

	if !isWhitelistedFile(plan) {
		t.Error("markdown inside new dir was not whitelisted")
	}
	assertStrings(t, watchedRel(t, root), []string{".", "docs", "new", "new/deep"})
}
