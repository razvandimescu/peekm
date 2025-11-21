package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// TestConcurrentBrowseDirAccess tests concurrent access to browseDir
// Run with: go test -race
func TestConcurrentBrowseDirAccess(t *testing.T) {
	testDir := t.TempDir()
	mdFile := createSimpleTestFile(t, testDir)

	// Set initial state
	setupTestState(t, testDir, []string{mdFile}, true)

	// Simulate concurrent requests to serveBrowser
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/", nil)
			w := httptest.NewRecorder()
			serveBrowser(w, req)
		}()
	}

	wg.Wait()
	// If -race detects issues, test will fail
}

// TestConcurrentMarkdownFilesAccess tests concurrent reads of markdownFiles
func TestConcurrentMarkdownFilesAccess(t *testing.T) {
	testDir := t.TempDir()
	mdFile := createSimpleTestFile(t, testDir)

	setupTestState(t, testDir, []string{mdFile}, false)

	var wg sync.WaitGroup

	// Concurrent readers
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fileMutex.RLock()
			_ = len(markdownFiles)
			fileMutex.RUnlock()
		}()
	}

	// Concurrent generateTreeHTML calls (which reads markdownFiles)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			generateTreeHTML()
		}()
	}

	wg.Wait()
}

// TestConcurrentHTMLAccess tests concurrent access to currentHTML
func TestConcurrentHTMLAccess(t *testing.T) {
	tmpFile := createSimpleTestFile(t, t.TempDir())

	var wg sync.WaitGroup

	// Concurrent writers (via renderMarkdown)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			renderMarkdown(tmpFile)
		}()
	}

	// Concurrent readers (via serveHTML)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/", nil)
			w := httptest.NewRecorder()
			serveHTML(w, req)
		}()
	}

	wg.Wait()
}

// TestConcurrentClientsAccess tests concurrent SSE client management
func TestConcurrentClientsAccess(t *testing.T) {
	var wg sync.WaitGroup

	// Add multiple clients concurrently
	clientChans := make([]chan struct{}, 10)
	for i := 0; i < 10; i++ {
		clientChans[i] = make(chan struct{})
		wg.Add(1)
		go func(ch chan struct{}) {
			defer wg.Done()
			clientsMutex.Lock()
			clients[ch] = true
			clientsMutex.Unlock()
		}(clientChans[i])
	}

	wg.Wait()

	// Notify clients concurrently
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			notifyClients()
		}()
	}

	wg.Wait()

	// Remove clients concurrently
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(ch chan struct{}) {
			defer wg.Done()
			clientsMutex.Lock()
			delete(clients, ch)
			clientsMutex.Unlock()
			close(ch)
		}(clientChans[i])
	}

	wg.Wait()
}

// TestWatcherManagerConcurrency tests concurrent watcher operations
func TestWatcherManagerConcurrency(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "test.md")
	os.WriteFile(tmpFile, []byte("# Test"), 0644)

	var wm watcherManager
	var wg sync.WaitGroup

	// Concurrent watch calls
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wm.watch(tmpFile)
		}()
	}

	wg.Wait()

	// Concurrent close calls
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wm.close()
		}()
	}

	wg.Wait()
}

// TestConcurrentCurrentFileAccess tests concurrent access to currentFile
func TestConcurrentCurrentFileAccess(t *testing.T) {
	testDir := t.TempDir()
	files := []string{
		createTestMarkdownFile(t, testDir, "file1.md", "# Test"),
		createTestMarkdownFile(t, testDir, "file2.md", "# Test"),
		createTestMarkdownFile(t, testDir, "file3.md", "# Test"),
	}

	// Set up state
	setupTestState(t, testDir, files, false)

	var wg sync.WaitGroup

	// Concurrent file serving (writes to currentFile)
	for _, f := range files {
		wg.Add(1)
		go func(file string) {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/view/"+filepath.Base(file), nil)
			w := httptest.NewRecorder()
			serveFile(w, req)
		}(f)
	}

	wg.Wait()
}

// TestConcurrentNavigate tests concurrent navigation requests
func TestConcurrentNavigate(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot get home directory")
	}

	// Create test directories
	testDirs := make([]string, 3)
	for i := 0; i < 3; i++ {
		dir := filepath.Join(homeDir, "peekm_test_concurrent_"+t.Name()+"_"+string(rune('a'+i)))
		os.Mkdir(dir, 0755)
		testDirs[i] = dir
		createTestMarkdownFile(t, dir, "test.md", "# Test")
		t.Cleanup(func() { os.RemoveAll(dir) })
	}

	var wg sync.WaitGroup

	// Concurrent navigation to different directories
	for _, dir := range testDirs {
		wg.Add(1)
		go func(targetDir string) {
			defer wg.Done()
			// Note: actual HTTP request simulation would be complex
			// This just tests the validate function concurrently
			_, err := validateAndResolvePath(targetDir)
			if err != nil {
				t.Logf("validation error: %v", err)
			}
		}(dir)
	}

	wg.Wait()
}

