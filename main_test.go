package main

import (
	"bytes"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderMarkdown tests markdown rendering with GFM features
func TestRenderMarkdown(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantContain []string
		wantNotContain []string
	}{
		{
			name:    "basic markdown",
			content: "# Hello\n\nThis is **bold**.",
			wantContain: []string{
				"<h1",
				"Hello",
				"<strong>bold</strong>",
			},
		},
		{
			name:    "GFM table",
			content: "| A | B |\n|---|---|\n| 1 | 2 |",
			wantContain: []string{
				"<table",
				"<th>A</th>",
				"<th>B</th>",
				"<td>1</td>",
				"<td>2</td>",
			},
		},
		{
			name:    "code block with language",
			content: "```go\nfunc main() {}\n```",
			wantContain: []string{
				"func",
				"main",
			},
		},
		{
			name:    "autolink",
			content: "https://example.com",
			wantContain: []string{
				"<a",
				"https://example.com",
			},
		},
		{
			name:    "heading with auto ID",
			content: "# Test Heading",
			wantContain: []string{
				"<h1",
				"id=",
				"Test Heading",
			},
		},
		{
			name:    "strikethrough (GFM)",
			content: "~~deleted~~",
			wantContain: []string{
				"<del>deleted</del>",
			},
		},
		{
			name:    "task list (GFM)",
			content: "- [x] Done\n- [ ] Todo",
			wantContain: []string{
				"checkbox",
				"checked",
			},
		},
		{
			name:    "typographer features",
			content: "This is a \"quote\" and --- a dash.",
			wantContain: []string{
				// Typographer converts -- to en-dash and --- to em-dash
				// and "quotes" to curly quotes, but let's just verify the content renders
				"quote",
				"dash",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpFile := filepath.Join(t.TempDir(), "test.md")
			err := os.WriteFile(tmpFile, []byte(tt.content), 0644)
			if err != nil {
				t.Fatalf("failed to write test file: %v", err)
			}

			err = renderMarkdown(tmpFile)
			if err != nil {
				t.Fatalf("renderMarkdown failed: %v", err)
			}

			htmlMutex.RLock()
			html := currentHTML
			htmlMutex.RUnlock()

			for _, want := range tt.wantContain {
				if !strings.Contains(html, want) {
					t.Errorf("HTML does not contain %q\nHTML: %s", want, html)
				}
			}

			for _, notWant := range tt.wantNotContain {
				if strings.Contains(html, notWant) {
					t.Errorf("HTML should not contain %q", notWant)
				}
			}
		})
	}
}

// TestRenderMarkdown_NonExistentFile tests error handling
func TestRenderMarkdown_NonExistentFile(t *testing.T) {
	err := renderMarkdown("/nonexistent/file.md")
	if err == nil {
		t.Error("expected error for non-existent file, got nil")
	}
}

// TestGenerateTreeHTML tests directory tree generation
func TestGenerateTreeHTML(t *testing.T) {
	// Create test structure
	testDir := t.TempDir()
	os.Mkdir(filepath.Join(testDir, "docs"), 0755)
	os.MkdirAll(filepath.Join(testDir, "docs", "sub"), 0755)

	files := []string{
		createTestMarkdownFile(t, testDir, "README.md", "# Test"),
		createTestMarkdownFile(t, filepath.Join(testDir, "docs"), "guide.md", "# Test"),
		createTestMarkdownFile(t, filepath.Join(testDir, "docs", "sub"), "deep.md", "# Test"),
	}

	// Set up state (thread-safe)
	setupTestState(t, testDir, files)

	html := generateTreeHTML()

	// Verify structure
	if !strings.Contains(html, "README.md") {
		t.Error("missing README.md")
	}
	if !strings.Contains(html, "docs/") {
		t.Error("missing docs directory")
	}
	if !strings.Contains(html, "tree-directory") {
		t.Error("missing directory styling")
	}
	if !strings.Contains(html, "tree-file") {
		t.Error("missing file styling")
	}

	// Verify collapsible at depth >= 2
	if !strings.Contains(html, "collapsed") {
		t.Error("directories should be collapsed by default at depth >= 2")
	}

	// Verify expand icon
	if !strings.Contains(html, "expand-icon") {
		t.Error("missing expand icon")
	}
}

