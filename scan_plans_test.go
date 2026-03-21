package main

import (
	"os"
	"path/filepath"
	"testing"
)

func withCleanWhitelist(t *testing.T) {
	t.Helper()
	fileMutex.Lock()
	orig := markdownFiles
	markdownFiles = nil
	fileMutex.Unlock()
	t.Cleanup(func() {
		fileMutex.Lock()
		markdownFiles = orig
		fileMutex.Unlock()
	})
}

func TestScanUntrackedPlans_WhitelistsNewFiles(t *testing.T) {
	plansDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "cache")

	planPath := filepath.Join(plansDir, "test-plan.md")
	if err := os.WriteFile(planPath, []byte("# Plan: Test Plan\n\nSome content"), 0644); err != nil {
		t.Fatal(err)
	}

	tracked := map[string]*SessionMetadata{}
	withCleanWhitelist(t)

	scanUntrackedPlans(tracked, plansDir, cacheDir)

	if !isWhitelistedFile(planPath) {
		t.Error("plan file should be whitelisted after scan")
	}

	cachedPath := filepath.Join(cacheDir, "test-plan.md")
	if _, err := os.Stat(cachedPath); err != nil {
		t.Errorf("cached plan file should exist: %v", err)
	}
}

func TestScanUntrackedPlans_SkipsTrackedFiles(t *testing.T) {
	plansDir := t.TempDir()

	planPath := filepath.Join(plansDir, "existing.md")
	if err := os.WriteFile(planPath, []byte("# Plan: Existing"), 0644); err != nil {
		t.Fatal(err)
	}

	tracked := map[string]*SessionMetadata{
		planPath: {ToolName: "Write"},
	}
	withCleanWhitelist(t)

	scanUntrackedPlans(tracked, plansDir, "")

	if isWhitelistedFile(planPath) {
		t.Error("tracked plan file should not be re-whitelisted")
	}
}

func TestScanUntrackedPlans_SkipsNonMarkdown(t *testing.T) {
	plansDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(plansDir, "notes.txt"), []byte("not markdown"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(plansDir, "subdir.md"), 0755); err != nil {
		t.Fatal(err)
	}

	tracked := map[string]*SessionMetadata{}
	withCleanWhitelist(t)

	scanUntrackedPlans(tracked, plansDir, "")

	fileMutex.Lock()
	count := len(markdownFiles)
	fileMutex.Unlock()

	if count != 0 {
		t.Errorf("expected 0 whitelisted files, got %d", count)
	}
}

func TestScanUntrackedPlans_EmptyDir(t *testing.T) {
	// Verify no panic on empty, missing, or blank dir
	scanUntrackedPlans(map[string]*SessionMetadata{}, t.TempDir(), "")
	scanUntrackedPlans(map[string]*SessionMetadata{}, "", "")
	scanUntrackedPlans(map[string]*SessionMetadata{}, "/nonexistent/path", "")
}
