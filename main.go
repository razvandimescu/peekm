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
	"io"
	"log"
	"net/http"
	"net/url"
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
	"unicode/utf8"

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
	versionCheckTTL     = 24 * time.Hour
)

type versionCache struct {
	Latest    string `json:"latest"`
	CheckedAt int64  `json:"checked_at"`
}

var (
	// Build info (set via ldflags)
	version = "dev"
	commit  = "none"
	date    = "unknown"

	// Hardcoded directory exclusions (O(1) lookup)
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

	// Memory browser mode
	memoryMode   bool
	memoryFilter string

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
	transcriptTmpl         *template.Template
	transcriptPartialTmpl  *template.Template

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
	events := decodeSessionEvents(el.file)
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

	// Deduplicate: hook writes JSONL then POSTs; skip if already persisted
	for i := len(el.events) - 1; i >= 0 && i >= len(el.events)-10; i-- {
		e := &el.events[i]
		if e.SessionID == event.SessionID &&
			e.FilePath == event.FilePath &&
			e.ToolName == event.ToolName &&
			e.Timestamp.Sub(event.Timestamp).Abs() < 5*time.Second {
			return nil
		}
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
		el.events = el.events[len(el.events)-eventLogMaxInMemory:]
	}
	return nil
}

// claudePlansDir returns the ~/.claude/plans/ directory path, or "" if $HOME is unavailable.
func claudePlansDir() string {
	homeDir, _ := os.UserHomeDir()
	if homeDir == "" {
		return ""
	}
	return filepath.Join(homeDir, ".claude", "plans")
}

// plansCacheDir returns the ~/.cache/peekm/plans/ directory path, or "" if $HOME is unavailable.
func plansCacheDir() string {
	homeDir, _ := os.UserHomeDir()
	if homeDir == "" {
		return ""
	}
	return filepath.Join(homeDir, ".cache", "peekm", "plans")
}

// readPlanFile reads a plan file, falling back to the cache if the original is missing.
func readPlanFile(planPath string) ([]byte, error) {
	content, err := os.ReadFile(planPath)
	if err == nil {
		return content, nil
	}
	cacheDir := plansCacheDir()
	if cacheDir == "" {
		return nil, err
	}
	return os.ReadFile(filepath.Join(cacheDir, filepath.Base(planPath)))
}

// isPlanFile checks whether a path is under ~/.claude/plans/.
func isPlanFile(path string) bool {
	plansDir := claudePlansDir()
	return plansDir != "" && strings.HasPrefix(path, plansDir+string(os.PathSeparator))
}

