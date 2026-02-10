package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateAndResolvePath tests the path validation and security checks
func TestValidateAndResolvePath(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("cannot get home directory: %v", err)
	}

	tests := []struct {
		name        string
		input       string
		wantErr     bool
		errContains string
		setup       func() (string, func()) // Returns test path and cleanup function
	}{
		{
			name: "valid path with tilde",
			setup: func() (string, func()) {
				testPath := filepath.Join(homeDir, "peekm_test_tilde")
				os.Mkdir(testPath, 0755)
				cleanup := func() { os.Remove(testPath) }
				return "~/peekm_test_tilde", cleanup
			},
			wantErr: false,
		},
		{
			name:    "tilde only expands to home",
			input:   "~",
			wantErr: false,
		},
		{
			name:        "path outside home directory - /etc",
			input:       "/etc/passwd",
			wantErr:     true,
			errContains: "access denied",
		},
		{
			name:        "path outside home directory - /tmp",
			input:       "/tmp",
			wantErr:     true,
			errContains: "access denied",
		},
		{
			name:        "path traversal attack",
			input:       filepath.Join(homeDir, "docs/../../../etc/passwd"),
			wantErr:     true,
			errContains: "access denied",
		},
		{
			name: "symlink pointing outside home",
			setup: func() (string, func()) {
				linkPath := filepath.Join(homeDir, "test_evil_symlink")
				os.Symlink("/etc/passwd", linkPath)
				cleanup := func() {
					os.Remove(linkPath)
				}
				return linkPath, cleanup
			},
			wantErr:     true,
			errContains: "access denied",
		},
		{
			name: "symlink pointing inside home (valid)",
			setup: func() (string, func()) {
				targetPath := filepath.Join(homeDir, "test_target")
				os.WriteFile(targetPath, []byte("test"), 0644)
				linkPath := filepath.Join(homeDir, "test_good_symlink")
				os.Symlink(targetPath, linkPath)
				cleanup := func() {
					os.Remove(linkPath)
					os.Remove(targetPath)
				}
				return linkPath, cleanup
			},
			wantErr: false,
		},
		{
			name:        "non-existent path",
			input:       filepath.Join(homeDir, "nonexistent_dir_12345_test"),
			wantErr:     true,
			errContains: "does not exist",
		},
		{
			name: "valid absolute path in home",
			setup: func() (string, func()) {
				testPath := filepath.Join(homeDir, "test_valid_path")
				os.Mkdir(testPath, 0755)
				cleanup := func() {
					os.Remove(testPath)
				}
				return testPath, cleanup
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testInput := tt.input
			var cleanup func()

			if tt.setup != nil {
				var setupPath string
				setupPath, cleanup = tt.setup()
				if cleanup != nil {
					defer cleanup()
				}
				testInput = setupPath
			}

			result, err := validateAndResolvePath(testInput)

			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				} else if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.errContains)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if result != "" && !strings.HasPrefix(result, homeDir) {
					t.Errorf("result %q not in home directory %q", result, homeDir)
				}
			}
		})
	}
}

