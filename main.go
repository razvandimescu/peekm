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

const (
	eventLogMaxOnDisk   = 5000
	eventLogMaxInMemory = 10000
)

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
	port        = flag.Int("port", 6419, "Port to serve on")
	openBrowser = flag.Bool("browser", true, "Open browser automatically")
	showVersion = flag.Bool("version", false, "Show version information")
	showIgnored = flag.Bool("show-ignored", false, "Show all excluded directories and exit")
	disableHook = flag.Bool("no-ai-tracking", false, "Disable AI session tracking endpoint")

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
	timelineTmpl           *template.Template
	timelinePartialTmpl    *template.Template

	// SSE event replay buffer (50 events = ~2 min of AI file creation)
	globalEventBuffer = newEventBuffer(50)

	// Claude Code session tracking (5s TTL for hook-to-fsnotify correlation)
	globalSessionStore *sessionStore

	// Persistent event log (JSONL file for session history)
	globalEventLog *eventLog
)

// watcherManager manages file watching with proper cleanup
type watcherManager struct {
	mu      sync.Mutex
	current *fsnotify.Watcher
	cancel  context.CancelFunc
}

// baseTemplateData contains common fields for all templates
type baseTemplateData struct {
	GitHubCSS         template.CSS
	ThemeOverrides    template.CSS
	ThemeManagerJS    template.JS
	EditorJS          template.JS
	NavigationJS      template.JS
	AITrackingEnabled bool
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
	Type    string `json:"type"` // "file_added" or "file_removed"
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
	mappings map[string]*SessionMetadata
}

// newSessionStore creates a session store (session data persists indefinitely)
func newSessionStore() *sessionStore {
	return &sessionStore{
		mappings: make(map[string]*SessionMetadata),
	}
}

// register stores session metadata for a file path (persists indefinitely)
func (ss *sessionStore) register(filePath string, metadata *SessionMetadata) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.mappings[filePath] = metadata
}

// get retrieves session metadata for a file path
func (ss *sessionStore) get(filePath string) (*SessionMetadata, bool) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	metadata, exists := ss.mappings[filePath]
	return metadata, exists
}

// SessionEvent is a single AI session event persisted to disk
type SessionEvent struct {
	SessionID      string    `json:"sid"`
	FilePath       string    `json:"path"`
	ToolName       string    `json:"tool"`
	PermissionMode string    `json:"perm,omitempty"`
	ToolUseID      string    `json:"tuid,omitempty"`
	CWD            string    `json:"cwd,omitempty"`
	TranscriptPath string    `json:"tp,omitempty"`
	Timestamp      time.Time `json:"ts"`
}

func (e *SessionEvent) toMetadata() *SessionMetadata {
	return &SessionMetadata{
		SessionID:      e.SessionID,
		ToolName:       e.ToolName,
		PermissionMode: e.PermissionMode,
		ToolUseID:      e.ToolUseID,
		CWD:            e.CWD,
		TranscriptPath: e.TranscriptPath,
		Timestamp:      e.Timestamp,
	}
}

func sessionEventFrom(meta *SessionMetadata, filePath string) SessionEvent {
	return SessionEvent{
		SessionID:      meta.SessionID,
		FilePath:       filePath,
		ToolName:       meta.ToolName,
		PermissionMode: meta.PermissionMode,
		ToolUseID:      meta.ToolUseID,
		CWD:            meta.CWD,
		TranscriptPath: meta.TranscriptPath,
		Timestamp:      meta.Timestamp,
	}
}

// eventLog persists session events to a JSONL file and keeps them in memory
type eventLog struct {
	mu       sync.RWMutex
	file     *os.File
	events   []SessionEvent
	filePath string
}

func newEventLog() (*eventLog, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}
	dir := filepath.Join(homeDir, ".peekm")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("cannot create ~/.peekm: %w", err)
	}
	fp := filepath.Join(dir, "events.jsonl")
	f, err := os.OpenFile(fp, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("cannot open events file: %w", err)
	}
	el := &eventLog{file: f, filePath: fp}
	if err := el.load(); err != nil {
		f.Close()
		return nil, err
	}
	return el, nil
}

func (el *eventLog) load() error {
	el.file.Seek(0, 0)
	scanner := bufio.NewScanner(el.file)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	var events []SessionEvent
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var evt SessionEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			log.Printf("Warning: skipping malformed event line: %v", err)
			continue
		}
		events = append(events, evt)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading events file: %w", err)
	}
	if len(events) > eventLogMaxOnDisk {
		events = events[len(events)-eventLogMaxOnDisk:]
		el.rewrite(events)
	}
	el.events = events
	return nil
}

// rewrite replaces the events file with the given events (called during load, single-threaded).
func (el *eventLog) rewrite(events []SessionEvent) {
	tmpPath := el.filePath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		log.Printf("Warning: cannot rewrite events file: %v", err)
		return
	}
	w := bufio.NewWriter(f)
	for _, evt := range events {
		if data, err := json.Marshal(evt); err == nil {
			w.Write(data)
			w.WriteByte('\n')
		}
	}
	w.Flush()
	f.Sync()
	f.Close()

	el.file.Close()
	if err := os.Rename(tmpPath, el.filePath); err != nil {
		log.Printf("Warning: cannot rename events file: %v", err)
		os.Remove(tmpPath)
	}
	// Reopen for append
	reopened, err := os.OpenFile(el.filePath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("Warning: cannot reopen events file after rewrite: %v", err)
		return
	}
	el.file = reopened
}

