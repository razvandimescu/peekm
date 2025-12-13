package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/fsnotify/fsnotify"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

//go:embed theme/*
var themeFS embed.FS

var (
	// Build info (set via ldflags)
	version = "dev"
	commit  = "none"
	date    = "unknown"

	// Hardcoded directory exclusions (common build artifacts and dependencies)
	hardcodedExclusions = []string{
		"node_modules", // Node.js dependencies
		"vendor",       // Go dependencies
		"dist",         // Build output
		"venv",         // Python virtual environment
		"env",          // Python virtual environment (alternative name)
		"virtualenv",   // Python virtual environment (alternative name)
	}

	// Map version for O(1) lookup performance
	hardcodedExclusionsMap = map[string]bool{
		"node_modules": true,
		"vendor":       true,
		"dist":         true,
		"venv":         true,
		"env":          true,
		"virtualenv":   true,
	}

	// Flags
	port            = flag.Int("port", 6419, "Port to serve on")
	openBrowser     = flag.Bool("browser", true, "Open browser automatically")
	showVersion     = flag.Bool("version", false, "Show version information")
	showIgnored     = flag.Bool("show-ignored", false, "Show all excluded directories and exit")
	enableClaudeHook = flag.Bool("hook-claude-code", false, "Enable Claude Code hook integration for session tracking")

	// State (global for single-user CLI simplicity; protected by mutexes)
	clients      = make(map[chan string]bool)
	clientsMutex sync.RWMutex

	// Browser mode (always active)
	markdownFiles []string
	currentFile   string
	fileMutex     sync.RWMutex
	browseDir     string
	fileWatcher   watcherManager
	dirWatcher    watcherManager


	// Ignore pattern cache (reduces file I/O on navigation)
	globalIgnoreCache struct {
		rootDir  string
		patterns []string
		mu       sync.RWMutex
	}

	// Templates, CSS, and JavaScript (loaded once at startup)
	githubCSS              string
	themeOverrides         string
	themeManagerJS         string
	editorJS               string
	navigationJS           string
	fileBrowserTmpl        *template.Template
	fileBrowserPartialTmpl *template.Template

	// SSE event replay buffer (50 events = ~2 min of AI file creation)
	globalEventBuffer = newEventBuffer(50)

	// Claude Code session tracking (5s TTL for hook-to-fsnotify correlation)
	globalSessionStore *sessionStore
)

// watcherManager manages file watching with proper cleanup
type watcherManager struct {
	mu      sync.Mutex
	current *fsnotify.Watcher
	cancel  context.CancelFunc
}

// baseTemplateData contains common fields for all templates
type baseTemplateData struct {
	GitHubCSS      template.CSS
	ThemeOverrides template.CSS
	ThemeManagerJS template.JS
	EditorJS       template.JS
	NavigationJS   template.JS
}

// browserTemplateData is used for rendering the file browser and file views
type browserTemplateData struct {
	baseTemplateData
	Title          string
	Subtitle       string
	TreeHTML       template.HTML
	ShowBackButton bool
	Content        template.HTML
	BrowsePath     string
	SessionData    *SessionMetadata // Claude Code session info for this file
}

// fileEventMessage is used for SSE notifications about file changes
type fileEventMessage struct {
	Type    string `json:"type"`              // "file_added" or "file_removed"
	Path    string `json:"path"`
	Session string `json:"session,omitempty"` // Optional Claude Code session ID
}

// connectionStatusMessage is used for SSE notifications about connection status
type connectionStatusMessage struct {
	Type  string `json:"type"`  // "connection_status"
	Count int    `json:"count"` // Number of active connections
}

// eventRecord stores a single SSE event with ID for replay
type eventRecord struct {
	id   string // Monotonic counter
	data string // JSON message
}

// eventBuffer maintains a circular buffer of recent events for SSE replay
type eventBuffer struct {
	mu      sync.RWMutex
	events  []eventRecord
	counter uint64
	maxSize int
}

// newEventBuffer creates an eventBuffer with specified capacity
func newEventBuffer(maxSize int) *eventBuffer {
	return &eventBuffer{
		events:  make([]eventRecord, 0, maxSize),
		maxSize: maxSize,
	}
}

// add assigns an event ID, stores the event, and returns the ID
func (eb *eventBuffer) add(data string) string {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	eb.counter++
	id := fmt.Sprintf("%d", eb.counter)

	evt := eventRecord{
		id:   id,
		data: data,
	}

	// Circular buffer: if at capacity, remove oldest
	if len(eb.events) >= eb.maxSize {
		eb.events = eb.events[1:]
	}
	eb.events = append(eb.events, evt)

	return id
}

// getAfter returns all events after the specified ID
func (eb *eventBuffer) getAfter(lastID string) []eventRecord {
	eb.mu.RLock()
	defer eb.mu.RUnlock()

	var result []eventRecord
	foundLast := false

	for _, evt := range eb.events {
		if foundLast {
			result = append(result, evt)
		}
		if evt.id == lastID {
			foundLast = true
		}
	}

	return result
}

// sessionMapping tracks which Claude Code session last modified a file
type sessionMapping struct {
	metadata  *SessionMetadata
	timestamp time.Time
}

// SessionMetadata contains complete Claude Code session information
type SessionMetadata struct {
	SessionID      string    `json:"session_id"`
	ToolName       string    `json:"tool_name"`
	PermissionMode string    `json:"permission_mode,omitempty"`
	ToolUseID      string    `json:"tool_use_id,omitempty"`
	CWD            string    `json:"cwd,omitempty"`
	TranscriptPath string    `json:"transcript_path,omitempty"`
	Timestamp      time.Time `json:"timestamp"`
}

// sessionStore maintains persistent mapping of file paths to session metadata
type sessionStore struct {
	mu       sync.RWMutex
	mappings map[string]sessionMapping
	ttl      time.Duration // Kept for potential future cleanup, but not used for expiration
}

// newSessionStore creates a session store (session data persists indefinitely)
func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{
		mappings: make(map[string]sessionMapping),
		ttl:      ttl,
	}
}

// register stores session metadata for a file path with current timestamp
func (ss *sessionStore) register(filePath string, metadata *SessionMetadata) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	ss.mappings[filePath] = sessionMapping{
		metadata:  metadata,
		timestamp: time.Now(),
	}
}

// get retrieves session metadata for a file path (persists indefinitely)
func (ss *sessionStore) get(filePath string) (*SessionMetadata, bool) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	mapping, exists := ss.mappings[filePath]
	if !exists {
		return nil, false
	}

	return mapping.metadata, true
}

// newBaseTemplateData creates a baseTemplateData with embedded resources
func newBaseTemplateData() baseTemplateData {
	return baseTemplateData{
		GitHubCSS:      template.CSS(githubCSS),
		ThemeOverrides: template.CSS(themeOverrides),
		ThemeManagerJS: template.JS(themeManagerJS),
		EditorJS:       template.JS(editorJS),
		NavigationJS:   template.JS(navigationJS),
	}
}

