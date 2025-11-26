package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testState manages test state setup and cleanup for global variables
type testState struct {
	oldBrowseDir     string
	oldMarkdownFiles []string
	oldCurrentFile   string
}

// setupTestState sets up test state with proper mutex handling and automatic cleanup
func setupTestState(t *testing.T, dir string, files []string) *testState {
	t.Helper()

	fileMutex.Lock()
	state := &testState{
		oldBrowseDir:     browseDir,
		oldMarkdownFiles: markdownFiles,
		oldCurrentFile:   currentFile,
	}
	browseDir = dir
	markdownFiles = files
	fileMutex.Unlock()

	// Register cleanup with t.Cleanup for LIFO execution and panic safety
	t.Cleanup(func() {
		fileMutex.Lock()
		defer fileMutex.Unlock()
		browseDir = state.oldBrowseDir
		markdownFiles = state.oldMarkdownFiles
		currentFile = state.oldCurrentFile
	})

	return state
}

// createTestMarkdownFile creates a markdown file with specified content
func createTestMarkdownFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to create test file %s: %v", path, err)
	}
	return path
}

// createSimpleTestFile creates a basic test markdown file with standard content
func createSimpleTestFile(t *testing.T, dir string) string {
	t.Helper()
	return createTestMarkdownFile(t, dir, "test.md", "# Test")
}

// uniqueTestDir creates a uniquely named test directory in $HOME with automatic cleanup
func uniqueTestDir(t *testing.T, prefix string) string {
	t.Helper()
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot get home directory")
	}

	// Use test name for uniqueness instead of timestamp
	safeName := strings.ReplaceAll(t.Name(), "/", "_")
	dir := filepath.Join(homeDir, prefix+"_"+safeName+"_"+time.Now().Format("20060102150405"))

	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatalf("failed to create test dir: %v", err)
	}

	t.Cleanup(func() {
		os.RemoveAll(dir)
	})

	return dir
}

// assertValidHTML checks for required HTML structure elements
func assertValidHTML(t *testing.T, html string) {
	t.Helper()
	required := []string{
		"<!DOCTYPE html>",
		"<html",
		"<head>",
		"<body>",
		"</body>",
		"</html>",
	}
	for _, tag := range required {
		if !strings.Contains(html, tag) {
			t.Errorf("HTML missing required tag: %s", tag)
		}
	}
}

// assertContains is a helper for checking string containment with clear error messages
func assertContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected string to contain %q, got: %s", substr, s)
	}
}

// assertNotContains is a helper for checking string non-containment
func assertNotContains(t *testing.T, s, substr string) {
	t.Helper()
	if strings.Contains(s, substr) {
		t.Errorf("expected string NOT to contain %q, but it does", substr)
	}
}

// assertStatusCode checks HTTP status code with clear error message
func assertStatusCode(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("expected status code %d, got %d", want, got)
	}
}