// TestCollectMarkdownFiles_SymlinkSecurity tests symlink security in file collection
func TestCollectMarkdownFiles_SymlinkSecurity(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("cannot get home directory: %v", err)
	}

	// Create test directory in home (required for security checks)
	testDir := filepath.Join(homeDir, "peekm_test_symlinks")
	os.Mkdir(testDir, 0755)
	defer os.RemoveAll(testDir)

	// Create legitimate .md file
	mdFile := filepath.Join(testDir, "valid.md")
	os.WriteFile(mdFile, []byte("# Test"), 0644)

	// Create symlink to file outside $HOME
	evilSymlink := filepath.Join(testDir, "evil.md")
	os.Symlink("/etc/passwd", evilSymlink)
	defer os.Remove(evilSymlink)

	// Create symlink to file inside $HOME (should be included)
	goodTarget := filepath.Join(homeDir, "peekm_test_target.md")
	os.WriteFile(goodTarget, []byte("# Good"), 0644)
	defer os.Remove(goodTarget)

	goodSymlink := filepath.Join(testDir, "good.md")
	os.Symlink(goodTarget, goodSymlink)
	defer os.Remove(goodSymlink)

	files := collectMarkdownFiles(testDir)

	// Should include valid.md and good.md, but NOT evil.md
	if len(files) != 2 {
		t.Errorf("expected 2 files, got %d: %v", len(files), files)
	}

	hasValid := false
	hasGood := false
	for _, f := range files {
		if strings.Contains(f, "valid.md") {
			hasValid = true
		}
		if strings.Contains(f, "good.md") {
			hasGood = true
		}
		if strings.Contains(f, "evil.md") {
			t.Errorf("SECURITY VIOLATION: symlink to /etc/passwd was included: %s", f)
		}
	}

	if !hasValid {
		t.Error("valid.md should be included")
	}
	if !hasGood {
		t.Error("good.md (symlink inside home) should be included")
	}
}

// TestCollectMarkdownFiles_HardcodedExclusions tests that hardcoded exclusions are skipped
func TestCollectMarkdownFiles_HardcodedExclusions(t *testing.T) {
	testDir := t.TempDir()

	// Create files in various locations
	os.WriteFile(filepath.Join(testDir, "root.md"), []byte("# Root"), 0644)

	// Hidden directory (.hidden - should be EXCLUDED)
	os.Mkdir(filepath.Join(testDir, ".hidden"), 0755)
	os.WriteFile(filepath.Join(testDir, ".hidden", "secret.md"), []byte("# Secret"), 0644)

	// .claude directory (should be INCLUDED - whitelisted for AI workflows)
	os.Mkdir(filepath.Join(testDir, ".claude"), 0755)
	os.Mkdir(filepath.Join(testDir, ".claude", "plans"), 0755)
	os.WriteFile(filepath.Join(testDir, ".claude", "plans", "feature.md"), []byte("# Feature"), 0644)

	// node_modules (should be skipped - hardcoded exclusion)
	os.Mkdir(filepath.Join(testDir, "node_modules"), 0755)
	os.WriteFile(filepath.Join(testDir, "node_modules", "lib.md"), []byte("# Lib"), 0644)

	// vendor (should be skipped - hardcoded exclusion)
	os.Mkdir(filepath.Join(testDir, "vendor"), 0755)
	os.WriteFile(filepath.Join(testDir, "vendor", "dep.md"), []byte("# Dep"), 0644)

	// dist (should be skipped - hardcoded exclusion)
	os.Mkdir(filepath.Join(testDir, "dist"), 0755)
	os.WriteFile(filepath.Join(testDir, "dist", "build.md"), []byte("# Build"), 0644)

	// Regular subdirectory (should be included)
	os.Mkdir(filepath.Join(testDir, "docs"), 0755)
	os.WriteFile(filepath.Join(testDir, "docs", "doc.md"), []byte("# Doc"), 0644)

	files := collectMarkdownFiles(testDir)

	if len(files) != 3 {
		t.Errorf("expected 3 files (root.md, feature.md, doc.md), got %d: %v", len(files), files)
	}

	// Verify root.md, feature.md, and doc.md are included
	hasRoot := false
	hasClaude := false
	hasDoc := false
	for _, f := range files {
		if strings.Contains(f, "root.md") {
			hasRoot = true
		}
		if strings.Contains(f, "feature.md") {
			hasClaude = true
		}
		if strings.Contains(f, "doc.md") {
			hasDoc = true
		}
		// Verify exclusions are NOT included
		if strings.Contains(f, "secret.md") || strings.Contains(f, "lib.md") ||
			strings.Contains(f, "dep.md") || strings.Contains(f, "build.md") {
			t.Errorf("should not include files from excluded dirs: %s", f)
		}
	}

	if !hasRoot {
		t.Error("root.md should be included")
	}
	if !hasClaude {
		t.Error("feature.md from .claude folder should be included (whitelisted)")
	}
	if !hasDoc {
		t.Error("doc.md should be included")
	}
}