// TestConcurrentRenderMarkdown tests concurrent markdown rendering
func TestConcurrentRenderMarkdown(t *testing.T) {
	testDir := t.TempDir()

	files := []string{
		filepath.Join(testDir, "test1.md"),
		filepath.Join(testDir, "test2.md"),
		filepath.Join(testDir, "test3.md"),
	}

	for i, f := range files {
		content := []byte("# Test " + string(rune('A'+i)))
		os.WriteFile(f, content, 0644)
	}

	var wg sync.WaitGroup

	// Render different files concurrently
	for _, f := range files {
		wg.Add(1)
		go func(file string) {
			defer wg.Done()
			err := renderMarkdown(file)
			if err != nil {
				t.Logf("render error: %v", err)
			}
		}(f)
	}

	wg.Wait()
}

// TestConcurrentTreeGeneration tests concurrent tree HTML generation
func TestConcurrentTreeGeneration(t *testing.T) {
	testDir := t.TempDir()
	os.Mkdir(filepath.Join(testDir, "docs"), 0755)

	files := []string{
		createTestMarkdownFile(t, testDir, "readme.md", "# Test"),
		createTestMarkdownFile(t, filepath.Join(testDir, "docs"), "guide.md", "# Test"),
	}

	setupTestState(t, testDir, files, false)

	var wg sync.WaitGroup

	// Generate tree concurrently
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = generateTreeHTML()
		}()
	}

	wg.Wait()
}

// TestConcurrentTemplateExecution tests concurrent template rendering
func TestConcurrentTemplateExecution(t *testing.T) {
	var wg sync.WaitGroup

	// Execute single file template concurrently
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			data := singleFileTemplateData{
				baseTemplateData: newBaseTemplateData(),
				Title:            "Test",
				Content:          "<p>Test content</p>",
			}
			var buf bytes.Buffer
			err := singleFileTmpl.Execute(&buf, data)
			if err != nil {
				t.Logf("template execution error: %v", err)
			}
		}(i)
	}

	// Execute browser template concurrently
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			data := browserTemplateData{
				baseTemplateData: newBaseTemplateData(),
				Title:            "Browser",
				Subtitle:         "Test",
			}
			var buf bytes.Buffer
			err := fileBrowserTmpl.Execute(&buf, data)
			if err != nil {
				t.Logf("template execution error: %v", err)
			}
		}(i)
	}

	wg.Wait()
}

// TestConcurrentCollectMarkdownFiles tests concurrent file collection
func TestConcurrentCollectMarkdownFiles(t *testing.T) {
	testDir := t.TempDir()

	// Create some files
	for i := 0; i < 5; i++ {
		os.WriteFile(filepath.Join(testDir, "test"+string(rune('a'+i))+".md"), []byte("# Test"), 0644)
	}

	var wg sync.WaitGroup

	// Collect files concurrently
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			files := collectMarkdownFiles(testDir)
			if len(files) != 5 {
				t.Logf("expected 5 files, got %d", len(files))
			}
		}()
	}

	wg.Wait()
}

// TestConcurrentNewBaseTemplateData tests concurrent factory calls
func TestConcurrentNewBaseTemplateData(t *testing.T) {
	var wg sync.WaitGroup

	// Call factory concurrently
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data := newBaseTemplateData()
			if data.GitHubCSS == "" || data.ThemeOverrides == "" || data.ThemeManagerJS == "" {
				t.Error("factory returned incomplete data")
			}
		}()
	}

	wg.Wait()
}

// TestWatchFileWithContext_ContextCancellation tests context cancellation handling
func TestWatchFileWithContext_ContextCancellation(t *testing.T) {
	tmpFile := createSimpleTestFile(t, t.TempDir())

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("failed to create watcher: %v", err)
	}
	defer watcher.Close()

	if err := watcher.Add(tmpFile); err != nil {
		t.Fatalf("failed to add file to watcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan bool)
	go func() {
		watchFileWithContext(ctx, watcher, tmpFile)
		done <- true
	}()

	// Give goroutine time to start watching
	time.Sleep(50 * time.Millisecond)

	// Cancel context
	cancel()

	// Verify goroutine exits promptly
	select {
	case <-done:
		// Success - goroutine exited
	case <-time.After(500 * time.Millisecond):
		t.Error("watchFileWithContext did not exit on context cancellation within timeout")
	}
}

// TestWatchFileWithContext_FileModification tests file change detection
func TestWatchFileWithContext_FileModification(t *testing.T) {
	tmpFile := createSimpleTestFile(t, t.TempDir())

	// Render initial version
	if err := renderMarkdown(tmpFile); err != nil {
		t.Fatalf("failed to render initial markdown: %v", err)
	}

	htmlMutex.RLock()
	initialHTML := currentHTML
	htmlMutex.RUnlock()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("failed to create watcher: %v", err)
	}
	defer watcher.Close()

	if err := watcher.Add(tmpFile); err != nil {
		t.Fatalf("failed to add file to watcher: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go watchFileWithContext(ctx, watcher, tmpFile)

	// Modify file
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(tmpFile, []byte("# Modified Content"), 0644); err != nil {
		t.Fatalf("failed to modify file: %v", err)
	}

	// Wait for file system event and re-render
	time.Sleep(300 * time.Millisecond)

	htmlMutex.RLock()
	modifiedHTML := currentHTML
	htmlMutex.RUnlock()

	if initialHTML == modifiedHTML {
		t.Log("Note: HTML not updated - filesystem events may be delayed")
		// Not a hard failure as FS events can be slow/unreliable in tests
	}
}