func (el *eventLog) append(event SessionEvent) error {
	el.mu.Lock()
	defer el.mu.Unlock()
	if el.file == nil {
		return fmt.Errorf("event log file is closed")
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := el.file.Write(data); err != nil {
		return err
	}
	el.events = append(el.events, event)
	if len(el.events) > eventLogMaxInMemory {
		el.events = el.events[len(el.events)-eventLogMaxOnDisk:]
	}
	return nil
}

// eventsForDir returns events under dir, newest first.
func (el *eventLog) eventsForDir(dir string) []SessionEvent {
	el.mu.RLock()
	defer el.mu.RUnlock()
	prefix := dir + string(filepath.Separator)
	var out []SessionEvent
	for i := len(el.events) - 1; i >= 0; i-- {
		evt := el.events[i]
		if strings.HasPrefix(evt.FilePath, prefix) || evt.FilePath == dir {
			out = append(out, evt)
		}
	}
	return out
}

func (el *eventLog) latestPerFile() map[string]*SessionMetadata {
	el.mu.RLock()
	defer el.mu.RUnlock()
	result := make(map[string]*SessionMetadata)
	// Iterate forward so later entries overwrite earlier ones
	for i := range el.events {
		evt := &el.events[i]
		if evt.ToolName == "View" {
			continue // Don't hydrate sessionStore with view events
		}
		result[evt.FilePath] = evt.toMetadata()
	}
	return result
}

func (el *eventLog) close() error {
	el.mu.Lock()
	defer el.mu.Unlock()
	if el.file != nil {
		return el.file.Close()
	}
	return nil
}

// newBaseTemplateData creates a baseTemplateData with embedded resources
func newBaseTemplateData() baseTemplateData {
	return baseTemplateData{
		GitHubCSS:         template.CSS(githubCSS),
		ThemeOverrides:    template.CSS(themeOverrides),
		ThemeManagerJS:    template.JS(themeManagerJS),
		EditorJS:          template.JS(editorJS),
		NavigationJS:      template.JS(navigationJS),
		AITrackingEnabled: !*disableHook,
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

	customPatterns := getIgnorePatterns(rootDir)

	err := filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Security: Skip symlinks outside $HOME
		resolvedInfo, _, resolveErr := validateSymlinkSecurity(path, info, homeDir)
		if resolveErr != nil {
			return nil
		}
		if resolvedInfo != nil {
			info = resolvedInfo
		}

		if info.IsDir() && path != rootDir {
			if isExcludedDir(info.Name(), customPatterns) {
				return filepath.SkipDir
			}
			dirsToWatch = append(dirsToWatch, path)
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

// withCSRFCheck rejects cross-origin POST requests by validating the Origin header
func withCSRFCheck(next http.HandlerFunc) http.HandlerFunc {
	allowedLocal := fmt.Sprintf("http://localhost:%d", *port)
	allowedLoopback := fmt.Sprintf("http://127.0.0.1:%d", *port)
	return func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && origin != allowedLocal && origin != allowedLoopback {
			log.Printf("CSRF: rejected cross-origin POST from %s", origin)
			http.Error(w, "Forbidden: cross-origin request", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// registerRoutes registers all HTTP routes
func registerRoutes() {
	http.HandleFunc("/", withRecovery(serveBrowser))
	http.HandleFunc("/view/", withRecovery(serveFile))
	http.HandleFunc("/navigate", withRecovery(withCSRFCheck(handleNavigate)))
	http.HandleFunc("/delete", withRecovery(withCSRFCheck(handleDelete)))
	http.HandleFunc("/raw/", withRecovery(serveRaw))
	http.HandleFunc("/save", withRecovery(withCSRFCheck(handleSave)))
	http.HandleFunc("/download", withRecovery(withCSRFCheck(handleDownload)))
	http.HandleFunc("/events", withRecovery(serveSSE))
	http.HandleFunc("/tree-html", withRecovery(serveTreeHTML))
	http.HandleFunc("/timeline", withRecovery(serveTimeline))

	// AI session tracking endpoint (always on unless --no-ai-tracking)
	if !*disableHook {
		http.HandleFunc("/hook/file-modified", withRecovery(handleClaudeHook))
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
		return nil, false, err
	}

	// Check if resolved path is within $HOME
	if homeDir != "" && !strings.HasPrefix(resolved, homeDir) {
		log.Printf("Security: Skipping symlink outside home directory: %s -> %s", path, resolved)
		return nil, false, fmt.Errorf("symlink outside home")
	}

	// Update info to reflect the resolved target
	resolvedInfo, err := os.Stat(resolved)
	if err != nil {
		log.Printf("Warning: Cannot stat symlink target: %s", resolved)
		return nil, false, err
	}

	return resolvedInfo, false, nil
}

// validateAndResolvePath validates and resolves a path with security checks
// Returns the validated absolute path or an error if validation fails
// isPartialRequest detects if the request is an AJAX/fetch request for partial content
func isPartialRequest(r *http.Request) bool {
	return r.Header.Get("X-Requested-With") == "XMLHttpRequest"
}

// renderTemplatePair selects full/partial template, executes to buffer, and writes the response.
// Returns true on success, false if an error was written to w.
func renderTemplatePair(w http.ResponseWriter, r *http.Request, full, partial *template.Template, data any) bool {
	tmpl := full
	if isPartialRequest(r) {
		tmpl = partial
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		log.Printf("Template execution error: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return false
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
	return true
}

// renderTemplate uses the default file-browser template pair.
func renderTemplate(w http.ResponseWriter, r *http.Request, data any) bool {
	return renderTemplatePair(w, r, fileBrowserTmpl, fileBrowserPartialTmpl, data)
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

// isWhitelistedFile checks if a path is in the current markdownFiles whitelist (thread-safe)
func isWhitelistedFile(path string) bool {
	fileMutex.RLock()
	defer fileMutex.RUnlock()
	for _, f := range markdownFiles {
		if f == path {
			return true
		}
	}
	return false
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

	// Load shared session info panel template
	sessionInfoPanelHTML, err := themeFS.ReadFile("theme/session-info-panel.html")
	if err != nil {
		log.Fatalf("Failed to load session-info-panel template: %v", err)
	}

	fileBrowserHTML, err := themeFS.ReadFile("theme/file-browser.html")
	if err != nil {
		log.Fatalf("Failed to load file-browser template: %v", err)
	}
	fileBrowserTmpl = template.Must(template.New("file-browser").Funcs(funcMap).Parse(string(fileBrowserHTML)))
	fileBrowserTmpl = template.Must(fileBrowserTmpl.Parse(string(sessionInfoPanelHTML)))

	fileBrowserPartialHTML, err := themeFS.ReadFile("theme/file-browser-partial.html")
	if err != nil {
		log.Fatalf("Failed to load file-browser-partial template: %v", err)
	}
	fileBrowserPartialTmpl = template.Must(template.New("file-browser-partial").Funcs(funcMap).Parse(string(fileBrowserPartialHTML)))
	fileBrowserPartialTmpl = template.Must(fileBrowserPartialTmpl.Parse(string(sessionInfoPanelHTML)))

	// Timeline templates
	timelineHTML, err := themeFS.ReadFile("theme/timeline.html")
	if err != nil {
		log.Fatalf("Failed to load timeline template: %v", err)
	}
	timelineTmpl = template.Must(template.New("timeline").Funcs(funcMap).Parse(string(timelineHTML)))

	timelinePartialHTML, err := themeFS.ReadFile("theme/timeline-partial.html")
	if err != nil {
		log.Fatalf("Failed to load timeline-partial template: %v", err)
	}
	timelinePartialTmpl = template.Must(template.New("timeline-partial").Funcs(funcMap).Parse(string(timelinePartialHTML)))
}

// runSetup handles the "peekm setup" subcommand
func runSetup(args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: peekm setup claude-code [--remove] [--port PORT]")
		fmt.Println("\nConfigures Claude Code to send file modification events to peekm.")
		os.Exit(1)
	}

	switch args[0] {
	case "claude-code":
		setupClaudeCode(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown setup target: %s\n", args[0])
		fmt.Println("Available: claude-code")
		os.Exit(1)
	}
}

func setupClaudeCode(args []string) {
	setupFlags := flag.NewFlagSet("setup claude-code", flag.ExitOnError)
	remove := setupFlags.Bool("remove", false, "Remove peekm hooks from Claude Code")
	hookPort := setupFlags.Int("port", 6419, "Port peekm runs on")
	setupFlags.Parse(args)

	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot determine home directory: %v\n", err)
		os.Exit(1)
	}

	claudeDir := filepath.Join(homeDir, ".claude")
	settingsPath := filepath.Join(claudeDir, "settings.json")
	hookScriptPath := filepath.Join(claudeDir, "peekm-hook.sh")

	if *remove {
		removeClaudeCodeSetup(settingsPath, hookScriptPath)
		return
	}

	fmt.Println("\n  AI Session Tracking Setup")
	fmt.Println("  " + strings.Repeat("\u2500", 25))

	// Step 1: Create hook script
	fmt.Printf("\n  Step 1: Hook script\n")

	hookScript := fmt.Sprintf(`#!/bin/bash
json=$(cat)
session_id=$(echo "$json" | jq -r '.session_id // empty')
tool_name=$(echo "$json" | jq -r '.tool_name // empty')
file_path=$(echo "$json" | jq -r '.tool_input.file_path // .tool_input.notebook_path // empty')

if [ -n "$session_id" ] && [ -n "$tool_name" ] && [ -n "$file_path" ]; then
    # For Claude plan files, forward content for devcontainer support
    if echo "$file_path" | grep -q '\.claude/plans/.*\.md$'; then
        payload=$(echo "$json" | jq -c '{session_id, tool_name, file_path: .tool_input.file_path, content: .tool_input.content}')
        curl -s -X POST -H 'Content-Type: application/json' \
            -d "$payload" \
            --max-time 0.5 http://localhost:%d/hook/file-modified >/dev/null 2>&1
    else
        curl -s -X POST -H 'Content-Type: application/json' \
            -d "{\"session_id\":\"$session_id\",\"tool_name\":\"$tool_name\",\"file_path\":\"$file_path\"}" \
            --max-time 0.1 http://localhost:%d/hook/file-modified >/dev/null 2>&1
    fi
fi
`, *hookPort, *hookPort)

	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "    Error creating %s: %v\n", claudeDir, err)
		os.Exit(1)
	}

	if err := os.WriteFile(hookScriptPath, []byte(hookScript), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "    Error writing hook script: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("    Created %s\n", hookScriptPath)

	// Step 2: Merge hooks into settings.json
	fmt.Printf("\n  Step 2: Claude Code settings\n")

	hookEntry := map[string]interface{}{
		"type":    "command",
		"command": hookScriptPath,
		"timeout": 0.15,
	}

	matchers := []string{"Write", "Edit", "NotebookEdit"}

	// Read existing settings or start fresh
	var settings map[string]interface{}
	data, err := os.ReadFile(settingsPath)
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			fmt.Fprintf(os.Stderr, "    Error parsing %s: %v\n", settingsPath, err)
			os.Exit(1)
		}
		fmt.Printf("    Found %s\n", settingsPath)
	} else {
		settings = make(map[string]interface{})
		fmt.Printf("    Creating %s\n", settingsPath)
	}

	// Ensure hooks.PostToolUse exists
	hooks, _ := settings["hooks"].(map[string]interface{})
	if hooks == nil {
		hooks = make(map[string]interface{})
		settings["hooks"] = hooks
	}

	postToolUse, _ := hooks["PostToolUse"].([]interface{})
	if postToolUse == nil {
		postToolUse = []interface{}{}
	}

	// Add hooks for each matcher (idempotent — skip if peekm hook already exists)
	added := 0
	for _, matcher := range matchers {
		if hasPeekmHook(postToolUse, matcher, hookScriptPath) {
			continue
		}

		entry := map[string]interface{}{
			"matcher": matcher,
			"hooks":   []interface{}{hookEntry},
		}
		postToolUse = append(postToolUse, entry)
		added++
	}

	hooks["PostToolUse"] = postToolUse
	settings["hooks"] = hooks

	// Write settings back
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "    Error serializing settings: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(settingsPath, append(out, '\n'), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "    Error writing %s: %v\n", settingsPath, err)
		os.Exit(1)
	}

	if added > 0 {
		fmt.Printf("    Added %d PostToolUse hook(s) (%s)\n", added, strings.Join(matchers[:added], ", "))
	} else {
		fmt.Printf("    Hooks already configured (no changes)\n")
	}

	fmt.Println("\n  Setup complete. Restart Claude Code to activate.")
	fmt.Println("  To verify: modify a file with Claude Code and check peekm")
	fmt.Println("  for the AI session badge.")
	fmt.Println()
}

// hasPeekmHook checks if a PostToolUse entry for this matcher already has a peekm hook
func hasPeekmHook(entries []interface{}, matcher, scriptPath string) bool {
	for _, entry := range entries {
		e, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		if e["matcher"] != matcher {
			continue
		}
		hooks, ok := e["hooks"].([]interface{})
		if !ok {
			continue
		}
		for _, h := range hooks {
			hook, ok := h.(map[string]interface{})
			if !ok {
				continue
			}
			if cmd, ok := hook["command"].(string); ok && cmd == scriptPath {
				return true
			}
		}
	}
	return false
}

// removeClaudeCodeSetup removes peekm hooks from Claude Code settings
// filterPeekmHooks returns PostToolUse entries that don't reference the peekm hook script.
func filterPeekmHooks(entries []interface{}, hookScriptPath string) (filtered []interface{}, removed int) {
	for _, entry := range entries {
		e, ok := entry.(map[string]interface{})
		if !ok {
			filtered = append(filtered, entry)
			continue
		}
		entryHooks, ok := e["hooks"].([]interface{})
		if !ok {
			filtered = append(filtered, entry)
			continue
		}
		if containsPeekmHook(entryHooks, hookScriptPath) {
			removed++
		} else {
			filtered = append(filtered, entry)
		}
	}
	return
}

func containsPeekmHook(hooks []interface{}, hookScriptPath string) bool {
	for _, h := range hooks {
		hook, ok := h.(map[string]interface{})
		if !ok {
			continue
		}
		if cmd, ok := hook["command"].(string); ok && cmd == hookScriptPath {
			return true
		}
	}
	return false
}

func removeClaudeCodeSetup(settingsPath, hookScriptPath string) {
	fmt.Println("\n  Removing AI Session Tracking")
	fmt.Println("  " + strings.Repeat("\u2500", 30))

	// Remove hook script
	if err := os.Remove(hookScriptPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "    Warning: %v\n", err)
	} else if err == nil {
		fmt.Printf("    Removed %s\n", hookScriptPath)
	}

	// Remove hooks from settings.json
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		fmt.Println("    No settings file found")
		fmt.Print("\n  Done.\n\n")
		return
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		fmt.Fprintf(os.Stderr, "    Error parsing settings: %v\n", err)
		os.Exit(1)
	}

	hooks, _ := settings["hooks"].(map[string]interface{})
	if hooks == nil {
		fmt.Println("    No hooks found in settings")
		fmt.Print("\n  Done.\n\n")
		return
	}

	postToolUse, _ := hooks["PostToolUse"].([]interface{})
	if postToolUse == nil {
		fmt.Println("    No PostToolUse hooks found")
		fmt.Print("\n  Done.\n\n")
		return
	}

	// Filter out entries whose hooks reference the peekm script
	filtered, removed := filterPeekmHooks(postToolUse, hookScriptPath)

	if removed > 0 {
		hooks["PostToolUse"] = filtered
		out, _ := json.MarshalIndent(settings, "", "  ")
		os.WriteFile(settingsPath, append(out, '\n'), 0644)
		fmt.Printf("    Removed %d hook(s) from settings.json\n", removed)
	} else {
		fmt.Println("    No peekm hooks found in settings")
	}

	fmt.Print("\n  Done.\n\n")
}

func runShowIgnored() {
	fmt.Println("Hardcoded exclusions:")
	fmt.Println("  .* (hidden directories, except .claude)")
	for _, dir := range hardcodedExclusions {
		fmt.Printf("  %s\n", dir)
	}

	checkDir := "."
	if flag.NArg() > 0 {
		checkDir = flag.Arg(0)
	}
	if absPath, err := filepath.Abs(checkDir); err == nil {
		checkDir = absPath
	}
	if info, err := os.Stat(checkDir); err == nil && !info.IsDir() {
		checkDir = filepath.Dir(checkDir)
	}

	if patterns := getIgnorePatterns(checkDir); len(patterns) > 0 {
		fmt.Printf("\nCustom exclusions (.peekmignore in %s):\n", checkDir)
		for _, p := range patterns {
			fmt.Printf("  %s\n", p)
		}
	} else {
		fmt.Printf("\nNo .peekmignore file found in %s\n", checkDir)
	}
}

// resolveTarget determines browseDir from CLI args and returns a target file (if any).
func resolveTarget() string {
	targetPath := "."
	if flag.NArg() > 0 {
		targetPath = flag.Arg(0)
	}

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

	if info.IsDir() {
		browseDir = absPath
		return ""
	}
	browseDir = filepath.Dir(absPath)
	return filepath.Base(absPath)
}

func buildStartupURL(baseURL, targetFile string) string {
	if targetFile == "" {
		fmt.Printf("peekm file browser at %s\n", baseURL)
		fmt.Printf("Browsing %s - found %d markdown file(s)\n", browseDir, len(markdownFiles))
		return baseURL
	}
	fullURL := baseURL
	for _, mdFile := range markdownFiles {
		if filepath.Base(mdFile) == targetFile {
			if relPath, err := filepath.Rel(browseDir, mdFile); err == nil {
				fullURL = fmt.Sprintf("%s/view/%s", baseURL, relPath)
			}
			break
		}
	}
	fmt.Printf("peekm at %s\n", baseURL)
	fmt.Printf("Opening %s - found %d markdown file(s)\n", targetFile, len(markdownFiles))
	return fullURL
}

func initSessionTracking() {
	globalSessionStore = newSessionStore()
	el, err := newEventLog()
	if err != nil {
		log.Printf("Warning: session persistence unavailable: %v", err)
		return
	}
	globalEventLog = el
	for path, meta := range el.latestPerFile() {
		globalSessionStore.register(path, meta)
	}
	el.mu.RLock()
	n := len(el.events)
	el.mu.RUnlock()
	log.Printf("Loaded %d persisted session events", n)
}

func main() {
	// Handle subcommands before flag.Parse()
	if len(os.Args) >= 2 && os.Args[1] == "setup" {
		runSetup(os.Args[2:])
		return
	}

	flag.Parse()

	if *showVersion {
		fmt.Printf("peekm %s (commit: %s, built: %s)\n", version, commit, date)
		os.Exit(0)
	}

	if *showIgnored {
		runShowIgnored()
		os.Exit(0)
	}

	if !*disableHook {
		initSessionTracking()
	}

	targetFile := resolveTarget()

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

	fullURL := buildStartupURL(url, targetFile)
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

		// Close event log
		if globalEventLog != nil {
			globalEventLog.close()
		}

		// Shutdown HTTP server
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("Server shutdown error: %v", err)
		}
	}()

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// getRelativePath converts absolute file path to relative path (thread-safe)
func getRelativePath(absPath string) string {
	fileMutex.RLock()
	defer fileMutex.RUnlock()

	relPath := absPath
	if browseDir != "" {
		if rel, err := filepath.Rel(browseDir, absPath); err == nil {
			relPath = rel
		}
	}
	return relPath
}

// removeFromWhitelist removes a file from the markdown files list (thread-safe)
func removeFromWhitelist(filePath string) {
	fileMutex.Lock()
	defer fileMutex.Unlock()

	for i, f := range markdownFiles {
		if f == filePath {
			markdownFiles = append(markdownFiles[:i], markdownFiles[i+1:]...)
			break
		}
	}
}

// sendFileEvent sends a file event notification to clients
func sendFileEvent(eventType, relPath, sessionID string) {
	msg := fileEventMessage{
		Type:    eventType,
		Path:    relPath,
		Session: sessionID,
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling %s message: %v", eventType, err)
	} else {
		notifyClientsWithMessage(string(msgBytes))
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

// handleDirCreated adds a newly created directory to the watcher if it's within $HOME.
func handleDirCreated(watcher *fsnotify.Watcher, dirPath string) {
	homeDir, _ := os.UserHomeDir()
	if homeDir == "" {
		return
	}
	resolved, err := filepath.EvalSymlinks(dirPath)
	if err != nil || !strings.HasPrefix(resolved, homeDir) {
		return
	}
	if err := watcher.Add(dirPath); err != nil {
		log.Printf("Warning: Cannot watch new directory %s: %v", dirPath, err)
	} else {
		log.Printf("Now watching new directory: %s", dirPath)
	}
}

// handleMarkdownCreated adds a new markdown file to the whitelist and notifies clients.
func handleMarkdownCreated(filePath string) {
	log.Printf("New markdown file created: %s", filePath)

	fileMutex.Lock()
	markdownFiles = append(markdownFiles, filePath)
	fileMutex.Unlock()

	go func() {
		sessionID := awaitSessionID(filePath)
		sendFileEvent("file_added", getRelativePath(filePath), sessionID)
	}()
}

// awaitSessionID polls the session store for up to 5s, returning the session ID if found.
func awaitSessionID(filePath string) string {
	if globalSessionStore == nil {
		return ""
	}
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			if metadata, found := globalSessionStore.get(filePath); found {
				return metadata.SessionID
			}
			return ""
		case <-ticker.C:
			if metadata, found := globalSessionStore.get(filePath); found {
				return metadata.SessionID
			}
		}
	}
}

// handleMarkdownRemoved removes a markdown file from the whitelist and notifies clients.
func handleMarkdownRemoved(filePath string, reason string) {
	log.Printf("%s file: %s", reason, filePath)
	removeFromWhitelist(filePath)
	sendFileEvent("file_removed", getRelativePath(filePath), "")
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

			if event.Op&fsnotify.Create == fsnotify.Create {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					handleDirCreated(watcher, event.Name)
				}
				if strings.HasSuffix(strings.ToLower(event.Name), ".md") {
					handleMarkdownCreated(event.Name)
				}
			}

			if event.Op&fsnotify.Remove == fsnotify.Remove {
				if strings.HasSuffix(strings.ToLower(event.Name), ".md") {
					handleMarkdownRemoved(event.Name, "Deleted")
				}
			}

			if event.Op&fsnotify.Rename == fsnotify.Rename {
				if strings.HasSuffix(strings.ToLower(event.Name), ".md") {
					handleMarkdownRemoved(event.Name, "Renamed")
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

	if !isWhitelistedFile(validated) {
		http.Error(w, "File not found or access denied", http.StatusForbidden)
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
		statusCode := http.StatusForbidden
		if strings.Contains(err.Error(), "does not exist") {
			statusCode = http.StatusNotFound
		}
		http.Error(w, fmt.Sprintf("Cannot save file: %v", err), statusCode)
		return
	}

	if !isWhitelistedFile(validated) {
		http.Error(w, "File not found or access denied", http.StatusForbidden)
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
	tmpFile, err := os.CreateTemp(dir, ".peekm-tmp-*")
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

	// Accept file path from request body (avoids global state race between tabs)
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Path) == "" {
		http.Error(w, "Missing file path", http.StatusBadRequest)
		return
	}

	absFilePath := resolveFilePath(filepath.Clean(strings.TrimPrefix(strings.TrimSpace(req.Path), "/")))

	filePath, err := validateAndResolvePath(absFilePath)
	if err != nil {
		http.Error(w, "Invalid path", http.StatusForbidden)
		return
	}

	if !isWhitelistedFile(filePath) {
		http.Error(w, "File not found or access denied", http.StatusForbidden)
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

	renderTemplate(w, r, data)
}

// handlePlanFile caches remote plan content and whitelists/broadcasts plan files.
// Returns the (possibly rewritten) file path.
func handlePlanFile(filePath, content, sessionID string) string {
	if !strings.HasSuffix(filePath, ".md") {
		return filePath
	}

	// Cache plan content from devcontainer/remote environments
	if content != "" && strings.Contains(filePath, ".claude/plans/") {
		homeDir, _ := os.UserHomeDir()
		if homeDir != "" {
			cacheDir := filepath.Join(homeDir, ".cache", "peekm", "plans")
			os.MkdirAll(cacheDir, 0755)
			localPath := filepath.Join(cacheDir, filepath.Base(filePath))
			if err := atomicWriteFile(localPath, content); err == nil {
				filePath = localPath
			}
		}
	}

	// Dynamically whitelist and broadcast plan files
	homeDir, _ := os.UserHomeDir()
	if homeDir == "" {
		return filePath
	}
	sep := string(os.PathSeparator)
	plansDir := filepath.Join(homeDir, ".claude", "plans")
	cacheDir := filepath.Join(homeDir, ".cache", "peekm", "plans")
	if !strings.HasPrefix(filePath, plansDir+sep) && !strings.HasPrefix(filePath, cacheDir+sep) {
		return filePath
	}
	if !isWhitelistedFile(filePath) {
		fileMutex.Lock()
		markdownFiles = append(markdownFiles, filePath)
		fileMutex.Unlock()
		log.Printf("Whitelisted Claude plan: %s", filePath)
	}
	sendFileEvent("file_modified", getRelativePath(filePath), sessionID)
	return filePath
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
		Content        string `json:"content"`
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

	req.FilePath = handlePlanFile(req.FilePath, req.Content, req.SessionID)

	// Register session mapping for file (after path rewrite so plan files use local path)
	globalSessionStore.register(req.FilePath, metadata)

	// Persist to event log
	if globalEventLog != nil {
		if err := globalEventLog.append(sessionEventFrom(metadata, req.FilePath)); err != nil {
			log.Printf("Warning: failed to persist session event: %v", err)
		}
	}

	log.Printf("AI session %s tracked for: %s (mode: %s)", truncateSessionID(req.SessionID), req.FilePath, req.PermissionMode)

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
		// Escape backslashes and double quotes to prevent AppleScript injection
		escaped := strings.ReplaceAll(filePath, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		script := fmt.Sprintf(`tell application "Finder" to delete POSIX file "%s"`, escaped)
		cmd = exec.Command("osascript", "-e", script)

	case "linux":
		// gio trash passes filePath as an argument, safe from injection
		cmd = exec.Command("gio", "trash", filePath)

	case "windows":
		// Escape single quotes for PowerShell single-quoted string
		escaped := strings.ReplaceAll(filePath, `'`, `''`)
		script := fmt.Sprintf(`Add-Type -AssemblyName Microsoft.VisualBasic; [Microsoft.VisualBasic.FileIO.FileSystem]::DeleteFile('%s', 'OnlyErrorDialogs', 'SendToRecycleBin')`, escaped)
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

	if !isWhitelistedFile(targetPath) {
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

	if !isWhitelistedFile(absFilePath) {
		http.NotFound(w, r)
		return
	}

	fileMutex.RLock()
	currentBrowseDir := browseDir
	fileMutex.RUnlock()

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

	// Log synthetic View event for "Unreviewed" smart folder tracking
	if globalEventLog != nil && sessionData != nil {
		if err := globalEventLog.append(SessionEvent{
			FilePath:  absFilePath,
			ToolName:  "View",
			Timestamp: time.Now(),
		}); err != nil {
			log.Printf("Warning: failed to log view event: %v", err)
		}
	}

	renderTemplate(w, r, data)
}

// Timeline template data types

type timelineTemplateData struct {
	baseTemplateData
	Title      string
	Subtitle   string
	BrowsePath string
	Groups     []timelineDayGroup
}

type timelineDayGroup struct {
	Label  string
	Events []timelineEntry
}

type timelineEntry struct {
	FilePath    string
	FileName    string
	ToolName    string
	TimeAgo     string
	TimeISO     string
	SessionID   string
	InWhitelist bool
}

func serveTimeline(w http.ResponseWriter, r *http.Request) {
	fileMutex.RLock()
	currentBrowseDir := browseDir
	fileMutex.RUnlock()

	var groups []timelineDayGroup

	if globalEventLog != nil {
		events := globalEventLog.eventsForDir(currentBrowseDir)

		now := time.Now()
		todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		yesterdayStart := todayStart.AddDate(0, 0, -1)

		// Group by date label
		type dayBucket struct {
			label  string
			events []timelineEntry
		}
		bucketMap := make(map[string]*dayBucket)
		var bucketOrder []string

		for _, evt := range events {
			if evt.ToolName == "View" {
				continue
			}

			relPath := evt.FilePath
			if rel, err := filepath.Rel(currentBrowseDir, evt.FilePath); err == nil {
				relPath = rel
			}

			entry := timelineEntry{
				FilePath:    template.URLQueryEscaper(relPath),
				FileName:    filepath.Base(evt.FilePath),
				ToolName:    evt.ToolName,
				TimeAgo:     formatTimeAgo(evt.Timestamp),
				TimeISO:     evt.Timestamp.Format(time.RFC3339),
				SessionID:   truncateSessionID(evt.SessionID),
				InWhitelist: isWhitelistedFile(evt.FilePath),
			}

			var label string
			if evt.Timestamp.After(todayStart) || evt.Timestamp.Equal(todayStart) {
				label = "Today"
			} else if evt.Timestamp.After(yesterdayStart) || evt.Timestamp.Equal(yesterdayStart) {
				label = "Yesterday"
			} else {
				label = evt.Timestamp.Format("Jan 2, 2006")
			}

			if _, exists := bucketMap[label]; !exists {
				bucketMap[label] = &dayBucket{label: label}
				bucketOrder = append(bucketOrder, label)
			}
			bucketMap[label].events = append(bucketMap[label].events, entry)
		}

		for _, label := range bucketOrder {
			b := bucketMap[label]
			groups = append(groups, timelineDayGroup{Label: b.label, Events: b.events})
		}
	}

	data := timelineTemplateData{
		baseTemplateData: newBaseTemplateData(),
		Title:            "AI Timeline",
		Subtitle:         fmt.Sprintf("Session history for %s", currentBrowseDir),
		BrowsePath:       currentBrowseDir,
		Groups:           groups,
	}

	renderTemplatePair(w, r, timelineTmpl, timelinePartialTmpl, data)
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
	customPatterns := getIgnorePatterns(rootDir)
	if len(customPatterns) > 0 {
		log.Printf("[peekm] Using .peekmignore (%d custom exclusions)", len(customPatterns))
	}

	homeDir, _ := os.UserHomeDir()

	visited := make(map[string]bool)
	var files []string
	collectMarkdownFilesWalk(rootDir, rootDir, homeDir, customPatterns, visited, &files)

	sort.Strings(files)
	return files
}

// isExcludedDir returns true if the directory name should be skipped
func isExcludedDir(name string, customPatterns []string) bool {
	if strings.HasPrefix(name, ".") && name != ".claude" {
		return true
	}
	if isHardcodedExclusion(name) {
		return true
	}
	if len(customPatterns) > 0 && matchesIgnorePattern(name, customPatterns) {
		return true
	}
	return false
}

// remapPath translates a resolved filesystem path back to its symlink-based equivalent
func remapPath(resolved, walkDir, path string) string {
	if walkDir == resolved {
		return path
	}
	relPath, err := filepath.Rel(resolved, path)
	if err != nil {
		return path
	}
	return filepath.Join(walkDir, relPath)
}

func collectMarkdownFilesWalk(walkDir, rootDir, homeDir string, customPatterns []string, visited map[string]bool, files *[]string) {
	// Resolve symlinks to get the real path for walking and cycle detection
	resolved, err := filepath.EvalSymlinks(walkDir)
	if err != nil {
		return
	}
	if visited[resolved] {
		return
	}
	visited[resolved] = true

	// Walk the resolved path (filepath.Walk won't descend into symlink roots)
	// Remap resolved paths back to the original symlink prefix for tree display
	filepath.Walk(resolved, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		// Security: Skip symlinks that point outside $HOME
		resolvedInfo, shouldSkip, resolveErr := validateSymlinkSecurity(path, info, homeDir)
		if shouldSkip {
			return filepath.SkipDir
		}
		if resolveErr != nil {
			return nil
		}

		isSymlink := info.Mode()&os.ModeSymlink != 0
		if resolvedInfo != nil {
			info = resolvedInfo
		}

		if info.IsDir() {
			if path != resolved && isExcludedDir(info.Name(), customPatterns) {
				return filepath.SkipDir
			}
			if isSymlink && path != resolved {
				collectMarkdownFilesWalk(remapPath(resolved, walkDir, path), rootDir, homeDir, customPatterns, visited, files)
				return nil
			}
		}

		if !info.IsDir() && strings.HasSuffix(strings.ToLower(info.Name()), ".md") {
			*files = append(*files, remapPath(resolved, walkDir, path))
		}

		return nil
	})
}

// Smart folder types for AI-aware sidebar sections
type smartFolder struct {
	Name  string
	ID    string // CSS-safe identifier
	Files []smartFolderFile
}

type smartFolderFile struct {
	RelPath   string
	Name      string
	ToolName  string
	TimeAgo   string
	SessionID string // truncated to 8 chars
}

func truncateSessionID(sid string) string {
	if len(sid) > 8 {
		return sid[:8]
	}
	return sid
}

func formatTimeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1m ago"
		}
		return fmt.Sprintf("%dm ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", h)
	case d < 48*time.Hour:
		return "yesterday"
	default:
		return t.Format("Jan 2")
	}
}

type smartFolderFileInfo struct {
	event       SessionEvent
	latestView  time.Time
	hasAIEvent  bool
	latestAIEvt time.Time
}

// aggregateFileEvents groups events by file path, tracking latest AI event and view per file.
func aggregateFileEvents(events []SessionEvent) map[string]*smartFolderFileInfo {
	fileMap := make(map[string]*smartFolderFileInfo)
	for _, evt := range events {
		fi, exists := fileMap[evt.FilePath]
		if !exists {
			fi = &smartFolderFileInfo{}
			fileMap[evt.FilePath] = fi
		}
		if evt.ToolName == "View" {
			if evt.Timestamp.After(fi.latestView) {
				fi.latestView = evt.Timestamp
			}
		} else if !fi.hasAIEvent {
			fi.event = evt
			fi.hasAIEvent = true
			fi.latestAIEvt = evt.Timestamp
		}
	}
	return fileMap
}

func generateSmartFolders() []smartFolder {
	if globalEventLog == nil {
		return nil
	}

	fileMutex.RLock()
	currentBrowseDir := browseDir
	fileMutex.RUnlock()

	events := globalEventLog.eventsForDir(currentBrowseDir)
	if len(events) == 0 {
		return nil
	}

	now := time.Now()
	fileMap := aggregateFileEvents(events)

	var recentAI, unreviewed []smartFolderFile

	for path, fi := range fileMap {
		if !fi.hasAIEvent || !isWhitelistedFile(path) {
			continue
		}

		relPath := path
		if rel, err := filepath.Rel(currentBrowseDir, path); err == nil {
			relPath = rel
		}

		sf := smartFolderFile{
			RelPath:   relPath,
			Name:      filepath.Base(path),
			ToolName:  fi.event.ToolName,
			TimeAgo:   formatTimeAgo(fi.event.Timestamp),
			SessionID: truncateSessionID(fi.event.SessionID),
		}

		if now.Sub(fi.latestAIEvt) < 24*time.Hour {
			recentAI = append(recentAI, sf)
		}
		if fi.latestView.Before(fi.latestAIEvt) {
			unreviewed = append(unreviewed, sf)
		}
	}

	sortByName := func(files []smartFolderFile) {
		sort.Slice(files, func(i, j int) bool {
			return files[i].Name < files[j].Name
		})
	}
	sortByName(recentAI)
	sortByName(unreviewed)

	var folders []smartFolder
	if len(recentAI) > 0 {
		folders = append(folders, smartFolder{Name: "Recent AI Edits", ID: "recent-ai", Files: recentAI})
	}
	if len(unreviewed) > 0 {
		folders = append(folders, smartFolder{Name: "Unreviewed", ID: "unreviewed", Files: unreviewed})
	}
	return folders
}

func generateSmartFolderHTML(folders []smartFolder) string {
	if len(folders) == 0 {
		return ""
	}
	var buf bytes.Buffer
	buf.WriteString(`<div class="smart-folders">`)
	for _, folder := range folders {
		buf.WriteString(fmt.Sprintf(`<div class="smart-folder" data-folder="%s">`, folder.ID))
		buf.WriteString(fmt.Sprintf(
			`<div class="tree-node smart-folder-header" onclick="toggleSmartFolder(this)" data-collapsed="false">`+
				`<span class="expand-icon">▼</span>`+
				`<span class="smart-folder-name">%s</span>`+
				`<span class="smart-folder-count">%d</span>`+
				`</div>`, folder.Name, len(folder.Files)))
		buf.WriteString(`<div class="tree-children smart-folder-children">`)
		for _, f := range folder.Files {
			escapedName := template.HTMLEscapeString(f.Name)
			escapedTool := template.HTMLEscapeString(f.ToolName)
			escapedSID := template.HTMLEscapeString(f.SessionID)
			escapedHref := template.URLQueryEscaper(f.RelPath)
			buf.WriteString(fmt.Sprintf(
				`<div class="tree-item"><div class="tree-node"><span class="tree-file smart-folder-file">`+
					`<a href="/view/%s">%s</a>`+
					`<span class="smart-folder-meta">`+
					`<span class="session-operation-badge session-operation-%s">%s</span>`+
					`<span class="smart-folder-time">%s</span>`+
					`<span class="smart-folder-sid">%s</span>`+
					`</span></span></div></div>`,
				escapedHref, escapedName, escapedTool, escapedTool,
				template.HTMLEscapeString(f.TimeAgo), escapedSID))
		}
		buf.WriteString(`</div></div>`)
	}
	buf.WriteString(`</div><div class="smart-folders-separator"></div>`)
	return buf.String()
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

	// Prepend smart folders (if AI tracking is active)
	if globalEventLog != nil {
		folders := generateSmartFolders()
		buf.WriteString(generateSmartFolderHTML(folders))
	}

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
