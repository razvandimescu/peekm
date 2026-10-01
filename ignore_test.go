package main

import (
	"path/filepath"
	"slices"
	"testing"
)

// writeIgnoreTree writes each path (relative to a fresh temp $HOME) with the given
// contents and clears memoized patterns so each case starts clean.
func writeIgnoreTree(t *testing.T, files map[string]string) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	for rel, content := range files {
		p := filepath.Join(home, rel)
		mustMkdir(t, filepath.Dir(p))
		mustWrite(t, p, content)
	}
	globalIgnoreCache.reset()
	t.Cleanup(globalIgnoreCache.reset)
	return home
}

func scanRelative(t *testing.T, home, scanDir string) []string {
	t.Helper()
	var rel []string
	for _, f := range collectMarkdownFiles(scanDir) {
		r, err := filepath.Rel(home, f)
		if err != nil {
			t.Fatal(err)
		}
		rel = append(rel, filepath.ToSlash(r))
	}
	slices.Sort(rel)
	return rel
}

// TestComposedIgnoreFiles checks that a file's location scopes its patterns: the one
// at $HOME reaches every repo, the one in repo/ reaches only repo.
func TestComposedIgnoreFiles(t *testing.T) {
	home := writeIgnoreTree(t, map[string]string{
		".peekmignore":         "drafts\n",
		"repo/.peekmignore":    "worktrees\n",
		"repo/keep/a.md":       "x",
		"repo/drafts/b.md":     "x",
		"repo/worktrees/c.md":  "x",
		"other/keep/d.md":      "x",
		"other/drafts/e.md":    "x",
		"other/worktrees/f.md": "x",
	})

	want := []string{"other/keep/d.md", "other/worktrees/f.md", "repo/keep/a.md"}
	if got := scanRelative(t, home, home); !slices.Equal(got, want) {
		t.Errorf("scan = %v, want %v", got, want)
	}
}

// TestIgnoreFileScopedToSubtree is the case a single root-level pattern set cannot
// express: excluding one repo's directory by name without touching its namesakes.
func TestIgnoreFileScopedToSubtree(t *testing.T) {
	home := writeIgnoreTree(t, map[string]string{
		"a/.peekmignore": "notes\n",
		"a/notes/x.md":   "x",
		"a/keep/y.md":    "x",
		"b/notes/z.md":   "x",
	})

	want := []string{"a/keep/y.md", "b/notes/z.md"}
	if got := scanRelative(t, home, home); !slices.Equal(got, want) {
		t.Errorf("scan = %v, want %v", got, want)
	}
}

// TestIgnorePatternsInheritedBelowScanRoot checks that patterns from directories
// above the scan root still apply — the memo is keyed by directory, not by root.
func TestIgnorePatternsInheritedBelowScanRoot(t *testing.T) {
	home := writeIgnoreTree(t, map[string]string{
		".peekmignore":     "drafts\n",
		"repo/keep/a.md":   "x",
		"repo/drafts/b.md": "x",
	})

	want := []string{"repo/keep/a.md"}
	if got := scanRelative(t, home, filepath.Join(home, "repo")); !slices.Equal(got, want) {
		t.Errorf("scan from repo = %v, want %v", got, want)
	}
}

// TestIgnoreFileRejectsPathPattern confirms anchoring syntax stays unsupported,
// since a .peekmignore's location is what scopes it.
func TestIgnoreFileRejectsPathPattern(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ".peekmignore"), "repo/drafts\nkeep\n")

	if got := parseIgnoreFile(dir); !slices.Equal(got, []string{"keep"}) {
		t.Errorf("parseIgnoreFile = %v, want only the separator-free pattern", got)
	}
}