func (m *watcherManager) watch(filePath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Stop existing watcher
	if m.cancel != nil {
		m.cancel()
	}
	if m.current != nil {
		m.current.Close()
	}

	// Start new watcher
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	m.current = watcher

	if err := watcher.Add(filePath); err != nil {
		if closeErr := watcher.Close(); closeErr != nil {
			log.Printf("Failed to close watcher after add error: %v", closeErr)
		}
		cancel()
		return err
	}

	go watchFileWithContext(ctx, watcher, filePath)
	return nil
}

func (m *watcherManager) watchDirectory(rootDir string) error {
	m.mu.Lock()

	// Stop existing watcher (under lock)
	if m.cancel != nil {
		m.cancel()
	}
	if m.current != nil {
		m.current.Close()
	}

	// Start new watcher
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	m.current = watcher

	// Add root directory
	if err := watcher.Add(rootDir); err != nil {
		if closeErr := watcher.Close(); closeErr != nil {
			log.Printf("Failed to close watcher after add error: %v", closeErr)
		}
		cancel()
		m.current = nil
		m.cancel = nil
		m.mu.Unlock()
		return err
	}

	// Unlock before slow directory walk
	m.mu.Unlock()

	// Collect directories to watch (without lock to avoid blocking on large trees)
	dirsToWatch, err := m.collectDirectories(rootDir)
	if err != nil {
		m.mu.Lock()
		// Clean up if we still own this watcher
		if m.current == watcher {
			if closeErr := watcher.Close(); closeErr != nil {
				log.Printf("Failed to close watcher after directory walk error: %v", closeErr)
			}
			cancel()
			m.current = nil
			m.cancel = nil
		}
		m.mu.Unlock()
		return fmt.Errorf("directory walk failed: %w", err)
	}

	// Re-acquire lock to finish setup
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if watcher was replaced during walk
	if m.current != watcher {
		// Another call won the race, abandon this setup
		if closeErr := watcher.Close(); closeErr != nil {
			log.Printf("Failed to close abandoned watcher: %v", closeErr)
		}
		cancel()
		return fmt.Errorf("watcher setup cancelled (replaced during walk)")
	}

	// Add directories (holding lock)
	for _, dir := range dirsToWatch {
		if err := watcher.Add(dir); err != nil {
			log.Printf("Warning: Cannot watch directory %s: %v", dir, err)
		}
	}

	go watchDirectoryWithContext(ctx, watcher)
	return nil
}

// collectDirectories walks the directory tree and returns paths to watch
func (m *watcherManager) collectDirectories(rootDir string) ([]string, error) {
	var dirsToWatch []string
	homeDir, _ := os.UserHomeDir()

	err := filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Security: Skip symlinks outside $HOME
		resolvedInfo, shouldSkip, err := validateSymlinkSecurity(path, info, homeDir)
		if shouldSkip {
			return filepath.SkipDir
		}
		if err != nil {
			return nil
		}
		if resolvedInfo != nil {
			info = resolvedInfo
		}

		// Skip hidden and excluded directories
		if info.IsDir() {
			name := info.Name()
			// Skip hidden dirs EXCEPT .claude (for AI workflows)
			if strings.HasPrefix(name, ".") && path != rootDir && name != ".claude" {
				return filepath.SkipDir
			}
			// Skip hardcoded exclusions (O(1) map lookup)
			if isHardcodedExclusion(name) {
				return filepath.SkipDir
			}

			if path != rootDir {
				dirsToWatch = append(dirsToWatch, path)
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return dirsToWatch, nil
}

func (m *watcherManager) close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cancel != nil {
		m.cancel()
	}
	if m.current != nil {
		m.current.Close()
	}
}

// newMarkdownRenderer creates a configured goldmark renderer
func newMarkdownRenderer() goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			extension.Typographer,
			highlighting.NewHighlighting(
				highlighting.WithFormatOptions(
					chromahtml.WithClasses(true),
				),
			),
		),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
		),
		goldmark.WithRendererOptions(
			html.WithUnsafe(),
		),
	)
}

// withRecovery wraps an HTTP handler with panic recovery
func withRecovery(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("PANIC: %v\n%s", err, debug.Stack())
				http.Error(w, "Internal server error", http.StatusInternalServerError)
			}
		}()
		next(w, r)
	}
}

// registerRoutes registers all HTTP routes
func registerRoutes() {
	http.HandleFunc("/", withRecovery(serveBrowser))
	http.HandleFunc("/view/", withRecovery(serveFile))
	http.HandleFunc("/navigate", withRecovery(handleNavigate))
	http.HandleFunc("/delete", withRecovery(handleDelete))
	http.HandleFunc("/raw/", withRecovery(serveRaw))
	http.HandleFunc("/save", withRecovery(handleSave))
	http.HandleFunc("/download", withRecovery(handleDownload))
	http.HandleFunc("/events", withRecovery(serveSSE))
	http.HandleFunc("/tree-html", withRecovery(serveTreeHTML))

	// Register Claude Code hook endpoint if enabled
	if *enableClaudeHook {
		http.HandleFunc("/hook/file-modified", withRecovery(handleClaudeHook))
		log.Println("Registered Claude Code hook endpoint: /hook/file-modified")
	}
}

// validateSymlinkSecurity checks if a symlink is safe to follow
// Returns the resolved FileInfo and whether to skip (for directories)
func validateSymlinkSecurity(path string, info os.FileInfo, homeDir string) (os.FileInfo, bool, error) {
	if info.Mode()&os.ModeSymlink == 0 {
		return info, false, nil // Not a symlink, OK to proceed
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		log.Printf("Warning: Skipping unresolvable symlink: %s", path)
		return nil, info.IsDir(), err
	}

	// Check if resolved path is within $HOME
	if homeDir != "" && !strings.HasPrefix(resolved, homeDir) {
		log.Printf("Security: Skipping symlink outside home directory: %s -> %s", path, resolved)
		return nil, info.IsDir(), fmt.Errorf("symlink outside home")
	}

	// Update info to reflect the resolved target
	resolvedInfo, err := os.Stat(resolved)
	if err != nil {
		log.Printf("Warning: Cannot stat symlink target: %s", resolved)
		return nil, info.IsDir(), err
	}

	return resolvedInfo, false, nil
}

// validateAndResolvePath validates and resolves a path with security checks
// Returns the validated absolute path or an error if validation fails
// isPartialRequest detects if the request is an AJAX/fetch request for partial content
func isPartialRequest(r *http.Request) bool {
	return r.Header.Get("X-Requested-With") == "XMLHttpRequest"
}

func validateAndResolvePath(targetPath string) (string, error) {
	// Expand ~ to home directory
	if strings.HasPrefix(targetPath, "~/") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		targetPath = filepath.Join(homeDir, targetPath[2:])
	} else if targetPath == "~" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		targetPath = homeDir
	}

	// Clean the path to prevent traversal
	targetPath = filepath.Clean(targetPath)

	// Make absolute if relative
	if !filepath.IsAbs(targetPath) {
		absPath, err := filepath.Abs(targetPath)
		if err != nil {
			return "", fmt.Errorf("invalid path: %w", err)
		}
		targetPath = absPath
	}

	// Resolve symlinks
	resolvedPath, err := filepath.EvalSymlinks(targetPath)
	if err != nil {
		return "", fmt.Errorf("path does not exist: %w", err)
	}
	targetPath = resolvedPath

	// Security: Restrict to $HOME directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	if !strings.HasPrefix(targetPath, homeDir) {
		return "", fmt.Errorf("access denied: path must be within home directory")
	}

	return targetPath, nil
}