// eventsForDir returns events under dir (plus plan files), newest first.
func (el *eventLog) eventsForDir(dir string) []SessionEvent {
	prefix := dir + string(filepath.Separator)
	var plansPrefix string
	if plansDir := claudePlansDir(); plansDir != "" {
		plansPrefix = plansDir + string(filepath.Separator)
	}

	el.mu.RLock()
	defer el.mu.RUnlock()
	var out []SessionEvent
	for i := len(el.events) - 1; i >= 0; i-- {
		evt := el.events[i]
		if strings.HasPrefix(evt.FilePath, prefix) || evt.FilePath == dir ||
			(plansPrefix != "" && strings.HasPrefix(evt.FilePath, plansPrefix)) {
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

// watchDirectories creates a single watcher for a pre-computed list of directories
// (no tree walk). Used by memory mode where directories are already known.
func (m *watcherManager) watchDirectories(dirs []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cancel != nil {
		m.cancel()
	}
	if m.current != nil {
		m.current.Close()
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	m.current = watcher

	for _, dir := range dirs {
		if addErr := watcher.Add(dir); addErr != nil {
			log.Printf("Warning: Cannot watch %s: %v", dir, addErr)
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
	http.HandleFunc("/transcript", withRecovery(serveTranscript))

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
	// Handle ~/... paths (files outside browseDir, e.g. plan files)
	if strings.HasPrefix(relativePath, "~/") {
		homeDir, _ := os.UserHomeDir()
		if homeDir != "" {
			return filepath.Clean(filepath.Join(homeDir, relativePath[2:]))
		}
	}

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
		"formatTimeAgo": formatTimeAgo,
		"pathEscape": func(s string) string {
			// Escape each segment individually, preserving /
			parts := strings.Split(s, "/")
			for i, p := range parts {
				parts[i] = url.PathEscape(p)
			}
			return strings.Join(parts, "/")
		},
		"toolIcon": toolIcon,
		"formatTime": func(ts string) string {
			for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
				if t, err := time.Parse(layout, ts); err == nil {
					return t.Local().Format("15:04")
				}
			}
			return ""
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
	// File-browser shell defines {{template "content" .}} — register the default content block
	fileBrowserContentHTML, err := themeFS.ReadFile("theme/file-browser-partial.html")
	if err != nil {
		log.Fatalf("Failed to load file-browser-partial template: %v", err)
	}

	// Full page: file-browser shell + file-browser content as "content" block + session panel
	fileBrowserTmpl = template.Must(template.New("file-browser").Funcs(funcMap).Parse(string(fileBrowserHTML)))
	template.Must(fileBrowserTmpl.New("content").Funcs(funcMap).Parse(string(fileBrowserContentHTML)))
	fileBrowserTmpl = template.Must(fileBrowserTmpl.Parse(string(sessionInfoPanelHTML)))

	// SPA partial: standalone file-browser-partial + session panel
	fileBrowserPartialTmpl = template.Must(template.New("file-browser-partial").Funcs(funcMap).Parse(string(fileBrowserContentHTML)))
	fileBrowserPartialTmpl = template.Must(fileBrowserPartialTmpl.Parse(string(sessionInfoPanelHTML)))

	// Timeline templates: full uses file-browser shell with timeline partial as "content"
	timelinePartialHTML, err := themeFS.ReadFile("theme/timeline-partial.html")
	if err != nil {
		log.Fatalf("Failed to load timeline-partial template: %v", err)
	}
	timelinePartialTmpl = template.Must(template.New("timeline-partial").Funcs(funcMap).Parse(string(timelinePartialHTML)))

	timelineTmpl = template.Must(template.New("timeline").Funcs(funcMap).Parse(string(fileBrowserHTML)))
	template.Must(timelineTmpl.New("content").Funcs(funcMap).Parse(string(timelinePartialHTML)))

	// Transcript templates: full uses file-browser shell with transcript partial as "content"
	transcriptPartialHTML, err := themeFS.ReadFile("theme/transcript-partial.html")
	if err != nil {
		log.Fatalf("Failed to load transcript-partial template: %v", err)
	}
	transcriptPartialTmpl = template.Must(template.New("transcript-partial").Funcs(funcMap).Parse(string(transcriptPartialHTML)))

	transcriptTmpl = template.Must(template.New("transcript").Funcs(funcMap).Parse(string(fileBrowserHTML)))
	template.Must(transcriptTmpl.New("content").Funcs(funcMap).Parse(string(transcriptPartialHTML)))
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
# peekm hook: Persist session events to JSONL, then notify running instance
json=$(cat)
session_id=$(echo "$json" | jq -r '.session_id // empty')
tool_name=$(echo "$json" | jq -r '.tool_name // empty')
file_path=$(echo "$json" | jq -r '.tool_input.file_path // .tool_input.notebook_path // empty')

[ -z "$session_id" ] || [ -z "$tool_name" ] || [ -z "$file_path" ] && exit 0

perm_mode=$(echo "$json" | jq -r '.permission_mode // empty')
tool_use_id=$(echo "$json" | jq -r '.tool_use_id // empty')
cwd=$(echo "$json" | jq -r '.cwd // empty')
content=$(echo "$json" | jq -r '.tool_input.content // empty')
ts=$(date -u +"%%Y-%%m-%%dT%%H:%%M:%%SZ")

event=$(jq -nc --arg sid "$session_id" --arg path "$file_path" \
    --arg tool "$tool_name" --arg perm "$perm_mode" \
    --arg tuid "$tool_use_id" --arg cwd "$cwd" --arg ts "$ts" \
    '{sid:$sid,path:$path,tool:$tool,perm:$perm,tuid:$tuid,cwd:$cwd,ts:$ts}|with_entries(select(.value!=""))')

# 1. Persist to event log (atomic append, works even if peekm not running)
mkdir -p ~/.peekm
echo "$event" >> ~/.peekm/events.jsonl 2>/dev/null

# 2. Best-effort notification to running peekm (scan common ports)
for port in %d 6419 8080 3000; do
    if echo "$file_path" | grep -q '\.claude/plans/.*\.md$' && [ -n "$content" ]; then
        payload=$(echo "$json" | jq -c '{session_id, tool_name, file_path: .tool_input.file_path, content: .tool_input.content, ts: "'"$ts"'"}')
        curl -sf -X POST -H 'Content-Type: application/json' \
            -d "$payload" \
            --max-time 0.05 "http://localhost:$port/hook/file-modified" >/dev/null 2>&1 && break
    else
        curl -sf -X POST -H 'Content-Type: application/json' \
            -d "$event" \
            --max-time 0.05 "http://localhost:$port/hook/file-modified" >/dev/null 2>&1 && break
    fi
done
`, *hookPort)

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

func findGitRoot(startDir string) (string, error) {
	dir := startDir
	for {
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("not a git repository (or any parent up to /)")
		}
		dir = parent
	}
}

func decodeSessionEvents(r io.Reader) []SessionEvent {
	var events []SessionEvent
	dec := json.NewDecoder(r)
	for dec.More() {
		var evt SessionEvent
		if err := dec.Decode(&evt); err != nil {
			continue // skip malformed lines, keep reading
		}
		events = append(events, evt)
	}
	return events
}

func runShowIgnored() {
	fmt.Println("Hardcoded exclusions:")
	fmt.Println("  .* (hidden directories, except .claude)")
	var excludedDirs []string
	for dir := range hardcodedExclusionsMap {
		excludedDirs = append(excludedDirs, dir)
	}
	sort.Strings(excludedDirs)
	for _, dir := range excludedDirs {
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

	plansDirPrefix := ""
	if plansDir := claudePlansDir(); plansDir != "" {
		plansDirPrefix = plansDir + string(os.PathSeparator)
	}

	cacheDir := plansCacheDir()

	for path, meta := range el.latestPerFile() {
		globalSessionStore.register(path, meta)
		// Whitelist plan files if original or cached copy exists
		if plansDirPrefix != "" && strings.HasPrefix(path, plansDirPrefix) && strings.HasSuffix(path, ".md") && !isWhitelistedFile(path) {
			_, origErr := os.Stat(path)
			if origErr != nil && cacheDir != "" {
				_, origErr = os.Stat(filepath.Join(cacheDir, filepath.Base(path)))
			}
			if origErr == nil {
				fileMutex.Lock()
				markdownFiles = append(markdownFiles, path)
				fileMutex.Unlock()
			}
		}
	}

	el.mu.RLock()
	n := len(el.events)
	el.mu.RUnlock()
	log.Printf("Loaded %d persisted session events", n)
}

// decodeProjectName resolves an encoded Claude Code project directory name
// (e.g. "-Users-rd-projects-rinkt-bot") back to the project's base name.
// Uses backtracking to handle hyphenated directory names (e.g. "my-projects").
func decodeProjectName(encoded string) string {
	s := strings.TrimPrefix(encoded, "-")
	if s == "" {
		return encoded
	}
	parts := strings.Split(s, "-")
	if name, ok := resolveDeepestDir(parts, 0, string(filepath.Separator)); ok {
		return name
	}
	return s
}

// resolveDeepestDir recursively tries all ways to split dash-separated parts
// into real directory path segments, returning the remainder as the project name.
func resolveDeepestDir(parts []string, start int, resolved string) (string, bool) {
	if start >= len(parts) {
		return filepath.Base(resolved), true
	}
	// Try consuming 1, 2, ... remaining parts as a single directory segment
	for n := 1; n <= len(parts)-start; n++ {
		segment := strings.Join(parts[start:start+n], "-")
		candidate := filepath.Join(resolved, segment)
		info, err := os.Stat(candidate)
		if err != nil || !info.IsDir() {
			continue
		}
		if name, ok := resolveDeepestDir(parts, start+n, candidate); ok {
			return name, true
		}
	}
	// No further directory resolution — remainder is the project name
	if start > 0 {
		return strings.Join(parts[start:], "-"), true
	}
	return "", false
}

// collectMemoryFiles scans ~/.claude/projects/*/memory/*.md and returns sorted absolute paths.
// If filter is non-empty, only projects whose decoded name contains the filter are included.
func collectMemoryFiles(filter string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	projectsDir := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		if filter != "" {
			decoded := decodeProjectName(entry.Name())
			if !strings.Contains(strings.ToLower(decoded), strings.ToLower(filter)) {
				continue
			}
		}

		memDir := filepath.Join(projectsDir, entry.Name(), "memory")
		mdFiles, err := filepath.Glob(filepath.Join(memDir, "*.md"))
		if err != nil || len(mdFiles) == 0 {
			continue
		}
		files = append(files, mdFiles...)
	}

	sort.Strings(files)
	return files
}

// uniqueParentDirs returns deduplicated parent directories of the given file paths.
func uniqueParentDirs(files []string) []string {
	seen := make(map[string]bool)
	var dirs []string
	for _, f := range files {
		dir := filepath.Dir(f)
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// serveAndWait starts the HTTP server, handles graceful shutdown, and blocks until exit.
func serveAndWait(addr, startURL string) {
	if version != "dev" {
		go checkLatestVersion()
	}
	if *openBrowser {
		go func() {
			time.Sleep(500 * time.Millisecond)
			openURL(startURL)
		}()
	}

	server := &http.Server{
		Addr:        addr,
		ReadTimeout: 15 * time.Second,
		// WriteTimeout intentionally omitted for SSE streaming endpoints
		// SSE connections are long-lived and should not have write timeouts
		IdleTimeout: 60 * time.Second,
	}

	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigint
		log.Println("\nShutting down gracefully...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		fileWatcher.close()
		dirWatcher.close()
		if globalEventLog != nil {
			globalEventLog.close()
		}
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("Server shutdown error: %v", err)
		}
	}()

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func runMemory(args []string) {
	memoryMode = true

	fs := flag.NewFlagSet("memory", flag.ExitOnError)
	memPort := fs.Int("port", 6419, "Port to serve on")
	memBrowser := fs.Bool("browser", true, "Open browser automatically")
	memDisableHook := fs.Bool("no-ai-tracking", false, "Disable AI session tracking")
	fs.Parse(args)

	if fs.NArg() > 0 {
		memoryFilter = fs.Arg(0)
	}

	*port = *memPort
	*openBrowser = *memBrowser
	*disableHook = *memDisableHook

	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Cannot determine home directory: %v", err)
	}
	browseDir = filepath.Join(home, ".claude", "projects")

	if !*disableHook {
		initSessionTracking()
	}

	markdownFiles = collectMemoryFiles(memoryFilter)
	if len(markdownFiles) == 0 {
		if memoryFilter != "" {
			fmt.Printf("No memory files found matching '%s'\n", memoryFilter)
		} else {
			fmt.Println("No memory files found in ~/.claude/projects/")
		}
		os.Exit(1)
	}

	dirs := uniqueParentDirs(markdownFiles)
	if len(dirs) > 0 {
		if err := dirWatcher.watchDirectories(dirs); err != nil {
			log.Printf("Warning: Cannot watch memory directories: %v", err)
		}
	}

	registerRoutes()

	addr := fmt.Sprintf("localhost:%d", *port)
	baseURL := fmt.Sprintf("http://%s", addr)

	fmt.Printf("peekm memory browser at %s\n", baseURL)
	fmt.Printf("Browsing %d project(s), %d memory file(s)\n", len(dirs), len(markdownFiles))
	if memoryFilter != "" {
		fmt.Printf("Filter: %s\n", memoryFilter)
	}
	fmt.Println("Press Ctrl+C to quit")

	serveAndWait(addr, baseURL)
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: peekm [options] [file|directory]\n")
		fmt.Fprintf(os.Stderr, "       peekm memory [options] [project-filter]\n")
		fmt.Fprintf(os.Stderr, "       peekm setup claude-code [--remove]\n")
		fmt.Fprintf(os.Stderr, "\nMarkdown viewer with AI session tracking.\n")
		fmt.Fprintf(os.Stderr, "\nSubcommands:\n")
		fmt.Fprintf(os.Stderr, "  memory    Browse Claude Code memory files across all projects\n")
		fmt.Fprintf(os.Stderr, "  setup     Configure integrations (e.g. Claude Code hooks)\n")
		fmt.Fprintf(os.Stderr, "\nOptions:\n")
		flag.PrintDefaults()
	}

	// Handle subcommands before flag.Parse()
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "setup":
			runSetup(os.Args[2:])
			return
		case "memory":
			runMemory(os.Args[2:])
			return
		}
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
		fmt.Fprintln(os.Stderr)
		flag.Usage()
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

	serveAndWait(addr, fullURL)
}

// tildeRelPath returns a path relative to baseDir, or ~/... if outside baseDir.
func tildeRelPath(absPath, baseDir string) string {
	if rel, err := filepath.Rel(baseDir, absPath); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	homeDir, _ := os.UserHomeDir()
	if homeDir != "" {
		if rel, err := filepath.Rel(homeDir, absPath); err == nil {
			return "~/" + rel
		}
	}
	return absPath
}

// getRelativePath converts absolute file path to relative path (thread-safe)
func getRelativePath(absPath string) string {
	fileMutex.RLock()
	defer fileMutex.RUnlock()
	if browseDir == "" {
		return absPath
	}
	return tildeRelPath(absPath, browseDir)
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

	var content []byte
	var readErr error
	if isPlanFile(validated) {
		content, readErr = readPlanFile(validated)
	} else {
		content, readErr = os.ReadFile(validated)
	}
	if readErr != nil {
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
	var content []byte
	if isPlanFile(filePath) {
		content, err = readPlanFile(filePath)
	} else {
		content, err = os.ReadFile(filePath)
	}
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

	if memoryMode {
		title = "Memory Browser"
		projectCount := len(uniqueParentDirs(currentMarkdownFiles))
		subtitle = fmt.Sprintf("%d project(s), %d file(s)", projectCount, len(currentMarkdownFiles))
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

// handlePlanFile caches plan content for durability and whitelists/broadcasts plan files.
// Always returns the canonical ~/.claude/plans/ path (never rewrites to cache path).
func handlePlanFile(filePath, content, sessionID string) string {
	if !strings.HasSuffix(filePath, ".md") {
		return filePath
	}
	plansDir := claudePlansDir()
	if plansDir == "" || !strings.HasPrefix(filePath, plansDir+string(os.PathSeparator)) {
		return filePath
	}

	// Cache plan content for durability (survives cleanup, handles remote)
	if content != "" {
		cacheDir := plansCacheDir()
		os.MkdirAll(cacheDir, 0755)
		localPath := filepath.Join(cacheDir, filepath.Base(filePath))
		if err := atomicWriteFile(localPath, content); err != nil {
			log.Printf("Warning: failed to cache plan file: %v", err)
		}
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
		// Long-form field names (legacy hook / plan file payloads)
		SessionID      string `json:"session_id"`
		ToolName       string `json:"tool_name"`
		FilePath       string `json:"file_path"`
		Content        string `json:"content"`
		PermissionMode string `json:"permission_mode"`
		ToolUseID      string `json:"tool_use_id"`
		CWD            string `json:"cwd"`
		TranscriptPath string `json:"transcript_path"`
		// Short-form field names (new hook writes SessionEvent JSON)
		SID  string `json:"sid"`
		Path string `json:"path"`
		Tool string `json:"tool"`
		Perm string `json:"perm"`
		TUID string `json:"tuid"`
		TS   string `json:"ts"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Merge short-form fields into long-form (short takes precedence if both present)
	coalesce(&req.SessionID, req.SID)
	coalesce(&req.FilePath, req.Path)
	coalesce(&req.ToolName, req.Tool)
	coalesce(&req.PermissionMode, req.Perm)
	coalesce(&req.ToolUseID, req.TUID)

	// Validate required fields
	if req.SessionID == "" || req.FilePath == "" {
		http.Error(w, "Missing required fields: session_id/sid and file_path/path", http.StatusBadRequest)
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
		Timestamp:      parseTimestampOrNow(req.TS),
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
	if memoryMode {
		http.Error(w, "Navigation disabled in memory browser mode", http.StatusForbidden)
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

	// Render the markdown file (with cache fallback for plan files)
	var content []byte
	var err error
	if isPlanFile(absFilePath) {
		content, err = readPlanFile(absFilePath)
	} else {
		content, err = os.ReadFile(absFilePath)
	}
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

	renderTemplate(w, r, data)
}

// Timeline template data types

type timelineTemplateData struct {
	baseTemplateData
	TreeHTML      template.HTML
	Title         string
	Subtitle      string
	BrowsePath    string
	Groups        []timelineDayGroup
	FilterSession string // non-empty when filtered by session ID
	SessionStats  *sessionFilterStats
	RepoInfo      *repoInfo
}

type repoInfo struct {
	Name   string // e.g. "peekm"
	Branch string // e.g. "main"
	Remote string // e.g. "github.com/razvandimescu/peekm"
}

type sessionFilterStats struct {
	FullID        string
	FileCount     int
	EditCount     int
	Duration      string
	Tools         string // e.g. "Edit: 14, Write: 3"
	HasTranscript bool
}

type timelineSession struct {
	SessionID     string // truncated 8 chars
	FullSessionID string
	Summary       string // first user prompt (truncated)
	Duration      string // e.g. "12m", "< 1s"
	FileCount     int
	EditCount     int
	Tools         []string // unique tool names
	HasTranscript bool
	IsActive      bool // newest event < 5min ago
	IsExpanded    bool // first session today starts expanded
	Events        []timelineEntry
	newestTime    time.Time
	oldestTime    time.Time
}

type timelineDayGroup struct {
	Label    string
	Sessions []timelineSession
}

type timelineEntry struct {
	FilePath      string // relative to browseDir (for display + view links)
	AbsPath       string // absolute path (for copy button)
	ToolName      string
	TimeAgo       string
	TimeISO       string
	SessionID     string // truncated for display
	FullSessionID string // full ID for linking
	IsViewable    bool   // true if file is in the markdown whitelist
	HasTranscript bool
	EditCount     int       // 1 = single event, >1 = aggregated
	oldestTime    time.Time // unexported, used for time range computation
	newestTime    time.Time // unexported, used for time range computation
}

func dayLabel(t time.Time) string {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	yesterday := today.AddDate(0, 0, -1)
	switch {
	case !t.Before(today):
		return "Today"
	case !t.Before(yesterday):
		return "Yesterday"
	default:
		return t.Format("Jan 2, 2006")
	}
}

func formatSessionDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return "< 1s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
}

type transcriptInfo struct {
	hasTranscript bool
	summary       string
}

func buildTranscriptCache(events []SessionEvent) map[string]transcriptInfo {
	cache := make(map[string]transcriptInfo)
	for _, evt := range events {
		if evt.SessionID == "" {
			continue
		}
		if _, checked := cache[evt.SessionID]; !checked {
			path := resolveTranscriptPath(evt.SessionID)
			cache[evt.SessionID] = transcriptInfo{
				hasTranscript: path != "",
				summary:       extractSessionSummary(path),
			}
		}
	}
	return cache
}

type sessionBuild struct {
	session *timelineSession
	files   map[string]bool
	tools   map[string]bool
}

func appendOrMergeEntry(sb *sessionBuild, evt SessionEvent, baseDir string, transcriptCache map[string]transcriptInfo) {
	sb.files[evt.FilePath] = true
	sb.tools[evt.ToolName] = true
	sb.session.EditCount++

	if sb.session.newestTime.IsZero() || evt.Timestamp.After(sb.session.newestTime) {
		sb.session.newestTime = evt.Timestamp
	}
	if sb.session.oldestTime.IsZero() || evt.Timestamp.Before(sb.session.oldestTime) {
		sb.session.oldestTime = evt.Timestamp
	}

	if n := len(sb.session.Events); n > 0 {
		prev := &sb.session.Events[n-1]
		if prev.AbsPath == evt.FilePath && prev.ToolName == evt.ToolName {
			prev.EditCount++
			prev.oldestTime = evt.Timestamp
			prev.TimeAgo = formatTimeRange(prev.newestTime, evt.Timestamp)
			return
		}
	}

	sb.session.Events = append(sb.session.Events, timelineEntry{
		FilePath:      tildeRelPath(evt.FilePath, baseDir),
		AbsPath:       evt.FilePath,
		ToolName:      evt.ToolName,
		TimeAgo:       formatTimeAgo(evt.Timestamp),
		TimeISO:       evt.Timestamp.Format(time.RFC3339),
		SessionID:     truncateSessionID(evt.SessionID),
		FullSessionID: evt.SessionID,
		IsViewable:    isWhitelistedFile(evt.FilePath),
		HasTranscript: transcriptCache[evt.SessionID].hasTranscript,
		EditCount:     1,
		newestTime:    evt.Timestamp,
		oldestTime:    evt.Timestamp,
	})
}

func groupEventsBySession(events []SessionEvent, baseDir string) []timelineSession {
	transcriptCache := buildTranscriptCache(events)

	sessionMap := make(map[string]*sessionBuild)
	var sessionOrder []string

	for _, evt := range events {
		sid := evt.SessionID
		if sid == "" {
			sid = "_unknown"
		}
		sb, exists := sessionMap[sid]
		if !exists {
			info := transcriptCache[sid]
			sb = &sessionBuild{
				session: &timelineSession{
					SessionID:     truncateSessionID(sid),
					FullSessionID: sid,
					Summary:       info.summary,
					HasTranscript: info.hasTranscript,
				},
				files: make(map[string]bool),
				tools: make(map[string]bool),
			}
			if sid == "_unknown" {
				sb.session.SessionID = "unknown"
				sb.session.FullSessionID = ""
			}
			sessionMap[sid] = sb
			sessionOrder = append(sessionOrder, sid)
		}
		appendOrMergeEntry(sb, evt, baseDir, transcriptCache)
	}

	sessions := make([]timelineSession, 0, len(sessionOrder))
	for _, sid := range sessionOrder {
		sb := sessionMap[sid]
		s := sb.session
		s.FileCount = len(sb.files)
		s.Duration = formatSessionDuration(s.newestTime.Sub(s.oldestTime))

		toolNames := make([]string, 0, len(sb.tools))
		for t := range sb.tools {
			toolNames = append(toolNames, t)
		}
		sort.Strings(toolNames)
		s.Tools = toolNames

		sessions = append(sessions, *s)
	}
	return sessions
}

func assignSessionsToDays(sessions []timelineSession) []timelineDayGroup {
	bucketMap := make(map[string]*timelineDayGroup)
	var bucketOrder []string

	for i := range sessions {
		label := dayLabel(sessions[i].newestTime)
		if _, exists := bucketMap[label]; !exists {
			bucketMap[label] = &timelineDayGroup{Label: label}
			bucketOrder = append(bucketOrder, label)
		}
		bucketMap[label].Sessions = append(bucketMap[label].Sessions, sessions[i])
	}

	groups := make([]timelineDayGroup, 0, len(bucketOrder))
	for _, label := range bucketOrder {
		groups = append(groups, *bucketMap[label])
	}
	return groups
}

func markActiveAndExpanded(groups []timelineDayGroup) {
	now := time.Now()
	marked := false
	for i := range groups {
		for j := range groups[i].Sessions {
			if now.Sub(groups[i].Sessions[j].newestTime) < 5*time.Minute {
				groups[i].Sessions[j].IsActive = true
			}
			if !marked {
				groups[i].Sessions[j].IsExpanded = true
				marked = true
			}
		}
	}
}

func buildSessionTimeline(events []SessionEvent, baseDir string) []timelineDayGroup {
	sessions := groupEventsBySession(events, baseDir)
	groups := assignSessionsToDays(sessions)
	markActiveAndExpanded(groups)
	return groups
}

func serveTimeline(w http.ResponseWriter, r *http.Request) {
	fileMutex.RLock()
	currentBrowseDir := browseDir
	fileMutex.RUnlock()

	filterSession := r.URL.Query().Get("session")

	var groups []timelineDayGroup
	var stats *sessionFilterStats

	if globalEventLog != nil {
		events := globalEventLog.eventsForDir(currentBrowseDir)

		// Filter by session if requested
		if filterSession != "" {
			var filtered []SessionEvent
			for _, evt := range events {
				if evt.SessionID == filterSession {
					filtered = append(filtered, evt)
				}
			}
			events = filtered
			stats = computeSessionStats(events, filterSession)
		}

		groups = buildSessionTimeline(events, currentBrowseDir)
	}

	title := "AI Timeline"
	subtitle := fmt.Sprintf("Session history for %s", currentBrowseDir)
	if filterSession != "" {
		title = fmt.Sprintf("Session %s", truncateSessionID(filterSession))
		subtitle = ""
	}

	data := timelineTemplateData{
		baseTemplateData: newBaseTemplateData(),
		TreeHTML:         template.HTML(generateTreeHTML()),
		Title:            title,
		Subtitle:         subtitle,
		BrowsePath:       currentBrowseDir,
		Groups:           groups,
		FilterSession:    filterSession,
		SessionStats:     stats,
		RepoInfo:         detectRepoInfo(currentBrowseDir),
	}

	renderTemplatePair(w, r, timelineTmpl, timelinePartialTmpl, data)
}

func computeSessionStats(events []SessionEvent, sessionID string) *sessionFilterStats {
	if len(events) == 0 {
		return &sessionFilterStats{FullID: sessionID}
	}
	files := make(map[string]bool)
	tools := make(map[string]int)
	var earliest, latest time.Time
	for _, evt := range events {
		files[evt.FilePath] = true
		tools[evt.ToolName]++
		if earliest.IsZero() || evt.Timestamp.Before(earliest) {
			earliest = evt.Timestamp
		}
		if evt.Timestamp.After(latest) {
			latest = evt.Timestamp
		}
	}

	dur := latest.Sub(earliest).Truncate(time.Second)
	durStr := dur.String()
	if dur == 0 {
		durStr = "< 1s"
	}

	// Format tool breakdown: "Edit: 14, Write: 3"
	var toolNames []string
	for t := range tools {
		toolNames = append(toolNames, t)
	}
	sort.Strings(toolNames)
	var toolParts []string
	for _, t := range toolNames {
		toolParts = append(toolParts, fmt.Sprintf("%s: %d", t, tools[t]))
	}

	return &sessionFilterStats{
		FullID:        sessionID,
		FileCount:     len(files),
		EditCount:     len(events),
		Duration:      durStr,
		Tools:         strings.Join(toolParts, ", "),
		HasTranscript: resolveTranscriptPath(sessionID) != "",
	}
}

func detectRepoInfo(dir string) *repoInfo {
	d, err := findGitRoot(dir)
	if err != nil {
		return nil
	}

	ri := &repoInfo{Name: filepath.Base(d)}

	// Read current branch from .git/HEAD
	if head, err := os.ReadFile(filepath.Join(d, ".git", "HEAD")); err == nil {
		s := strings.TrimSpace(string(head))
		if strings.HasPrefix(s, "ref: refs/heads/") {
			ri.Branch = strings.TrimPrefix(s, "ref: refs/heads/")
		}
	}

	// Read remote URL from git config
	ri.Remote = parseGitOriginURL(filepath.Join(d, ".git", "config"))

	return ri
}

func parseGitOriginURL(configPath string) string {
	cfg, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(cfg), "\n")
	inOrigin := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == `[remote "origin"]` {
			inOrigin = true
			continue
		}
		if inOrigin && strings.HasPrefix(trimmed, "[") {
			break
		}
		if inOrigin && strings.HasPrefix(trimmed, "url = ") {
			remote := strings.TrimPrefix(trimmed, "url = ")
			remote = strings.TrimSuffix(remote, ".git")
			if strings.HasPrefix(remote, "git@") {
				remote = strings.TrimPrefix(remote, "git@")
				remote = strings.Replace(remote, ":", "/", 1)
			} else if strings.HasPrefix(remote, "https://") {
				remote = strings.TrimPrefix(remote, "https://")
			}
			return remote
		}
	}
	return ""
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

// coalesce sets dst to alt if alt is non-empty and dst is empty.
func coalesce(dst *string, alt string) {
	if alt != "" {
		*dst = alt
	}
}

func parseTimestampOrNow(ts string) time.Time {
	if ts != "" {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			return parsed
		}
	}
	return time.Now()
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

// formatTimeRange returns a compact range like "29m - 31m ago" or a single value if both ends match.
func formatTimeRange(newest, oldest time.Time) string {
	n := formatTimeAgo(newest)
	o := formatTimeAgo(oldest)
	if n == o {
		return n
	}
	return strings.TrimSuffix(n, " ago") + " - " + o
}

type smartFolderFileInfo struct {
	event      SessionEvent
	hasAIEvent bool
}

// aggregateFileEvents groups events by file path, keeping the latest AI event per file.
func aggregateFileEvents(events []SessionEvent) map[string]*smartFolderFileInfo {
	fileMap := make(map[string]*smartFolderFileInfo)
	for _, evt := range events {
		fi, exists := fileMap[evt.FilePath]
		if !exists {
			fi = &smartFolderFileInfo{}
			fileMap[evt.FilePath] = fi
		}
		if !fi.hasAIEvent {
			fi.event = evt
			fi.hasAIEvent = true
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

	var recentAI []smartFolderFile

	for path, fi := range fileMap {
		if !fi.hasAIEvent || !isWhitelistedFile(path) {
			continue
		}

		if now.Sub(fi.event.Timestamp) >= 24*time.Hour {
			continue
		}

		relPath := tildeRelPath(path, currentBrowseDir)

		recentAI = append(recentAI, smartFolderFile{
			RelPath:   relPath,
			Name:      filepath.Base(path),
			ToolName:  fi.event.ToolName,
			TimeAgo:   formatTimeAgo(fi.event.Timestamp),
			SessionID: truncateSessionID(fi.event.SessionID),
		})
	}

	sort.Slice(recentAI, func(i, j int) bool {
		return recentAI[i].Name < recentAI[j].Name
	})

	if len(recentAI) > 0 {
		return []smartFolder{{Name: "Recent AI Edits", ID: "recent-ai", Files: recentAI}}
	}
	return nil
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
			escapedHref := pathEscapeSegments(f.RelPath)
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

// pathEscapeSegments escapes each path segment individually, preserving /
func pathEscapeSegments(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

type memoryProject struct {
	encoded   string
	decoded   string
	files     []string
	newestMod time.Time
}

// groupMemoryByProject groups memory files by their parent project directory,
// decodes project names, and returns them sorted by most recent modification.
func groupMemoryByProject(files []string, baseDir string) []*memoryProject {
	groups := make(map[string]*memoryProject)

	for _, f := range files {
		rel, err := filepath.Rel(baseDir, f)
		if err != nil {
			continue
		}
		parts := strings.SplitN(rel, string(filepath.Separator), 3)
		if len(parts) < 3 {
			continue
		}
		encoded := parts[0]

		g, ok := groups[encoded]
		if !ok {
			g = &memoryProject{encoded: encoded, decoded: decodeProjectName(encoded)}
			groups[encoded] = g
		}
		g.files = append(g.files, f)

		if info, err := os.Stat(f); err == nil && info.ModTime().After(g.newestMod) {
			g.newestMod = info.ModTime()
		}
	}

	sorted := make([]*memoryProject, 0, len(groups))
	for _, g := range groups {
		// Sort files: MEMORY.md first, rest alphabetical
		sort.Slice(g.files, func(i, j int) bool {
			bi, bj := filepath.Base(g.files[i]), filepath.Base(g.files[j])
			if bi == "MEMORY.md" {
				return true
			}
			if bj == "MEMORY.md" {
				return false
			}
			return bi < bj
		})
		sorted = append(sorted, g)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].newestMod.After(sorted[j].newestMod)
	})
	return sorted
}

// generateMemoryTreeHTML builds a two-level tree: decoded project names → memory files.
func generateMemoryTreeHTML() string {
	fileMutex.RLock()
	currentFiles := make([]string, len(markdownFiles))
	copy(currentFiles, markdownFiles)
	currentBrowseDir := browseDir
	activeFile := currentFile
	fileMutex.RUnlock()

	if len(currentFiles) == 0 {
		return ""
	}

	projects := groupMemoryByProject(currentFiles, currentBrowseDir)

	var buf bytes.Buffer
	for _, g := range projects {
		projNode := &fileNode{name: g.decoded, path: g.encoded, isDir: true}
		hasActive := false
		for _, f := range g.files {
			relPath := tildeRelPath(f, currentBrowseDir)
			projNode.children = append(projNode.children, &fileNode{name: filepath.Base(f), path: relPath})
			if f == activeFile {
				hasActive = true
			}
		}
		// depth=0 → expanded (collapsed := depth >= 1 is false)
		// depth=1 → collapsed (collapsed := depth >= 1 is true)
		depth := 1
		if hasActive {
			depth = 0
		}
		generateTreeHTMLRecursive(projNode, "", false, false, depth, false, &buf)
	}

	return buf.String()
}

func generateTreeHTML() string {
	if memoryMode {
		return generateMemoryTreeHTML()
	}

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

		// Make path relative to browse directory (~/... for out-of-browseDir files)
		relPath := tildeRelPath(absPath, absDir)

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
			buf.WriteString(fmt.Sprintf(`<a href="/view/%s">%s</a>`, pathEscapeSegments(node.path), template.HTMLEscapeString(node.name)))
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

func readCachedVersion(cachePath string) string {
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return ""
	}
	var cached versionCache
	if json.Unmarshal(data, &cached) != nil || cached.Latest == "" {
		return ""
	}
	if time.Now().Unix()-cached.CheckedAt >= int64(versionCheckTTL.Seconds()) {
		return ""
	}
	return cached.Latest
}

func fetchLatestVersion(cacheDir, cachePath string) string {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("https://registry.npmjs.org/@peekm/peekm/latest")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pkg); err != nil || pkg.Version == "" {
		return ""
	}
	if err := os.MkdirAll(cacheDir, 0755); err == nil {
		if data, err := json.Marshal(versionCache{Latest: pkg.Version, CheckedAt: time.Now().Unix()}); err == nil {
			os.WriteFile(cachePath, data, 0644) //nolint:errcheck
		}
	}
	return pkg.Version
}

func checkLatestVersion() {
	if fi, err := os.Stdout.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}
	cacheDir := filepath.Join(homeDir, ".peekm")
	cachePath := filepath.Join(cacheDir, "version-check.json")

	latest := readCachedVersion(cachePath)
	if latest == "" {
		latest = fetchLatestVersion(cacheDir, cachePath)
	}
	if latest != "" && latest != version {
		fmt.Printf("\nUpdate available: %s → %s — run: npm i -g @peekm/peekm\n", version, latest)
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

// ============================================================================
// Transcript Viewer
// ============================================================================

// transcriptTemplateData is used for rendering the transcript viewer
type transcriptTemplateData struct {
	baseTemplateData
	TreeHTML   template.HTML
	Title      string
	Subtitle   string
	BrowsePath string
	SessionID  string
	Turns      []transcriptTurn
	NotFound   bool
}

// transcriptTurn represents a single user or assistant turn in the conversation
type transcriptTurn struct {
	Role      string // "user" or "assistant"
	Blocks    []contentBlock
	Model     string
	Timestamp string
}

// imageData represents a base64-encoded image from a tool result
type imageData struct {
	MediaType string // e.g. "image/png"
	Data      string // base64-encoded
}

// contentBlock represents a piece of content within a turn
type contentBlock struct {
	Type            string        // "text", "tool_use", "tool_result", "thinking", "context_summary"
	HTML            template.HTML // rendered markdown (for text blocks)
	Text            string        // raw text (for thinking, tool input)
	ToolName        string        // for tool_use blocks
	ToolInput       string        // pretty-printed, truncated
	ToolID          string        // for pairing tool_use ↔ tool_result
	Result          *contentBlock // paired tool_result (nil if unpaired)
	ToolDisplayName string        // humanized name
	ToolServer      string        // MCP server prefix
	ToolInputHTML   template.HTML // structured rendering
	ItemCount       int           // for context_summary
	Images          []imageData   // for tool_result blocks containing images
}

// resolveTranscriptPath finds a Claude Code transcript by scanning project directories.
// Tries the current browseDir first, then falls back to scanning all project dirs.
func resolveTranscriptPath(sessionID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	projectsDir := filepath.Join(home, ".claude", "projects")
	fileName := sessionID + ".jsonl"

	// Try current browseDir first (fast path)
	fileMutex.RLock()
	dir := browseDir
	fileMutex.RUnlock()
	candidate := filepath.Join(projectsDir, strings.ReplaceAll(dir, "/", "-"), fileName)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}

	// Scan all project directories
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate = filepath.Join(projectsDir, entry.Name(), fileName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func isSystemNoise(text string) bool {
	return strings.Contains(text, "<local-command-caveat>") ||
		strings.Contains(text, "<command-name>") ||
		strings.Contains(text, "<local-command-stdout>") ||
		strings.HasPrefix(strings.TrimSpace(text), "[Request interrupted")
}

// extractSessionSummary reads the first real user message from a transcript JSONL.
// Skips system caveats, slash commands, and empty messages. Returns truncated text.
func extractSessionSummary(transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	for i := 0; i < 50; i++ { // scan at most 50 lines
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			break
		}
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.Type != "user" {
			continue
		}
		text := extractUserText(entry.Message.Content)
		if text == "" {
			continue
		}
		if isSystemNoise(text) {
			continue
		}
		text = strings.TrimSpace(text)
		// "Implement the following plan:\n\n# Title" → extract the heading
		if strings.HasPrefix(text, "Implement the following plan") {
			if title := extractPlanTitle(text); title != "" {
				return truncateString(title, 120)
			}
		}
		// Take first line only
		if idx := strings.IndexByte(text, '\n'); idx >= 0 {
			text = text[:idx]
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		return truncateString(text, 120)
	}
	return ""
}

func extractPlanTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			title := strings.TrimPrefix(line, "# ")
			// Strip "Plan: " prefix if present
			title = strings.TrimPrefix(title, "Plan: ")
			return title
		}
	}
	return ""
}

func extractUserText(content json.RawMessage) string {
	// Try string content first
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	// Try array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}

// pairToolResults attaches tool_result blocks to their matching tool_use blocks.
// Paired results are set on the tool_use's Result field and removed from the block list.
func pairToolResults(turns []transcriptTurn) []transcriptTurn {
	// Pass 1: index tool_use blocks by ToolID
	useIndex := make(map[string]*contentBlock)
	for i := range turns {
		for j := range turns[i].Blocks {
			if turns[i].Blocks[j].Type == "tool_use" && turns[i].Blocks[j].ToolID != "" {
				useIndex[turns[i].Blocks[j].ToolID] = &turns[i].Blocks[j]
			}
		}
	}

	// Pass 2: pair tool_results and remove from block lists
	for i := range turns {
		filtered := turns[i].Blocks[:0]
		for j := range turns[i].Blocks {
			b := &turns[i].Blocks[j]
			if b.Type == "tool_result" && b.ToolID != "" {
				if use, ok := useIndex[b.ToolID]; ok {
					use.Result = b
					continue // remove from block list
				}
			}
			filtered = append(filtered, *b)
		}
		turns[i].Blocks = filtered
	}
	return turns
}

// mergeConsecutiveTurns combines adjacent turns with the same role into one turn.
func mergeConsecutiveTurns(turns []transcriptTurn) []transcriptTurn {
	if len(turns) == 0 {
		return turns
	}
	merged := []transcriptTurn{turns[0]}
	for i := 1; i < len(turns); i++ {
		last := &merged[len(merged)-1]
		if turns[i].Role == last.Role {
			last.Blocks = append(last.Blocks, turns[i].Blocks...)
		} else {
			merged = append(merged, turns[i])
		}
	}
	return merged
}

// removeEmptyTurns filters out turns with no content blocks.
func removeEmptyTurns(turns []transcriptTurn) []transcriptTurn {
	filtered := turns[:0]
	for _, t := range turns {
		if len(t.Blocks) > 0 {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

// parseTranscript reads a Claude Code transcript JSONL file and returns conversation turns
func parseTranscript(path string) ([]transcriptTurn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	md := newMarkdownRenderer()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // 10MB max line

	var turns []transcriptTurn
	isFirstUser := true

	for scanner.Scan() {
		collapseCtx := isFirstUser
		turn, skip := parseTranscriptLine(scanner.Bytes(), md, collapseCtx)
		if skip {
			continue
		}
		if collapseCtx {
			isFirstUser = false
		}
		turns = append(turns, turn)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	turns = pairToolResults(turns)
	turns = removeEmptyTurns(turns)
	turns = mergeConsecutiveTurns(turns)
	return turns, nil
}

// transcriptLineEnvelope is the minimal structure for a transcript JSONL line
type transcriptLineEnvelope struct {
	Type      string          `json:"type"`
	IsMeta    bool            `json:"isMeta"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

// transcriptMsg is the message body within a transcript line
type transcriptMsg struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

// parseTranscriptLine parses a single JSONL line into a transcriptTurn.
// Returns (turn, skip). If skip is true, the line should be ignored.
func parseTranscriptLine(line []byte, md goldmark.Markdown, collapseToolResults bool) (transcriptTurn, bool) {
	var env transcriptLineEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return transcriptTurn{}, true
	}

	// Only keep user/assistant, skip meta
	if env.IsMeta || (env.Type != "user" && env.Type != "assistant") {
		return transcriptTurn{}, true
	}
	if len(env.Message) == 0 {
		return transcriptTurn{}, true
	}

	var msg transcriptMsg
	if err := json.Unmarshal(env.Message, &msg); err != nil {
		return transcriptTurn{}, true
	}

	blocks := parseContentBlocks(msg.Content, md, collapseToolResults && msg.Role == "user")
	if len(blocks) == 0 {
		return transcriptTurn{}, true
	}

	return transcriptTurn{
		Role:      msg.Role,
		Blocks:    blocks,
		Model:     msg.Model,
		Timestamp: env.Timestamp,
	}, false
}

// parseContentBlocks extracts content blocks from a message's content field
func parseContentBlocks(raw json.RawMessage, md goldmark.Markdown, collapseToolResults bool) []contentBlock {
	// Content can be a string (user prompt) or array of blocks
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if str == "" {
			return nil
		}
		return []contentBlock{{Type: "text", HTML: renderMarkdownToHTML(md, str)}}
	}

	var rawBlocks []json.RawMessage
	if err := json.Unmarshal(raw, &rawBlocks); err != nil {
		return nil
	}

	// Count tool_results for context collapse
	if collapseToolResults {
		if n := countToolResults(rawBlocks); n > 10 {
			return []contentBlock{{Type: "context_summary", ItemCount: n}}
		}
	}

	return convertRawBlocks(rawBlocks, md)
}

// countToolResults counts tool_result blocks in a raw block list
func countToolResults(rawBlocks []json.RawMessage) int {
	count := 0
	for _, rb := range rawBlocks {
		var peek struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(rb, &peek) == nil && peek.Type == "tool_result" {
			count++
		}
	}
	return count
}

// rawContentBlock matches the JSON structure of a Claude message content block
type rawContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
}

// convertRawBlocks parses raw JSON blocks into contentBlock values
func convertRawBlocks(rawBlocks []json.RawMessage, md goldmark.Markdown) []contentBlock {
	var blocks []contentBlock
	for _, rb := range rawBlocks {
		var peek rawContentBlock
		if json.Unmarshal(rb, &peek) != nil {
			continue
		}
		switch peek.Type {
		case "text":
			if peek.Text != "" {
				blocks = append(blocks, contentBlock{Type: "text", HTML: renderMarkdownToHTML(md, peek.Text)})
			}
		case "thinking":
			if peek.Thinking != "" {
				blocks = append(blocks, contentBlock{Type: "thinking", Text: truncateString(peek.Thinking, 4000)})
			}
		case "tool_use":
			displayName, server := humanizeToolName(peek.Name)
			blocks = append(blocks, contentBlock{
				Type:            "tool_use",
				ToolName:        peek.Name,
				ToolInput:       formatToolInput(peek.Input),
				ToolID:          peek.ID,
				ToolDisplayName: displayName,
				ToolServer:      server,
				ToolInputHTML:   formatStructuredToolInput(peek.Name, peek.Input),
			})
		case "tool_result":
			text, images := extractToolResultContent(peek.Content)
			if text != "" || len(images) > 0 {
				if text != "" {
					text = truncateString(text, 8000)
				}
				block := contentBlock{Type: "tool_result", ToolID: peek.ToolUseID, Images: images}
				if text != "" {
					block.HTML = renderMarkdownToHTML(md, text)
				}
				blocks = append(blocks, block)
			}
		}
	}
	return blocks
}

// renderMarkdownToHTML converts markdown text to HTML using goldmark
func renderMarkdownToHTML(md goldmark.Markdown, text string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(text), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(text))
	}
	return template.HTML(buf.String())
}

// formatToolInput pretty-prints tool input JSON, truncated to a reasonable size
func formatToolInput(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var v interface{}
	if err := json.Unmarshal(input, &v); err != nil {
		return truncateString(string(input), 2000)
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return truncateString(string(input), 2000)
	}
	return truncateString(string(pretty), 2000)
}

// humanizeToolName splits MCP tool names (mcp__server__action) into display name and server.
func humanizeToolName(name string) (string, string) {
	if strings.HasPrefix(name, "mcp__") {
		parts := strings.SplitN(name[5:], "__", 2)
		if len(parts) == 2 {
			return parts[1], parts[0]
		}
	}
	return name, ""
}

// toolIcon returns a Unicode icon for common tool names.
func toolIcon(name string) string {
	switch name {
	case "Bash":
		return "\u25B6" // ▶
	case "Read":
		return "\u2630" // ☰
	case "Edit":
		return "\u270E" // ✎
	case "Write":
		return "\u2714" // ✔
	case "Glob":
		return "\u2026" // …
	case "Grep":
		return "\u2315" // ⌕
	case "WebFetch", "WebSearch":
		return "\u2197" // ↗
	case "TaskCreate":
		return "\u002B" // +
	case "TaskUpdate":
		return "\u2611" // ☑
	case "TaskList", "TaskGet":
		return "\u2610" // ☐
	case "NotebookEdit":
		return "\u2338" // ⌸
	case "Agent":
		return "\u21BB" // ↻
	default:
		return "\u2699" // ⚙
	}
}

// toolInputStr extracts a string value from a parsed tool input map.
func toolInputStr(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// esc is a package-level alias for template.HTMLEscapeString, reducing repetition
// and the risk of forgetting to escape in HTML-building helpers.
var esc = template.HTMLEscapeString

// formatStructuredToolInput returns a structured HTML rendering for known tools.
// Returns empty HTML for unknown tools (template falls back to raw JSON).
func formatStructuredToolInput(toolName string, input json.RawMessage) template.HTML {
	if len(input) == 0 {
		return ""
	}
	var m map[string]interface{}
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	switch toolName {
	case "Bash":
		return formatBashInput(m)
	case "Glob":
		if pat := toolInputStr(m, "pattern"); pat != "" {
			return template.HTML(`<code class="transcript-structured-input">` + esc(pat) + `</code>`)
		}
	case "Read":
		return formatReadInput(m)
	case "Edit":
		return formatEditInput(m)
	case "Write":
		return formatWriteInput(m)
	case "Grep":
		return formatGrepInput(m)
	case "TaskCreate", "TaskUpdate", "WebSearch", "WebFetch", "NotebookEdit":
		return formatMiscToolInput(toolName, m)
	}
	return ""
}

func formatBashInput(m map[string]interface{}) template.HTML {
	cmd := toolInputStr(m, "command")
	if cmd == "" {
		return ""
	}
	var b strings.Builder
	if desc := toolInputStr(m, "description"); desc != "" {
		b.WriteString(`<div class="transcript-bash-description">`)
		b.WriteString(esc(desc))
		b.WriteString(`</div>`)
	}
	b.WriteString(`<pre class="transcript-structured-input transcript-bash-input"><code>$ `)
	b.WriteString(esc(cmd))
	b.WriteString(`</code></pre>`)
	return template.HTML(b.String())
}

func formatMiscToolInput(toolName string, m map[string]interface{}) template.HTML {
	switch toolName {
	case "TaskCreate":
		if subject := toolInputStr(m, "subject"); subject != "" {
			return template.HTML(`<span class="transcript-structured-input">` + esc(subject) + `</span>`)
		}
	case "TaskUpdate":
		id := toolInputStr(m, "taskId")
		status := toolInputStr(m, "status")
		if id != "" && status != "" {
			return template.HTML(`<span class="transcript-structured-input">#` + esc(id) + ` &#x2192; ` + esc(status) + `</span>`)
		}
	case "WebSearch":
		if query := toolInputStr(m, "query"); query != "" {
			return template.HTML(`<code class="transcript-structured-input">` + esc(query) + `</code>`)
		}
	case "WebFetch":
		if u := toolInputStr(m, "url"); u != "" {
			return template.HTML(`<code class="transcript-structured-input">` + esc(truncateString(u, 100)) + `</code>`)
		}
	case "NotebookEdit":
		if path := toolInputStr(m, "notebook_path"); path != "" {
			return template.HTML(`<span class="transcript-structured-input">` + esc(filepath.Base(path)) + `</span>`)
		}
	}
	return ""
}

func formatReadInput(m map[string]interface{}) template.HTML {
	fp := toolInputStr(m, "file_path")
	if fp == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<span class="transcript-structured-input" title="`)
	b.WriteString(esc(fp))
	b.WriteString(`">`)
	b.WriteString(esc(filepath.Base(fp)))
	if offset := toolInputStr(m, "offset"); offset != "" {
		b.WriteString(`<span class="transcript-structured-range">:`)
		b.WriteString(esc(offset))
		if limit := toolInputStr(m, "limit"); limit != "" {
			b.WriteByte('-')
			b.WriteString(esc(limit))
		}
		b.WriteString(`</span>`)
	}
	b.WriteString(`</span>`)
	return template.HTML(b.String())
}

func formatEditInput(m map[string]interface{}) template.HTML {
	fp := toolInputStr(m, "file_path")
	if fp == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<div class="transcript-structured-input" title="`)
	b.WriteString(esc(fp))
	b.WriteString(`"><span>`)
	b.WriteString(esc(filepath.Base(fp)))
	b.WriteString(`</span>`)
	old := toolInputStr(m, "old_string")
	new := toolInputStr(m, "new_string")
	if old != "" || new != "" {
		b.WriteString(`<pre class="transcript-mini-diff">`)
		if old != "" {
			b.WriteString(`<span class="diff-remove">- `)
			b.WriteString(esc(truncateString(old, 200)))
			b.WriteString("</span>\n")
		}
		if new != "" {
			b.WriteString(`<span class="diff-add">+ `)
			b.WriteString(esc(truncateString(new, 200)))
			b.WriteString(`</span>`)
		}
		b.WriteString(`</pre>`)
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

func formatWriteInput(m map[string]interface{}) template.HTML {
	fp := toolInputStr(m, "file_path")
	if fp == "" {
		return ""
	}
	content := toolInputStr(m, "content")
	lines := strings.Count(content, "\n") + 1
	label := fmt.Sprintf("%d line", lines)
	if lines != 1 {
		label += "s"
	}
	return template.HTML(`<span class="transcript-structured-input" title="` + esc(fp) + `">` + esc(filepath.Base(fp)) + ` <span class="transcript-structured-range">(` + label + `)</span></span>`)
}

func formatGrepInput(m map[string]interface{}) template.HTML {
	pat := toolInputStr(m, "pattern")
	if pat == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<span class="transcript-structured-input"><code>/`)
	b.WriteString(esc(pat))
	b.WriteString(`/</code>`)
	if p := toolInputStr(m, "path"); p != "" {
		b.WriteString(` in <span title="`)
		b.WriteString(esc(p))
		b.WriteString(`">`)
		b.WriteString(esc(filepath.Base(p)))
		b.WriteString(`</span>`)
	}
	b.WriteString(`</span>`)
	return template.HTML(b.String())
}

// extractToolResultContent extracts text and images from a tool_result content field.
// Content can be a plain string or an array of typed content blocks.
func extractToolResultContent(content json.RawMessage) (string, []imageData) {
	if len(content) == 0 {
		return "", nil
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
	}
	if json.Unmarshal(content, &parts) == nil {
		var buf strings.Builder
		var images []imageData
		for _, p := range parts {
			switch p.Type {
			case "text":
				buf.WriteString(p.Text)
			case "image":
				if p.Source.Type == "base64" && p.Source.MediaType != "" && p.Source.Data != "" {
					images = append(images, imageData{MediaType: p.Source.MediaType, Data: p.Source.Data})
				}
			}
		}
		return buf.String(), images
	}
	return "", nil
}

// truncateString truncates a string to maxLen runes, adding "..." if truncated.
func truncateString(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	i := 0
	for n := 0; n < maxLen; n++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i] + "..."
}

// serveTranscript handles GET /transcript?session=<id>
func serveTranscript(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		http.Error(w, "Missing session parameter", http.StatusBadRequest)
		return
	}

	fileMutex.RLock()
	currentBrowseDir := browseDir
	fileMutex.RUnlock()

	path := resolveTranscriptPath(sessionID)

	data := transcriptTemplateData{
		baseTemplateData: newBaseTemplateData(),
		TreeHTML:         template.HTML(generateTreeHTML()),
		Title:            "Transcript",
		Subtitle:         "Session " + truncateSessionID(sessionID),
		BrowsePath:       currentBrowseDir,
		SessionID:        sessionID,
	}

	if path == "" {
		data.NotFound = true
	} else if turns, err := parseTranscript(path); err != nil {
		data.NotFound = true
	} else {
		data.Turns = turns
		model := ""
		for _, t := range turns {
			if t.Role == "assistant" && t.Model != "" {
				model = t.Model
				break
			}
		}
		if model != "" {
			data.Subtitle = fmt.Sprintf("Session %s · %s · %d turns", truncateSessionID(sessionID), model, len(turns))
		} else {
			data.Subtitle = fmt.Sprintf("Session %s · %d turns", truncateSessionID(sessionID), len(turns))
		}
	}

	renderTemplatePair(w, r, transcriptTmpl, transcriptPartialTmpl, data)
}
