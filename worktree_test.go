package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsLinkedWorktree(t *testing.T) {
	root := t.TempDir()

	worktree := filepath.Join(root, "linked")
	mustMkdir(t, worktree)
	mustWrite(t, filepath.Join(worktree, ".git"), "gitdir: /repo/.git/worktrees/linked")

	mainRepo := filepath.Join(root, "main")
	mustMkdir(t, filepath.Join(mainRepo, ".git"))

	plain := filepath.Join(root, "plain")
	mustMkdir(t, plain)

	submodule := filepath.Join(root, "docs")
	mustMkdir(t, submodule)
	mustWrite(t, filepath.Join(submodule, ".git"), "gitdir: ../.git/modules/docs")

	tests := []struct {
		name string
		dir  string
		want bool
	}{
		{"linked worktree has a gitfile", worktree, true},
		{"main checkout has a .git directory", mainRepo, false},
		{"plain directory has no .git", plain, false},
		{"missing directory", filepath.Join(root, "nope"), false},
		{"submodule gitfile points at modules, holds unique content", submodule, false},
	}
	for _, tt := range tests {
		if got := isLinkedWorktree(tt.dir); got != tt.want {
			t.Errorf("%s: isLinkedWorktree() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestIsInLinkedWorktree(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "projects")
	worktree := filepath.Join(root, "repo", ".claude", "worktrees", "feat-x")
	mustMkdir(t, filepath.Join(worktree, "docs"))
	mustWrite(t, filepath.Join(worktree, ".git"), "gitdir: /repo/.git/worktrees/feat-x")
	mustMkdir(t, filepath.Join(root, "repo", "docs"))

	// A sibling whose name merely extends the browse root's.
	sibling := filepath.Join(parent, "projects-old")
	mustMkdir(t, sibling)
	mustWrite(t, filepath.Join(sibling, ".git"), "gitdir: /elsewhere/.git/worktrees/old")

	setBrowseDir(t, root)

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"file directly in worktree", filepath.Join(worktree, "README.md"), true},
		{"file nested under worktree", filepath.Join(worktree, "docs", "a.md"), true},
		{"the worktree directory itself", worktree, true},
		{"file in the main checkout", filepath.Join(root, "repo", "docs", "a.md"), false},
		{"file at the browse root", filepath.Join(root, "a.md"), false},
		{"sibling sharing a name prefix is outside the root", filepath.Join(sibling, "a.md"), false},
	}
	for _, tt := range tests {
		if got := isInLinkedWorktree(tt.path); got != tt.want {
			t.Errorf("%s: isInLinkedWorktree(%q) = %v, want %v", tt.name, tt.path, got, tt.want)
		}
	}
}

// isInLinkedWorktree must not walk above the browse root: pointing peekm at a
// worktree deliberately has to keep working.
func TestIsInLinkedWorktreeExemptsBrowseRoot(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".git"), "gitdir: /repo/.git/worktrees/feat-x")
	setBrowseDir(t, root)

	if isInLinkedWorktree(filepath.Join(root, "README.md")) {
		t.Error("browse root that is itself a worktree should not be excluded")
	}
}

func TestBulkWrite(t *testing.T) {
	base := time.Date(2026, 8, 5, 7, 11, 0, 0, time.UTC)
	// Rows are always sorted newest first before bulkWrite sees them.
	at := func(offset time.Duration, fromEvent bool) recentFile {
		return recentFile{rel: "a.md", mod: base.Add(-offset), fromEvent: fromEvent}
	}
	run := func(n int, offset time.Duration) []recentFile {
		rows := make([]recentFile, n)
		for i := range rows {
			rows[i] = at(offset, false)
		}
		return rows
	}

	tests := []struct {
		name string
		rows []recentFile
		want int
	}{
		{"empty", nil, 0},
		{"a run below the threshold is not bulk", run(bulkFileThreshold-1, 0), 0},
		{"checkout stamps every file at once", run(bulkFileThreshold, 0), bulkFileThreshold},
		{
			"burst stops at the window edge",
			append(run(bulkFileThreshold, 0), at(time.Hour, false)),
			bulkFileThreshold,
		},
		{
			"an AI event ends the burst",
			append(run(bulkFileThreshold, 0), at(time.Second, true)),
			bulkFileThreshold,
		},
		{
			"a row with an AI event never heads a burst",
			append([]recentFile{at(0, true)}, run(bulkFileThreshold, 0)...),
			0,
		},
		{
			"drift beyond the window cannot chain",
			append(run(bulkFileThreshold, 0), at(3*time.Second, false)),
			bulkFileThreshold,
		},
	}
	for _, tt := range tests {
		if got := len(bulkWrite(tt.rows)); got != tt.want {
			t.Errorf("%s: len(bulkWrite()) = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestCommonDirPrefix(t *testing.T) {
	rowsFor := func(paths ...string) []recentFile {
		rows := make([]recentFile, len(paths))
		for i, p := range paths {
			rows[i] = recentFile{rel: filepath.FromSlash(p)}
		}
		return rows
	}

	tests := []struct {
		name string
		rows []recentFile
		want string
	}{
		{"identical directory", rowsFor("repo/docs/a.md", "repo/docs/b.md"), "repo/docs/"},
		{"shared ancestor", rowsFor("repo/docs/a.md", "repo/plans/b.md"), "repo/"},
		{"one is an ancestor of the other", rowsFor("repo/a.md", "repo/docs/b.md"), "repo/"},
		{"no shared prefix", rowsFor("one/a.md", "two/b.md"), ""},
		{"files at the browse root", rowsFor("a.md", "b.md"), ""},
		{"single row", rowsFor("repo/docs/a.md"), "repo/docs/"},
		{"partly-matched segment does not count", rowsFor("a/foo.md", "a/food/x.md"), "a/"},
		{"sibling dirs sharing a name prefix", rowsFor("ab/x.md", "ac/y.md"), ""},
	}
	for _, tt := range tests {
		if got := commonDirPrefix(tt.rows); got != tt.want {
			t.Errorf("%s: commonDirPrefix() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func setBrowseDir(t *testing.T, dir string) {
	t.Helper()
	fileMutex.Lock()
	prev := browseDir
	browseDir = dir
	fileMutex.Unlock()
	t.Cleanup(func() {
		fileMutex.Lock()
		browseDir = prev
		fileMutex.Unlock()
	})
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