// resolveFilePath converts a relative file path to absolute using browseDir
// Thread-safe helper to eliminate duplication across handlers
func resolveFilePath(relativePath string) string {
	// Get current browse directory (thread-safe)
	fileMutex.RLock()
	currentBrowseDir := browseDir
	fileMutex.RUnlock()

	// Convert relative path to absolute by joining with browseDir
	var absFilePath string
	if filepath.IsAbs(relativePath) {
		absFilePath = relativePath
	} else {
		absFilePath = filepath.Join(currentBrowseDir, relativePath)
	}

	// Clean the absolute path
	return filepath.Clean(absFilePath)
}

func init() {
	// Load CSS files
	cssData, err := themeFS.ReadFile("theme/github-markdown.css")
	if err != nil {
		log.Fatalf("Failed to load GitHub CSS: %v", err)
	}
	githubCSS = string(cssData)

	overridesData, err := themeFS.ReadFile("theme/theme-overrides.css")
	if err != nil {
		log.Fatalf("Failed to load theme overrides CSS: %v", err)
	}
	themeOverrides = string(overridesData)

	// Load JavaScript files
	themeManagerData, err := themeFS.ReadFile("theme/theme-manager.js")
	if err != nil {
		log.Fatalf("Failed to load theme manager JS: %v", err)
	}
	themeManagerJS = string(themeManagerData)

	editorData, err := themeFS.ReadFile("theme/editor.js")
	if err != nil {
		log.Fatalf("Failed to load editor JS: %v", err)
	}
	editorJS = string(editorData)

	navigationData, err := themeFS.ReadFile("theme/navigation.js")
	if err != nil {
		log.Fatalf("Failed to load navigation JS: %v", err)
	}
	navigationJS = string(navigationData)

	// Load HTML templates with custom functions
	funcMap := template.FuncMap{
		"formatISO": func(t time.Time) string {
			return t.Format(time.RFC3339)
		},
	}

	fileBrowserHTML, err := themeFS.ReadFile("theme/file-browser.html")
	if err != nil {
		log.Fatalf("Failed to load file-browser template: %v", err)
	}
	fileBrowserTmpl = template.Must(template.New("file-browser").Funcs(funcMap).Parse(string(fileBrowserHTML)))

	fileBrowserPartialHTML, err := themeFS.ReadFile("theme/file-browser-partial.html")
	if err != nil {
		log.Fatalf("Failed to load file-browser-partial template: %v", err)
	}
	fileBrowserPartialTmpl = template.Must(template.New("file-browser-partial").Funcs(funcMap).Parse(string(fileBrowserPartialHTML)))
}

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Printf("peekm %s (commit: %s, built: %s)\n", version, commit, date)
		os.Exit(0)
	}

	if *showIgnored {
		fmt.Println("Hardcoded exclusions:")
		fmt.Println("  .* (hidden directories, except .claude)")
		for _, dir := range hardcodedExclusions {
			fmt.Printf("  %s\n", dir)
		}

		// Determine directory to check for .peekmignore
		checkDir := "."
		if flag.NArg() > 0 {
			checkDir = flag.Arg(0)
		}

		// Try to resolve to absolute path
		absPath, err := filepath.Abs(checkDir)
		if err == nil {
			checkDir = absPath
		}

		// If it's a file, check parent directory
		if info, err := os.Stat(checkDir); err == nil && !info.IsDir() {
			checkDir = filepath.Dir(checkDir)
		}

		// Load .peekmignore if present (uses caching)
		if patterns := getIgnorePatterns(checkDir); len(patterns) > 0 {
			fmt.Printf("\nCustom exclusions (.peekmignore in %s):\n", checkDir)
			for _, p := range patterns {
				fmt.Printf("  %s\n", p)
			}
		} else {
			fmt.Printf("\nNo .peekmignore file found in %s\n", checkDir)
		}
		os.Exit(0)
	}

	// Initialize Claude Code session tracking if enabled
	if *enableClaudeHook {
		globalSessionStore = newSessionStore(5 * time.Second)
		log.Println("Claude Code hook integration enabled (session data persists indefinitely)")

		// Note: No cleanup goroutine - sessions persist indefinitely in memory
	}

	// Determine target path (default to current directory)
	targetPath := "."
	var targetFile string // Specific file to auto-navigate to

	if flag.NArg() > 0 {
		targetPath = flag.Arg(0)
	}

	// Resolve path
	absPath, err := filepath.Abs(targetPath)
	if err != nil {
		log.Fatalf("Error getting absolute path: %v", err)
	}

	info, err := os.Stat(absPath)
	if os.IsNotExist(err) {
		log.Fatalf("Path not found: %s", targetPath)
	}
	if err != nil {
		log.Fatalf("Error accessing path: %v", err)
	}

	// Determine browse directory and target file
	if info.IsDir() {
		browseDir = absPath
	} else {
		// File argument - browse parent directory and remember file for auto-navigation
		browseDir = filepath.Dir(absPath)
		targetFile = filepath.Base(absPath)
	}

	// Collect markdown files
	markdownFiles = collectMarkdownFiles(browseDir)
	if len(markdownFiles) == 0 {
		fmt.Printf("No markdown files found in: %s\n", browseDir)
		fmt.Println("\nUsage: peekm [options] <markdown-file|directory>")
		fmt.Println("\nOptions:")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Watch for new markdown files
	if err := dirWatcher.watchDirectory(browseDir); err != nil {
		log.Printf("Warning: Cannot watch directory for changes: %v", err)
	}

	// Register all routes
	registerRoutes()

	addr := fmt.Sprintf("localhost:%d", *port)
	url := fmt.Sprintf("http://%s", addr)

	// Build URL with auto-navigation if specific file requested
	fullURL := url
	if targetFile != "" {
		// Get relative path for URL
		for _, mdFile := range markdownFiles {
			if filepath.Base(mdFile) == targetFile {
				relPath, err := filepath.Rel(browseDir, mdFile)
				if err == nil {
					fullURL = fmt.Sprintf("%s/view/%s", url, relPath)
				}
				break
			}
		}
		fmt.Printf("peekm at %s\n", url)
		fmt.Printf("Opening %s - found %d markdown file(s)\n", targetFile, len(markdownFiles))
	} else {
		fmt.Printf("peekm file browser at %s\n", url)
		fmt.Printf("Browsing %s - found %d markdown file(s)\n", browseDir, len(markdownFiles))
	}
	fmt.Println("Press Ctrl+C to quit")

	if *openBrowser {
		go func() {
			time.Sleep(500 * time.Millisecond)
			openURL(fullURL)
		}()
	}

	// Setup graceful shutdown
	server := &http.Server{
		Addr:        addr,
		ReadTimeout: 15 * time.Second,
		// WriteTimeout intentionally omitted for SSE streaming endpoints
		// SSE connections are long-lived and should not have write timeouts
		IdleTimeout: 60 * time.Second,
	}

	// Handle shutdown signals
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigint

		log.Println("\nShutting down gracefully...")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Close watchers
		fileWatcher.close()
		dirWatcher.close()

		// Shutdown HTTP server
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("Server shutdown error: %v", err)
		}
	}()

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}