// TestCollectMarkdownFiles_SortedOutput tests that files are returned sorted
func TestCollectMarkdownFiles_SortedOutput(t *testing.T) {
	testDir := t.TempDir()

	// Create files in random order
	files := []string{"zebra.md", "alpha.md", "beta.md"}
	for _, f := range files {
		os.WriteFile(filepath.Join(testDir, f), []byte("# Test"), 0644)
	}

	result := collectMarkdownFiles(testDir)

	if len(result) != 3 {
		t.Fatalf("expected 3 files, got %d", len(result))
	}

	// Verify sorted order
	for i := 0; i < len(result)-1; i++ {
		if result[i] > result[i+1] {
			t.Errorf("files not sorted: %v", result)
			break
		}
	}
}

// TestCollectMarkdownFiles_NestedStructure tests deep directory hierarchies
func TestCollectMarkdownFiles_NestedStructure(t *testing.T) {
	testDir := t.TempDir()

	// Create nested structure
	os.MkdirAll(filepath.Join(testDir, "a", "b", "c"), 0755)
	os.WriteFile(filepath.Join(testDir, "root.md"), []byte("# Root"), 0644)
	os.WriteFile(filepath.Join(testDir, "a", "level1.md"), []byte("# Level1"), 0644)
	os.WriteFile(filepath.Join(testDir, "a", "b", "level2.md"), []byte("# Level2"), 0644)
	os.WriteFile(filepath.Join(testDir, "a", "b", "c", "level3.md"), []byte("# Level3"), 0644)

	files := collectMarkdownFiles(testDir)

	if len(files) != 4 {
		t.Errorf("expected 4 files, got %d: %v", len(files), files)
	}

	// Verify all levels are found
	levels := map[string]bool{
		"root.md":   false,
		"level1.md": false,
		"level2.md": false,
		"level3.md": false,
	}

	for _, f := range files {
		for level := range levels {
			if strings.Contains(f, level) {
				levels[level] = true
			}
		}
	}

	for level, found := range levels {
		if !found {
			t.Errorf("%s not found in collected files", level)
		}
	}
}

// TestCollectMarkdownFiles_EmptyDirectory tests empty directory handling
func TestCollectMarkdownFiles_EmptyDirectory(t *testing.T) {
	testDir := t.TempDir()

	files := collectMarkdownFiles(testDir)

	if len(files) != 0 {
		t.Errorf("expected 0 files in empty directory, got %d", len(files))
	}
}

// TestCollectMarkdownFiles_OnlyNonMarkdown tests non-.md files are ignored
func TestCollectMarkdownFiles_OnlyNonMarkdown(t *testing.T) {
	testDir := t.TempDir()

	// Create non-markdown files
	os.WriteFile(filepath.Join(testDir, "test.txt"), []byte("text"), 0644)
	os.WriteFile(filepath.Join(testDir, "test.html"), []byte("html"), 0644)
	os.WriteFile(filepath.Join(testDir, "test.go"), []byte("go"), 0644)

	files := collectMarkdownFiles(testDir)

	if len(files) != 0 {
		t.Errorf("expected 0 markdown files, got %d: %v", len(files), files)
	}
}

// TestCollectMarkdownFiles_CaseInsensitive tests .MD, .md, .Md extensions
func TestCollectMarkdownFiles_CaseInsensitive(t *testing.T) {
	testDir := t.TempDir()

	// Create files with different case extensions
	os.WriteFile(filepath.Join(testDir, "lower.md"), []byte("# Lower"), 0644)
	os.WriteFile(filepath.Join(testDir, "upper.MD"), []byte("# Upper"), 0644)
	os.WriteFile(filepath.Join(testDir, "mixed.Md"), []byte("# Mixed"), 0644)

	files := collectMarkdownFiles(testDir)

	if len(files) != 3 {
		t.Errorf("expected 3 files (case insensitive), got %d: %v", len(files), files)
	}
}