// TestGenerateTreeHTML_EmptyFiles tests empty file list
func TestGenerateTreeHTML_EmptyFiles(t *testing.T) {
	setupTestState(t, "", []string{})

	html := generateTreeHTML()

	if html != "" {
		t.Error("expected empty HTML for empty file list")
	}
}

// TestTemplateDataTypes tests template data structures
func TestTemplateDataTypes(t *testing.T) {
	base := newBaseTemplateData()

	if base.GitHubCSS == "" {
		t.Error("GitHubCSS should not be empty")
	}
	if base.ThemeOverrides == "" {
		t.Error("ThemeOverrides should not be empty")
	}
	if base.ThemeManagerJS == "" {
		t.Error("ThemeManagerJS should not be empty")
	}

	// Test that CSS/JS are valid template types
	_ = template.CSS(base.GitHubCSS)
	_ = template.CSS(base.ThemeOverrides)
	_ = template.JS(base.ThemeManagerJS)
}

// TestSingleFileTemplateData tests single file template data embedding
func TestSingleFileTemplateData(t *testing.T) {
	base := newBaseTemplateData()

	data := singleFileTemplateData{
		baseTemplateData: base,
		Title:            "Test",
		Content:          template.HTML("<p>test</p>"),
	}

	// Verify embedded fields are accessible
	if data.GitHubCSS != base.GitHubCSS {
		t.Error("embedded GitHubCSS should match")
	}
	if data.ThemeOverrides != base.ThemeOverrides {
		t.Error("embedded ThemeOverrides should match")
	}
	if data.ThemeManagerJS != base.ThemeManagerJS {
		t.Error("embedded ThemeManagerJS should match")
	}

	// Verify specific fields
	if data.Title != "Test" {
		t.Errorf("expected Title 'Test', got %q", data.Title)
	}
	if data.Content != template.HTML("<p>test</p>") {
		t.Errorf("expected Content '<p>test</p>', got %q", data.Content)
	}
}

// TestBrowserTemplateData tests browser template data embedding
func TestBrowserTemplateData(t *testing.T) {
	base := newBaseTemplateData()

	data := browserTemplateData{
		baseTemplateData: base,
		Title:            "Browser",
		Subtitle:         "Test Subtitle",
		ShowBackButton:   true,
		Instructions:     true,
		BrowsePath:       "/test/path",
	}

	// Verify embedded fields
	if data.GitHubCSS != base.GitHubCSS {
		t.Error("embedded GitHubCSS should match")
	}

	// Verify specific fields
	if data.Title != "Browser" {
		t.Errorf("expected Title 'Browser', got %q", data.Title)
	}
	if data.Subtitle != "Test Subtitle" {
		t.Errorf("expected Subtitle 'Test Subtitle', got %q", data.Subtitle)
	}
	if !data.ShowBackButton {
		t.Error("expected ShowBackButton to be true")
	}
	if !data.Instructions {
		t.Error("expected Instructions to be true")
	}
	if data.BrowsePath != "/test/path" {
		t.Errorf("expected BrowsePath '/test/path', got %q", data.BrowsePath)
	}
}

// TestSingleFileTemplateExecution tests template rendering
func TestSingleFileTemplateExecution(t *testing.T) {
	base := newBaseTemplateData()

	data := singleFileTemplateData{
		baseTemplateData: base,
		Title:            "Test Document",
		Content:          template.HTML("<h1>Hello World</h1>"),
	}

	var buf bytes.Buffer
	err := singleFileTmpl.Execute(&buf, data)
	if err != nil {
		t.Fatalf("template execution failed: %v", err)
	}

	html := buf.String()

	// Verify template rendered correctly
	if !strings.Contains(html, "Test Document") {
		t.Error("title not found in output")
	}
	if !strings.Contains(html, "<h1>Hello World</h1>") {
		t.Error("content not found in output")
	}
	if !strings.Contains(html, "setTheme") {
		t.Error("theme manager JS not found")
	}
	if !strings.Contains(html, "<!DOCTYPE html>") {
		t.Error("missing DOCTYPE")
	}
	if !strings.Contains(html, "EventSource") {
		t.Error("missing SSE code")
	}
}