func watchFileWithContext(ctx context.Context, watcher *fsnotify.Watcher, filePath string) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Write == fsnotify.Write {
				log.Println("File modified, sending reload notification...")

				// Send file_modified event with path so client can auto-refresh if viewing this file
				msgBytes, err := json.Marshal(map[string]string{
					"type": "file_modified",
					"path": filePath,
				})
				if err != nil {
					log.Printf("Error marshaling file modified message: %v", err)
					notifyClients() // Fallback to plain reload
				} else {
					notifyClientsWithMessage(string(msgBytes))
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Watcher error: %v", err)
		}
	}
}

func watchDirectoryWithContext(ctx context.Context, watcher *fsnotify.Watcher) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			// Handle CREATE events for new files/directories
			if event.Op&fsnotify.Create == fsnotify.Create {
				// Check if it's a directory - if so, add it to watcher
				info, err := os.Stat(event.Name)
				if err == nil && info.IsDir() {
					// Security check: validate directory is within $HOME
					homeDir, _ := os.UserHomeDir()
					if homeDir != "" {
						resolved, err := filepath.EvalSymlinks(event.Name)
						if err == nil && strings.HasPrefix(resolved, homeDir) {
							// Add new directory to watcher
							if err := watcher.Add(event.Name); err != nil {
								log.Printf("Warning: Cannot watch new directory %s: %v", event.Name, err)
							} else {
								log.Printf("Now watching new directory: %s", event.Name)
							}
						}
					}
				}

				// Check if it's a markdown file
				if strings.HasSuffix(strings.ToLower(event.Name), ".md") {
					log.Printf("New markdown file created: %s", event.Name)

					// Add to whitelist immediately so the file is clickable
					fileMutex.Lock()
					markdownFiles = append(markdownFiles, event.Name)
					fileMutex.Unlock()

					// Notify clients about the new file with JSON message
					relPath := event.Name
					fileMutex.RLock()
					if browseDir != "" {
						if rel, err := filepath.Rel(browseDir, event.Name); err == nil {
							relPath = rel
						}
					}
					fileMutex.RUnlock()

				// Delay notification to wait for PostToolUse hook (up to 5s)
				go func(filePath, relPath string) {
					sessionID := ""
					if globalSessionStore != nil {
						// Poll for session registration (check every 100ms, max 5s)
						timeout := time.After(5 * time.Second)
						ticker := time.NewTicker(100 * time.Millisecond)
						defer ticker.Stop()

						for {
							select {
							case <-timeout:
								// Timeout reached, send notification with or without session
								if metadata, found := globalSessionStore.get(filePath); found {
									sessionID = metadata.SessionID
								}
								goto sendNotification
							case <-ticker.C:
								// Check if session has been registered
								if metadata, found := globalSessionStore.get(filePath); found {
									sessionID = metadata.SessionID
									goto sendNotification
								}
							}
						}
					}

				sendNotification:
					// Use json.Marshal for proper escaping (prevents JSON injection)
					msg := fileEventMessage{
						Type:    "file_added",
						Path:    relPath,
						Session: sessionID,
					}
					msgBytes, err := json.Marshal(msg)
					if err != nil {
						log.Printf("Error marshaling file added message: %v", err)
					} else {
						notifyClientsWithMessage(string(msgBytes))
					}
				}(event.Name, relPath)

					// Note: No scheduleRefresh() needed - client handles insertion dynamically
				}
			}

			// Handle REMOVE events for deleted/moved files
			if event.Op&fsnotify.Remove == fsnotify.Remove {
				if strings.HasSuffix(strings.ToLower(event.Name), ".md") {
					log.Printf("Deleted file: %s", event.Name)

					// Remove from whitelist immediately
					fileMutex.Lock()
					for i, f := range markdownFiles {
						if f == event.Name {
							markdownFiles = append(markdownFiles[:i], markdownFiles[i+1:]...)
							break
						}
					}
					fileMutex.Unlock()

					// Notify clients about the removed file with JSON message
					relPath := event.Name
					fileMutex.RLock()
					if browseDir != "" {
						if rel, err := filepath.Rel(browseDir, event.Name); err == nil {
							relPath = rel
						}
					}
					fileMutex.RUnlock()

					// Use json.Marshal for proper escaping
					msg := fileEventMessage{
						Type: "file_removed",
						Path: relPath,
					}
					msgBytes, err := json.Marshal(msg)
					if err != nil {
						log.Printf("Error marshaling file removed message: %v", err)
					} else {
						notifyClientsWithMessage(string(msgBytes))
					}

					// Note: No scheduleRefresh() needed - client handles removal dynamically
				}
			}

			// Handle RENAME events for renamed files
			// Note: Rename generates two events - RENAME for old name, CREATE for new name
			if event.Op&fsnotify.Rename == fsnotify.Rename {
				if strings.HasSuffix(strings.ToLower(event.Name), ".md") {
					log.Printf("Renamed file: %s", event.Name)

					// Remove from whitelist (CREATE event will add it back with new name)
					fileMutex.Lock()
					for i, f := range markdownFiles {
						if f == event.Name {
							markdownFiles = append(markdownFiles[:i], markdownFiles[i+1:]...)
							break
						}
					}
					fileMutex.Unlock()

					// Notify clients about the removed file (new name will trigger CREATE event)
					relPath := event.Name
					fileMutex.RLock()
					if browseDir != "" {
						if rel, err := filepath.Rel(browseDir, event.Name); err == nil {
							relPath = rel
						}
					}
					fileMutex.RUnlock()

					msg := fileEventMessage{
						Type: "file_removed",
						Path: relPath,
					}
					msgBytes, err := json.Marshal(msg)
					if err != nil {
						log.Printf("Error marshaling file removed message: %v", err)
					} else{
						notifyClientsWithMessage(string(msgBytes))
					}

					// Note: No scheduleRefresh() needed - client handles removal dynamically
				}
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Directory watcher error: %v", err)
		}
	}
}


func serveRaw(w http.ResponseWriter, r *http.Request) {
	filePath := strings.TrimPrefix(r.URL.Path, "/raw")
	filePath = strings.TrimPrefix(filePath, "/")

	// Clean the path
	filePath = filepath.Clean(filePath)

	// Resolve to absolute path using browseDir
	absFilePath := resolveFilePath(filePath)

	validated, err := validateAndResolvePath(absFilePath)
	if err != nil {
		http.Error(w, "Invalid path", http.StatusForbidden)
		return
	}

	content, err := os.ReadFile(validated)
	if err != nil {
		http.Error(w, "Failed to read file", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if _, err := w.Write(content); err != nil {
		log.Printf("Failed to write raw file response: %v", err)
	}
}

func handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	filePath := r.FormValue("file")
	content := r.FormValue("content")

	// Clean the path and strip leading slash (web paths are relative to browse dir)
	filePath = filepath.Clean(strings.TrimPrefix(filePath, "/"))

	// Resolve to absolute path using browseDir
	absFilePath := resolveFilePath(filePath)

	validated, err := validateAndResolvePath(absFilePath)
	if err != nil {
		// Return specific error message (file doesn't exist, permission denied, etc.)
		statusCode := http.StatusForbidden
		if strings.Contains(err.Error(), "does not exist") {
			statusCode = http.StatusNotFound
		}
		http.Error(w, fmt.Sprintf("Cannot save file: %v", err), statusCode)
		return
	}

	if err := atomicWriteFile(validated, content); err != nil {
		http.Error(w, fmt.Sprintf("Failed to save: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "Saved successfully")
}

func atomicWriteFile(path, content string) error {
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, ".peek-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	defer os.Remove(tmpPath)

	if _, err := tmpFile.WriteString(content); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}

	return nil
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get currently viewed file
	fileMutex.RLock()
	filePath := currentFile
	fileMutex.RUnlock()

	if filePath == "" {
		http.Error(w, "No file currently open", http.StatusBadRequest)
		return
	}

	// Read and render markdown
	content, err := os.ReadFile(filePath)
	if err != nil {
		http.Error(w, "Failed to read file", http.StatusInternalServerError)
		return
	}

	md := newMarkdownRenderer()
	var buf bytes.Buffer
	if err := md.Convert(content, &buf); err != nil {
		http.Error(w, "Failed to render markdown", http.StatusInternalServerError)
		return
	}

	// Build self-contained HTML with inlined CSS (light theme only)
	htmlTemplate := `<!DOCTYPE html>
<html lang="en" data-color-mode="light" data-light-theme="light">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>%s</title>
    <style>
%s
    </style>
</head>
<body class="markdown-body">
    <div class="container" style="max-width: 980px; margin: 0 auto; padding: 45px;">
%s
    </div>
</body>
</html>`

	// Use light theme CSS only (from github-markdown.css)
	html := fmt.Sprintf(htmlTemplate,
		template.HTMLEscapeString(filepath.Base(filePath)),
		githubCSS,
		buf.String(),
	)

	// Set headers for download
	filename := strings.TrimSuffix(filepath.Base(filePath), ".md") + ".html"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(html)))

	if _, err := w.Write([]byte(html)); err != nil {
		log.Printf("Failed to write download response: %v", err)
	}
}

func serveTreeHTML(w http.ResponseWriter, r *http.Request) {
	// Get state snapshot (thread-safe)
	fileMutex.RLock()
	currentBrowseDir := browseDir
	fileMutex.RUnlock()

	// Generate tree HTML
	treeHTML := generateTreeHTML()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")

	if _, err := w.Write([]byte(treeHTML)); err != nil {
		log.Printf("Failed to write tree HTML response: %v", err)
	}

	log.Printf("Served tree HTML for directory: %s", currentBrowseDir)
}

func serveSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable proxy buffering

	// Verify flusher support early
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("SSE error: ResponseWriter doesn't support flushing")
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	clientChan := make(chan string, 10) // Buffer 10 events to handle bursts

	clientsMutex.Lock()
	clients[clientChan] = true
	clientCount := len(clients)
	clientsMutex.Unlock()

	// Broadcast connection status to all clients
	broadcastConnectionStatus(clientCount)

	defer func() {
		clientsMutex.Lock()
		delete(clients, clientChan)
		clientCount := len(clients)
		clientsMutex.Unlock()
		close(clientChan)

		// Broadcast updated connection status to remaining clients
		broadcastConnectionStatus(clientCount)
	}()

	// Send initial comment to establish connection
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	// Replay missed events if client reconnected with Last-Event-ID
	lastEventID := r.Header.Get("Last-Event-ID")
	if lastEventID != "" {
		log.Printf("Client reconnected with Last-Event-ID: %s", lastEventID)
		missedEvents := globalEventBuffer.getAfter(lastEventID)
		if len(missedEvents) > 0 {
			log.Printf("Replaying %d missed events", len(missedEvents))
			for _, evt := range missedEvents {
				fmt.Fprintf(w, "id: %s\ndata: %s\n\n", evt.id, evt.data)
			}
			flusher.Flush()
		} else {
			log.Printf("No missed events found after ID %s", lastEventID)
		}
	}

	// Keep connection alive (10s interval < 15s WriteTimeout to prevent disconnections)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case message := <-clientChan:
			// Message already formatted with "id: X\ndata: Y" from notifyClientsWithMessage
			if _, err := fmt.Fprintf(w, "%s\n\n", message); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func notifyClients() {
	notifyClientsWithMessage("reload")
}

func notifyClientsWithMessage(message string) {
	// Assign event ID and add to buffer for replay
	id := globalEventBuffer.add(message)

	clientsMutex.RLock()
	defer clientsMutex.RUnlock()

	// Send with SSE event ID for replay support
	formattedMsg := fmt.Sprintf("id: %s\ndata: %s", id, message)

	for clientChan := range clients {
		select {
		case clientChan <- formattedMsg:
		default:
		}
	}
}

func broadcastConnectionStatus(count int) {
	msg := connectionStatusMessage{
		Type:  "connection_status",
		Count: count,
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling connection status: %v", err)
		return
	}
	notifyClientsWithMessage(string(msgBytes))
}

func serveBrowser(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	// Get state snapshot (thread-safe)
	fileMutex.RLock()
	currentBrowseDir := browseDir
	currentMarkdownFiles := make([]string, len(markdownFiles))
	copy(currentMarkdownFiles, markdownFiles)
	fileMutex.RUnlock()

	// Generate tree HTML for sidebar
	treeHTML := generateTreeHTML()

	// Smart file selection for unified layout
	defaultFile := selectDefaultFile(currentMarkdownFiles)

	var content template.HTML
	var showBackButton bool
	var title, subtitle string

	if defaultFile != "" {
		// Render markdown content for the selected file
		markdownContent, err := os.ReadFile(defaultFile)
		if err == nil {
			md := newMarkdownRenderer()
			var buf bytes.Buffer
			if err := md.Convert(markdownContent, &buf); err == nil {
				content = template.HTML(buf.String())
				showBackButton = true
				title = filepath.Base(defaultFile)

				// Get relative path for subtitle
				relPath := defaultFile
				if rel, err := filepath.Rel(currentBrowseDir, defaultFile); err == nil {
					relPath = rel
				}
				subtitle = fmt.Sprintf("%s - %d file(s)", relPath, len(currentMarkdownFiles))
			} else {
				log.Printf("Error rendering markdown: %v", err)
			}
		} else {
			log.Printf("Error reading default file: %v", err)
		}
	}

	// If no content was rendered, show empty state
	if content == "" {
		title = "Documentation"
		subtitle = fmt.Sprintf("%s - %d file(s)", currentBrowseDir, len(currentMarkdownFiles))
	}

	data := browserTemplateData{
		baseTemplateData: newBaseTemplateData(),
		Title:            title,
		Subtitle:         subtitle,
		TreeHTML:         template.HTML(treeHTML),
		Content:          content,
		ShowBackButton:   showBackButton,
		BrowsePath:       currentBrowseDir,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Choose template based on request type (SPA partial vs full page)
	tmpl := fileBrowserTmpl
	if isPartialRequest(r) {
		tmpl = fileBrowserPartialTmpl
	}

	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Template execution error: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// handleClaudeHook receives file modification events from Claude Code hooks
func handleClaudeHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		SessionID      string `json:"session_id"`
		ToolName       string `json:"tool_name"`
		FilePath       string `json:"file_path"`
		PermissionMode string `json:"permission_mode"`
		ToolUseID      string `json:"tool_use_id"`
		CWD            string `json:"cwd"`
		TranscriptPath string `json:"transcript_path"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Validate required fields
	if req.SessionID == "" || req.FilePath == "" {
		http.Error(w, "Missing required fields: session_id and file_path", http.StatusBadRequest)
		return
	}

	// Create session metadata
	metadata := &SessionMetadata{
		SessionID:      req.SessionID,
		ToolName:       req.ToolName,
		PermissionMode: req.PermissionMode,
		ToolUseID:      req.ToolUseID,
		CWD:            req.CWD,
		TranscriptPath: req.TranscriptPath,
		Timestamp:      time.Now(),
	}

	// Register session mapping for file
	globalSessionStore.register(req.FilePath, metadata)

	// Truncate session ID for logging (first 8 chars)
	shortSession := req.SessionID
	if len(shortSession) > 8 {
		shortSession = shortSession[:8]
	}

	log.Printf("Registered Claude Code session %s for file: %s (mode: %s)", shortSession, req.FilePath, req.PermissionMode)

	w.WriteHeader(http.StatusOK)
}

func handleNavigate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path string `json:"path"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	targetPath := strings.TrimSpace(req.Path)
	if targetPath == "" {
		http.Error(w, "Path cannot be empty", http.StatusBadRequest)
		return
	}

	// Validate and resolve path with security checks
	validatedPath, err := validateAndResolvePath(targetPath)
	if err != nil {
		statusCode := http.StatusBadRequest
		if strings.Contains(err.Error(), "access denied") {
			statusCode = http.StatusForbidden
		} else if strings.Contains(err.Error(), "cannot determine home directory") {
			statusCode = http.StatusInternalServerError
		}
		http.Error(w, err.Error(), statusCode)
		return
	}
	targetPath = validatedPath

	// Check if path exists and is a directory
	info, err := os.Stat(targetPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Cannot access path: %v", err), http.StatusBadRequest)
		return
	}
	if !info.IsDir() {
		http.Error(w, "Path must be a directory", http.StatusBadRequest)
		return
	}

	// Collect markdown files in new directory
	newMarkdownFiles := collectMarkdownFiles(targetPath)
	if len(newMarkdownFiles) == 0 {
		http.Error(w, "No markdown files found in directory", http.StatusBadRequest)
		return
	}

	// Update state thread-safely
	fileMutex.Lock()
	browseDir = targetPath
	markdownFiles = newMarkdownFiles
	fileMutex.Unlock()

	// Restart directory watcher for new directory
	if err := dirWatcher.watchDirectory(targetPath); err != nil {
		log.Printf("Warning: Cannot watch new directory for changes: %v", err)
	}

	log.Printf("Navigated to: %s (%d markdown files)", targetPath, len(newMarkdownFiles))

	w.WriteHeader(http.StatusOK)
}

// moveToTrash attempts to move a file to the OS trash/recycle bin.
// Falls back to permanent deletion (os.Remove) if trash commands fail.
// Supports macOS (osascript), Linux (gio trash), and Windows (PowerShell).
func moveToTrash(filePath string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin": // macOS
		// Use AppleScript to move to Trash
		script := fmt.Sprintf(`tell application "Finder" to delete POSIX file "%s"`, filePath)
		cmd = exec.Command("osascript", "-e", script)

	case "linux":
		// Try gio trash (GNOME/modern Linux)
		cmd = exec.Command("gio", "trash", filePath)

	case "windows":
		// Use PowerShell to move to Recycle Bin
		script := fmt.Sprintf(`Add-Type -AssemblyName Microsoft.VisualBasic; [Microsoft.VisualBasic.FileIO.FileSystem]::DeleteFile('%s', 'OnlyErrorDialogs', 'SendToRecycleBin')`, filePath)
		cmd = exec.Command("powershell", "-Command", script)

	default:
		// Unsupported OS, fall back to permanent deletion
		log.Printf("Warning: Trash not supported on %s, permanently deleting file: %s", runtime.GOOS, filePath)
		return os.Remove(filePath)
	}

	// Attempt to move to trash
	if err := cmd.Run(); err != nil {
		log.Printf("Warning: Failed to move to trash (attempting permanent deletion): %v", err)
		// Fallback to permanent deletion
		return os.Remove(filePath)
	}

	log.Printf("Moved to trash: %s", filePath)
	return nil
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path string `json:"path"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	targetPath := strings.TrimSpace(req.Path)
	if targetPath == "" {
		http.Error(w, "Path cannot be empty", http.StatusBadRequest)
		return
	}

	// Validate and resolve path with security checks
	validatedPath, err := validateAndResolvePath(targetPath)
	if err != nil {
		statusCode := http.StatusBadRequest
		if strings.Contains(err.Error(), "access denied") {
			statusCode = http.StatusForbidden
		} else if strings.Contains(err.Error(), "cannot determine home directory") {
			statusCode = http.StatusInternalServerError
		}
		http.Error(w, err.Error(), statusCode)
		return
	}
	targetPath = validatedPath

	// Security: check if file is in our markdown files whitelist
	fileMutex.RLock()
	found := false
	for _, f := range markdownFiles {
		if f == targetPath {
			found = true
			break
		}
	}
	fileMutex.RUnlock()

	if !found {
		http.Error(w, "File not found or access denied", http.StatusForbidden)
		return
	}

	// Move file to trash (with fallback to permanent deletion)
	if err := moveToTrash(targetPath); err != nil {
		http.Error(w, fmt.Sprintf("Failed to delete file: %v", err), http.StatusInternalServerError)
		return
	}

	// Remove from markdownFiles list and recollect files
	fileMutex.Lock()
	currentBrowseDir := browseDir
	markdownFiles = collectMarkdownFiles(currentBrowseDir)
	// Clear currentFile if it was the deleted file
	if currentFile == targetPath {
		currentFile = ""
	}
	fileMutex.Unlock()

	log.Printf("Deleted file: %s", targetPath)

	w.WriteHeader(http.StatusOK)
}

func serveFile(w http.ResponseWriter, r *http.Request) {
	filePath := strings.TrimPrefix(r.URL.Path, "/view/")
	filePath = strings.TrimPrefix(filePath, "/")

	// Clean the path
	filePath = filepath.Clean(filePath)

	// Resolve to absolute path using browseDir
	absFilePath := resolveFilePath(filePath)

	// Security: check if file is in our markdown files whitelist (thread-safe)
	// This prevents directory traversal attacks by only allowing pre-scanned files
	fileMutex.RLock()
	currentBrowseDir := browseDir // Also capture for template data
	found := false
	for _, f := range markdownFiles {
		if f == absFilePath {
			found = true
			break
		}
	}
	fileMutex.RUnlock()

	if !found {
		http.NotFound(w, r)
		return
	}

	// Render the markdown file
	content, err := os.ReadFile(absFilePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	md := newMarkdownRenderer()

	var buf bytes.Buffer
	if err := md.Convert(content, &buf); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Generate tree HTML only for full page loads (not SPA navigation)
	var treeHTML string
	if !isPartialRequest(r) {
		treeHTML = generateTreeHTML()
	}

	// Fetch session metadata for this file (if available)
	var sessionData *SessionMetadata
	if globalSessionStore != nil {
		if metadata, found := globalSessionStore.get(absFilePath); found {
			sessionData = metadata
		}
	}

	data := browserTemplateData{
		baseTemplateData: newBaseTemplateData(),
		Title:            filepath.Base(absFilePath),
		Subtitle:         absFilePath,
		TreeHTML:         template.HTML(treeHTML),
		Content:          template.HTML(buf.String()),
		ShowBackButton:   true,
		BrowsePath:       currentBrowseDir,
		SessionData:      sessionData,
	}

	// Set current file for watching
	fileMutex.Lock()
	oldFile := currentFile
	currentFile = absFilePath
	fileMutex.Unlock()

	// Start watching the new file if it changed
	if oldFile != absFilePath {
		if err := fileWatcher.watch(absFilePath); err != nil {
			log.Printf("Error watching file: %v", err)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Choose template based on request type (SPA partial vs full page)
	tmpl := fileBrowserTmpl
	if isPartialRequest(r) {
		tmpl = fileBrowserPartialTmpl
	}

	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Template execution error: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// parseIgnoreFile reads and parses .peekmignore file
func parseIgnoreFile(rootDir string) []string {
	ignoreFilePath := filepath.Join(rootDir, ".peekmignore")

	// CRITICAL: Validate path through existing security chain
	validatedPath, err := validateAndResolvePath(ignoreFilePath)
	if err != nil {
		return nil // Outside $HOME or path validation failed
	}

	file, err := os.Open(validatedPath)
	if err != nil {
		return nil // File doesn't exist or can't be read - silent fallback
	}
	defer file.Close()

	const maxWarnings = 3
	const maxPatternLength = 256

	var customPatterns []string
	var invalidCount int
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Reject patterns that are too long (prevent pathological cases)
		if len(line) > maxPatternLength {
			invalidCount++
			if invalidCount <= maxWarnings {
				log.Printf("Warning: .peekmignore pattern too long (max %d chars, ignored): %s", maxPatternLength, line[:50]+"...")
			}
			continue
		}

		// Reject patterns with path separators (ambiguous intent)
		if strings.Contains(line, "/") || strings.Contains(line, "\\") {
			invalidCount++
			if invalidCount <= maxWarnings {
				log.Printf("Warning: .peekmignore pattern contains path separator (ignored): %s", line)
			}
			continue
		}

		// Validate pattern syntax with arbitrary test filename
		if _, err := filepath.Match(line, "test"); err != nil {
			invalidCount++
			if invalidCount <= maxWarnings {
				log.Printf("Warning: Invalid .peekmignore pattern '%s': %v", line, err)
			}
			continue
		}

		customPatterns = append(customPatterns, line)
	}

	// Summarize suppressed warnings
	if invalidCount > maxWarnings {
		log.Printf("Warning: Suppressed %d additional invalid .peekmignore patterns", invalidCount-maxWarnings)
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Warning: Error reading .peekmignore: %v", err)
		return nil
	}

	return customPatterns
}

// getIgnorePatterns returns custom ignore patterns with caching
// Reduces file I/O by caching patterns per rootDir
func getIgnorePatterns(rootDir string) []string {
	// Check cache (read lock)
	globalIgnoreCache.mu.RLock()
	if globalIgnoreCache.rootDir == rootDir {
		patterns := globalIgnoreCache.patterns
		globalIgnoreCache.mu.RUnlock()
		return patterns // Cache hit
	}
	globalIgnoreCache.mu.RUnlock()

	// Cache miss - parse file
	patterns := parseIgnoreFile(rootDir)

	// Update cache (write lock)
	globalIgnoreCache.mu.Lock()
	globalIgnoreCache.rootDir = rootDir
	globalIgnoreCache.patterns = patterns
	globalIgnoreCache.mu.Unlock()

	return patterns
}

// matchesIgnorePattern checks if directory name matches any pattern
func matchesIgnorePattern(dirName string, patterns []string) bool {
	for _, pattern := range patterns {
		// Simple wildcard matching using filepath.Match
		matched, err := filepath.Match(pattern, dirName)
		if err != nil {
			log.Printf("Warning: Invalid pattern '%s': %v", pattern, err)
			continue
		}
		if matched {
			return true
		}
	}
	return false
}

// isHardcodedExclusion checks if directory name is in hardcoded exclusions
// Uses map for O(1) lookup performance
func isHardcodedExclusion(dirName string) bool {
	return hardcodedExclusionsMap[dirName]
}

// FileInfo holds file metadata for smart selection
type FileInfo struct {
	Path    string
	ModTime time.Time
}

// selectDefaultFile returns the best file to display by default
// Priority: README.md > readme.md > most recent > first alphabetically
func selectDefaultFile(files []string) string {
	if len(files) == 0 {
		return ""
	}

	// Convert to FileInfo with modification times
	var fileInfos []FileInfo
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			continue // Skip files that can't be stat'd
		}
		fileInfos = append(fileInfos, FileInfo{
			Path:    f,
			ModTime: info.ModTime(),
		})
	}

	if len(fileInfos) == 0 {
		return ""
	}

	// Priority 1: README.md (exact match)
	for _, f := range fileInfos {
		if filepath.Base(f.Path) == "README.md" {
			return f.Path
		}
	}

	// Priority 2: readme.md (case-insensitive)
	for _, f := range fileInfos {
		if strings.EqualFold(filepath.Base(f.Path), "readme.md") {
			return f.Path
		}
	}

	// Priority 3: Most recently modified (AI workflow optimization)
	mostRecent := fileInfos[0]
	for _, f := range fileInfos {
		if f.ModTime.After(mostRecent.ModTime) {
			mostRecent = f
		}
	}
	return mostRecent.Path
}

func collectMarkdownFiles(rootDir string) []string {
	var files []string

	// Load .peekmignore patterns from rootDir (with caching)
	customPatterns := getIgnorePatterns(rootDir)
	if len(customPatterns) > 0 {
		log.Printf("[peekm] Using .peekmignore (%d custom exclusions)", len(customPatterns))
	}

	// Get home directory for security boundary checks
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Printf("Warning: Cannot determine home directory for security checks: %v", err)
		homeDir = "" // Disable boundary check if we can't determine home
	}

	filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Security: Skip symlinks that point outside $HOME
		resolvedInfo, shouldSkip, err := validateSymlinkSecurity(path, info, homeDir)
		if shouldSkip {
			return filepath.SkipDir
		}
		if err != nil {
			return nil
		}
		if resolvedInfo != nil {
			info = resolvedInfo
		}

		// Skip hidden and excluded directories (hardcoded + custom .peekmignore)
		if info.IsDir() {
			name := info.Name()
			// Skip hidden dirs EXCEPT .claude (for AI workflows)
			if strings.HasPrefix(name, ".") && path != rootDir && name != ".claude" {
				return filepath.SkipDir
			}
			// Skip hardcoded exclusions (O(1) map lookup)
			if isHardcodedExclusion(name) {
				return filepath.SkipDir
			}
			// Apply custom .peekmignore patterns (skip check if no patterns loaded)
			if len(customPatterns) > 0 && matchesIgnorePattern(name, customPatterns) {
				return filepath.SkipDir
			}
		}

		// Collect .md files
		if !info.IsDir() && strings.HasSuffix(strings.ToLower(info.Name()), ".md") {
			files = append(files, path)
		}

		return nil
	})

	sort.Strings(files)
	return files
}

func generateTreeHTML() string {
	// Get state snapshot (thread-safe)
	fileMutex.RLock()
	currentBrowseDir := browseDir
	currentMarkdownFiles := make([]string, len(markdownFiles))
	copy(currentMarkdownFiles, markdownFiles)
	fileMutex.RUnlock()

	if len(currentMarkdownFiles) == 0 {
		return ""
	}

	// Make browse directory absolute for proper relative path calculation
	absDir, err := filepath.Abs(currentBrowseDir)
	if err != nil {
		absDir = currentBrowseDir
	}

	root := &fileNode{name: ".", isDir: true}
	dirNodes := make(map[string]*fileNode)
	dirNodes["."] = root

	// Build directory structure
	for _, path := range currentMarkdownFiles {
		// Make file path absolute first
		absPath := path
		if !filepath.IsAbs(path) {
			absPath, _ = filepath.Abs(path)
		}

		// Make path relative to browse directory
		relPath, err := filepath.Rel(absDir, absPath)
		if err != nil {
			relPath = filepath.Base(path)
		}

		parts := strings.Split(filepath.Dir(relPath), string(filepath.Separator))

		currentPath := "."
		for _, part := range parts {
			if part == "." {
				continue
			}

			parentPath := currentPath
			if currentPath == "." {
				currentPath = part
			} else {
				currentPath = filepath.Join(currentPath, part)
			}

			if _, exists := dirNodes[currentPath]; !exists {
				node := &fileNode{
					name:  part,
					path:  currentPath, // Use relative path for directories too
					isDir: true,
				}
				dirNodes[currentPath] = node
				if parent, ok := dirNodes[parentPath]; ok {
					parent.children = append(parent.children, node)
				}
			}
		}

		// Add file
		info, err := os.Stat(path)
		if err != nil {
			// Skip files that no longer exist (e.g., after navigation to different directory)
			continue
		}
		fileNode := &fileNode{
			name: filepath.Base(relPath),
			path: relPath, // Use relative path for the link (security & clean URLs)
			size: info.Size(),
		}

		dir := filepath.Dir(relPath)
		if parent, ok := dirNodes[dir]; ok {
			parent.children = append(parent.children, fileNode)
		}
	}

	// Clean and sort tree
	cleanEmptyDirs(root)
	sortTree(root)

	// Generate HTML
	var buf bytes.Buffer
	generateTreeHTMLRecursive(root, "", true, true, 0, false, &buf)
	return buf.String()
}

func generateTreeHTMLRecursive(node *fileNode, prefix string, isLast bool, isRoot bool, depth int, parentCollapsed bool, buf *bytes.Buffer) {
	if !isRoot {
		// Start tree item container
		buf.WriteString(`<div class="tree-item">`)

		if node.isDir {
			// Collapse directories at depth >= 1 by default
			collapsed := depth >= 1

			// Directory node with chevron and name
			buf.WriteString(fmt.Sprintf(`<div class="tree-node"><span class="tree-directory" onclick="toggleDir(this)" data-path="%s">`,
				template.HTMLEscapeString(node.path)))

			// Chevron icon
			if collapsed {
				buf.WriteString(`<span class="expand-icon">▶</span>`)
			} else {
				buf.WriteString(`<span class="expand-icon">▼</span>`)
			}

			buf.WriteString(fmt.Sprintf(`<span class="dir-name">%s</span></span></div>`, template.HTMLEscapeString(node.name)))

			// Children container (collapsed by default at depth >= 1)
			if len(node.children) > 0 {
				if collapsed {
					buf.WriteString(`<div class="tree-children" style="display: none;">`)
				} else {
					buf.WriteString(`<div class="tree-children">`)
				}

				// Render children recursively
				for _, child := range node.children {
					generateTreeHTMLRecursive(child, "", false, false, depth+1, false, buf)
				}

				buf.WriteString(`</div>`) // Close tree-children
			}
		} else {
			// File node (leaf)
			buf.WriteString(`<div class="tree-node"><span class="tree-file">`)
			buf.WriteString(fmt.Sprintf(`<a href="/view/%s">%s</a>`, template.URLQueryEscaper(node.path), template.HTMLEscapeString(node.name)))
			buf.WriteString(`</span></div>`)
		}

		buf.WriteString(`</div>`) // Close tree-item
	} else {
		// Root node - just render children
		for _, child := range node.children {
			generateTreeHTMLRecursive(child, "", false, false, depth, false, buf)
		}
	}
}

func openURL(url string) {
	var cmd string
	var args []string

	switch {
	case fileExists("/usr/bin/open"): // macOS
		cmd = "open"
		args = []string{url}
	case fileExists("/usr/bin/xdg-open"): // Linux
		cmd = "xdg-open"
		args = []string{url}
	default: // Windows
		cmd = "cmd"
		args = []string{"/c", "start", url}
	}

	exec := exec.Command(cmd, args...)
	if err := exec.Start(); err != nil {
		log.Printf("Failed to open URL %s: %v", url, err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

type fileNode struct {
	name     string
	path     string
	size     int64
	isDir    bool
	children []*fileNode
}

func cleanEmptyDirs(node *fileNode) bool {
	if !node.isDir {
		return true // Keep files
	}

	// Recursively clean children
	kept := make([]*fileNode, 0)
	for _, child := range node.children {
		if cleanEmptyDirs(child) {
			kept = append(kept, child)
		}
	}
	node.children = kept

	// Keep directory if it has children or is root
	return len(node.children) > 0 || node.name == "."
}

func sortTree(node *fileNode) {
	if !node.isDir {
		return
	}

	// Sort children: directories first, then files, alphabetically within each group
	sort.Slice(node.children, func(i, j int) bool {
		if node.children[i].isDir != node.children[j].isDir {
			return node.children[i].isDir
		}
		return node.children[i].name < node.children[j].name
	})

	// Recursively sort children
	for _, child := range node.children {
		sortTree(child)
	}
}