// TestBrowserTemplateExecution tests browser template rendering
func TestBrowserTemplateExecution(t *testing.T) {
	base := newBaseTemplateData()

	data := browserTemplateData{
		baseTemplateData: base,
		Title:            "Markdown Files",
		Subtitle:         "Found 5 files",
		TreeHTML:         template.HTML("<div>Tree content</div>"),
		Instructions:     true,
		BrowsePath:       "/home/user/docs",
	}

	var buf bytes.Buffer
	err := fileBrowserTmpl.Execute(&buf, data)
	if err != nil {
		t.Fatalf("template execution failed: %v", err)
	}

	html := buf.String()

	// Verify template rendered correctly
	if !strings.Contains(html, "Markdown Files") {
		t.Error("title not found in output")
	}
	if !strings.Contains(html, "Found 5 files") {
		t.Error("subtitle not found in output")
	}
	if !strings.Contains(html, "Tree content") {
		t.Error("tree HTML not found in output")
	}
	if !strings.Contains(html, "setTheme") {
		t.Error("theme manager JS not found")
	}
	if !strings.Contains(html, "📝 Click any file") {
		t.Error("instructions not found")
	}
	if !strings.Contains(html, "/home/user/docs") {
		t.Error("browse path not found")
	}
}

// TestNewMarkdownRenderer tests markdown renderer factory
func TestNewMarkdownRenderer(t *testing.T) {
	md := newMarkdownRenderer()
	if md == nil {
		t.Fatal("newMarkdownRenderer returned nil")
	}

	// Test that it can convert markdown
	var buf bytes.Buffer
	err := md.Convert([]byte("# Test\n\nHello **world**"), &buf)
	if err != nil {
		t.Errorf("markdown conversion failed: %v", err)
	}

	html := buf.String()
	if !strings.Contains(html, "<h1") {
		t.Error("markdown not converted to HTML")
	}
	if !strings.Contains(html, "<strong>world</strong>") {
		t.Error("bold not converted")
	}
}

// TestFormatSize tests file size formatting
func TestFormatSize(t *testing.T) {
	tests := []struct {
		size int64
		want string
	}{
		{0, "(0 bytes)"},
		{100, "(100 bytes)"},
		{1023, "(1023 bytes)"},
		{1024, "(1 KB)"},
		{2048, "(2 KB)"},
		{1024 * 1024, "(1024 KB)"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatSize(tt.size)
			if got != tt.want {
				t.Errorf("formatSize(%d) = %q, want %q", tt.size, got, tt.want)
			}
		})
	}
}

// Note: fileNode, cleanEmptyDirs, and sortTree are internal implementation details
// and are tested indirectly through generateTreeHTML tests

// TestGenerateTreeHTML_StaleFiles tests handling of non-existent files (REGRESSION TEST)
func TestGenerateTreeHTML_StaleFiles(t *testing.T) {
	// This reproduces the panic when markdownFiles contains stale paths
	// Scenario: User navigates to different directory, but markdownFiles has old paths

	testDir := t.TempDir()

	// Create a file that we'll reference but won't exist
	nonExistentFile := filepath.Join(testDir, "deleted.md")
	existingFile := createSimpleTestFile(t, testDir)

	// Set up state with a non-existent file in the list (simulating stale state)
	setupTestState(t, testDir, []string{nonExistentFile, existingFile})

	// This should NOT panic even with non-existent files
	html := generateTreeHTML()

	// Should only contain the existing file
	if strings.Contains(html, "deleted.md") {
		t.Error("tree should not contain non-existent files")
	}
	if !strings.Contains(html, "test.md") {
		t.Error("tree should contain existing file")
	}
}

// TestFileExists tests the fileExists helper function
func TestFileExists(t *testing.T) {
	testDir := t.TempDir()

	// Create an existing file
	existingFile := createSimpleTestFile(t, testDir)

	// Test existing file
	if !fileExists(existingFile) {
		t.Error("fileExists should return true for existing file")
	}

	// Test non-existent file
	nonExistentFile := filepath.Join(testDir, "nonexistent.md")
	if fileExists(nonExistentFile) {
		t.Error("fileExists should return false for non-existent file")
	}

	// Test directory
	if !fileExists(testDir) {
		t.Error("fileExists should return true for existing directory")
	}
}
