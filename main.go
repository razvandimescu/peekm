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
	"net"
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
		"target":       true,
		"__pycache__":  true,
	}

	// Flags
	port        = flag.Int("port", 6419, "Port to serve on")
	openBrowser = flag.Bool("browser", true, "Open browser automatically")
	showVersion = flag.Bool("version", false, "Show version information")
	showIgnored = flag.Bool("show-ignored", false, "Show all excluded directories and exit")
	disableHook = flag.Bool("no-ai-tracking", false, "Disable AI session tracking endpoint")
	demoMode    = flag.Bool("demo", false, "Demo mode (fake tunnel for public sharing)")

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
	memoryTmpl             *template.Template
	memoryPartialTmpl      *template.Template
	transcriptTmpl         *template.Template
	transcriptPartialTmpl  *template.Template
	sharedViewTmpl         *template.Template

	// SSE event replay buffer (50 events = ~2 min of AI file creation)
	globalEventBuffer = newEventBuffer(50)

	// Claude Code session tracking (5s TTL for hook-to-fsnotify correlation)
	globalSessionStore *sessionStore

	// Persistent event log (JSONL file for session history)
	globalEventLog *eventLog

	// Heartbeat tracker (last tool call per session, in-memory only)
	globalHeartbeats = newHeartbeatStore()

	// LAN share store (in-memory, dies on restart)
	globalShareStore *shareStore
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
	FilePath       string           // Relative path of displayed file (for edit/raw)
	SessionData    *SessionMetadata // Claude Code session info for this file
	IsPreview      bool             // true for HTML/SVG/TXT files (no edit mode)
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

// heartbeatStore tracks the last tool call per session (in-memory, not persisted)
type heartbeatStore struct {
	mu    sync.RWMutex
	beats map[string]heartbeat // session ID → last heartbeat
}

type heartbeat struct {
	ToolName  string
	Detail    string
	Timestamp time.Time
}

func newHeartbeatStore() *heartbeatStore {
	return &heartbeatStore{beats: make(map[string]heartbeat)}
}

func (hs *heartbeatStore) update(sessionID, toolName, detail string) {
	now := time.Now()
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.beats[sessionID] = heartbeat{ToolName: toolName, Detail: detail, Timestamp: now}
	// Lazy eviction: reap stale entries when map grows
	if len(hs.beats) > 50 {
		for id, hb := range hs.beats {
			if now.Sub(hb.Timestamp) > 10*time.Minute {
				delete(hs.beats, id)
			}
		}
	}
}

func (hs *heartbeatStore) get(sessionID string) (heartbeat, bool) {
	hs.mu.RLock()
	defer hs.mu.RUnlock()
	hb, ok := hs.beats[sessionID]
	return hb, ok
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
	PlanTitle      string    `json:"pt_title,omitempty"`
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
		origin := r.Header.Get("Origin")
		if origin == "" || origin == allowedLocal || origin == allowedLoopback {
			next(w, r)
			return
		}
		// Allow requests where Origin matches the Host (local DNS aliases like Numa)
		if host := r.Host; host != "" && (origin == "http://"+host || origin == "https://"+host) {
			next(w, r)
			return
		}
		log.Printf("CSRF: rejected cross-origin POST from %s", origin)
		http.Error(w, "Forbidden: cross-origin request", http.StatusForbidden)
	}
}

// localOnly rejects requests from non-localhost addresses
func localOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if host != "127.0.0.1" && host != "::1" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// registerRoutes registers all HTTP routes
func registerRoutes() {
	// Local-only routes (owner's browser)
	http.HandleFunc("/", localOnly(withRecovery(serveBrowser)))
	http.HandleFunc("/view/", localOnly(withRecovery(serveFile)))
	http.HandleFunc("/navigate", localOnly(withRecovery(withCSRFCheck(handleNavigate))))
	http.HandleFunc("/folder", localOnly(withRecovery(withCSRFCheck(handleCreateFolder))))
	http.HandleFunc("/delete", localOnly(withRecovery(withCSRFCheck(handleDelete))))
	http.HandleFunc("/move", localOnly(withRecovery(withCSRFCheck(handleMove))))
	http.HandleFunc("/raw/", localOnly(withRecovery(serveRaw)))
	http.HandleFunc("/save", localOnly(withRecovery(withCSRFCheck(handleSave))))
	http.HandleFunc("/download", localOnly(withRecovery(withCSRFCheck(handleDownload))))
	http.HandleFunc("/preview-content/", localOnly(withRecovery(servePreviewContent)))
	http.HandleFunc("/tree-html", localOnly(withRecovery(serveTreeHTML)))
	http.HandleFunc("/timeline", localOnly(withRecovery(serveTimeline)))
	http.HandleFunc("/memory", localOnly(withRecovery(serveMemory)))
	http.HandleFunc("/transcript", localOnly(withRecovery(serveTranscript)))

	// Share management (local only; CSRF applied per-method inside handleShare)
	http.HandleFunc("/share", localOnly(withRecovery(handleShare)))
	http.HandleFunc("/share/public", localOnly(withRecovery(withCSRFCheck(handleShareMakePublic))))

	// LAN-accessible routes (token-gated or non-sensitive)
	http.HandleFunc("/s/", withRecovery(serveSharedFile))
	http.HandleFunc("/events", withRecovery(serveSSE))

	// AI session tracking endpoint (always on unless --no-ai-tracking)
	if !*disableHook {
		http.HandleFunc("/hook/file-modified", localOnly(withRecovery(handleClaudeHook)))
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

// isMemoryFile checks if an absolute path is a valid Claude Code memory file
// (~/.claude/projects/<project>/memory/<file>.md) or a project CLAUDE.md
// (<project>/.claude/CLAUDE.md) within $HOME. Pattern-only check, no stat.
func isMemoryFile(absPath string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	if !strings.HasPrefix(absPath, home+string(filepath.Separator)) {
		return false
	}

	// Memory file: ~/.claude/projects/<project>/memory/<file>.md
	projectsDir := filepath.Join(home, ".claude", "projects")
	if strings.HasPrefix(absPath, projectsDir+string(filepath.Separator)) {
		rel, err := filepath.Rel(projectsDir, absPath)
		if err != nil {
			return false
		}
		parts := strings.Split(rel, string(filepath.Separator))
		return len(parts) == 3 && parts[1] == "memory" && strings.HasSuffix(parts[2], ".md")
	}

	// Project CLAUDE.md: <dir>/.claude/CLAUDE.md (must be under $HOME)
	return filepath.Base(absPath) == "CLAUDE.md" && filepath.Base(filepath.Dir(absPath)) == ".claude"
}

// isWhitelistedFile checks if a path is in the current markdownFiles whitelist (thread-safe),
// falling back to isMemoryFile for Claude Code memory files.
func isWhitelistedFile(path string) bool {
	fileMutex.RLock()
	defer fileMutex.RUnlock()
	for _, f := range markdownFiles {
		if f == path {
			return true
		}
	}
	return isMemoryFile(path)
}

// mustReadThemeFile reads a file from themeFS or fatally logs
func mustReadThemeFile(name string) string {
	data, err := themeFS.ReadFile(name)
	if err != nil {
		log.Fatalf("Failed to load %s: %v", name, err)
	}
	return string(data)
}

// templateFuncMap returns the shared template function map
func templateFuncMap() template.FuncMap {
	return template.FuncMap{
		"formatISO": func(t time.Time) string {
			return t.Format(time.RFC3339)
		},
		"formatTimeAgo": formatTimeAgo,
		"pathEscape":    pathEscapeSegments,
		"toolIcon":      toolIcon,
		"formatTime": func(ts string) string {
			if t, ok := parseTimestamp(ts); ok {
				return t.Local().Format("15:04")
			}
			return ""
		},
	}
}

func init() {
	githubCSS = mustReadThemeFile("theme/github-markdown.css")
	themeOverrides = mustReadThemeFile("theme/theme-overrides.css")
	themeManagerJS = mustReadThemeFile("theme/theme-manager.js")
	editorJS = mustReadThemeFile("theme/editor.js")
	navigationJS = mustReadThemeFile("theme/navigation.js")

	funcMap := templateFuncMap()
	sessionInfoPanelHTML := mustReadThemeFile("theme/session-info-panel.html")
	fileBrowserHTML := mustReadThemeFile("theme/file-browser.html")
	fileBrowserContentHTML := mustReadThemeFile("theme/file-browser-partial.html")

	// Full page: file-browser shell + content block + session panel
	fileBrowserTmpl = template.Must(template.New("file-browser").Funcs(funcMap).Parse(fileBrowserHTML))
	template.Must(fileBrowserTmpl.New("content").Funcs(funcMap).Parse(fileBrowserContentHTML))
	fileBrowserTmpl = template.Must(fileBrowserTmpl.Parse(sessionInfoPanelHTML))

	// SPA partial: standalone file-browser-partial + session panel
	fileBrowserPartialTmpl = template.Must(template.New("file-browser-partial").Funcs(funcMap).Parse(fileBrowserContentHTML))
	fileBrowserPartialTmpl = template.Must(fileBrowserPartialTmpl.Parse(sessionInfoPanelHTML))

	// Timeline
	timelinePartialHTML := mustReadThemeFile("theme/timeline-partial.html")
	timelinePartialTmpl = template.Must(template.New("timeline-partial").Funcs(funcMap).Parse(timelinePartialHTML))
	timelineTmpl = template.Must(template.New("timeline").Funcs(funcMap).Parse(fileBrowserHTML))
	template.Must(timelineTmpl.New("content").Funcs(funcMap).Parse(timelinePartialHTML))

	// Transcript
	transcriptPartialHTML := mustReadThemeFile("theme/transcript-partial.html")
	transcriptPartialTmpl = template.Must(template.New("transcript-partial").Funcs(funcMap).Parse(transcriptPartialHTML))
	transcriptTmpl = template.Must(template.New("transcript").Funcs(funcMap).Parse(fileBrowserHTML))
	template.Must(transcriptTmpl.New("content").Funcs(funcMap).Parse(transcriptPartialHTML))

	// Memory
	memoryPartialHTML := mustReadThemeFile("theme/memory-partial.html")
	memoryPartialTmpl = template.Must(template.New("memory-partial").Funcs(funcMap).Parse(memoryPartialHTML))
	memoryTmpl = template.Must(template.New("memory").Funcs(funcMap).Parse(fileBrowserHTML))
	template.Must(memoryTmpl.New("content").Funcs(funcMap).Parse(memoryPartialHTML))

	// Shared view (standalone, not using file-browser shell)
	sharedViewHTML := mustReadThemeFile("theme/shared-view.html")
	sharedViewTmpl = template.Must(template.New("shared-view").Funcs(funcMap).Parse(sharedViewHTML))
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

	plansDir := claudePlansDir()
	plansDirPrefix := ""
	if plansDir != "" {
		plansDirPrefix = plansDir + string(os.PathSeparator)
	}

	cacheDir := plansCacheDir()

	tracked := el.latestPerFile()
	for path, meta := range tracked {
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

	scanUntrackedPlans(tracked, plansDir, cacheDir)

	el.mu.RLock()
	n := len(el.events)
	el.mu.RUnlock()
	log.Printf("Loaded %d persisted session events", n)
}

// scanUntrackedPlans discovers plan files in ~/.claude/plans/ that have no
// corresponding event in events.jsonl, whitelists them for viewing, and caches
// them for durability. No timeline events are created — the owning session's
// transcript already provides timeline visibility.
func scanUntrackedPlans(tracked map[string]*SessionMetadata, plansDir, cacheDir string) {
	if plansDir == "" {
		return
	}
	entries, err := os.ReadDir(plansDir)
	if err != nil {
		return
	}
	if cacheDir != "" {
		os.MkdirAll(cacheDir, 0755)
	}
	var count int
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		absPath := filepath.Join(plansDir, entry.Name())
		if _, exists := tracked[absPath]; exists {
			continue
		}
		if cacheDir != "" {
			content, err := os.ReadFile(absPath)
			if err != nil {
				continue
			}
			_ = atomicWriteFile(filepath.Join(cacheDir, entry.Name()), string(content))
		}
		if !isWhitelistedFile(absPath) {
			fileMutex.Lock()
			markdownFiles = append(markdownFiles, absPath)
			fileMutex.Unlock()
		}
		count++
	}
	if count > 0 {
		log.Printf("Discovered %d untracked plan file(s)", count)
	}
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

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: peekm [options] [file|directory]\n")
		fmt.Fprintf(os.Stderr, "       peekm setup claude-code [--remove]\n")
		fmt.Fprintf(os.Stderr, "       peekm setup autostart [--remove]\n")
		fmt.Fprintf(os.Stderr, "\nMarkdown viewer with AI session tracking.\n")
		fmt.Fprintf(os.Stderr, "\nSubcommands:\n")
		fmt.Fprintf(os.Stderr, "  setup     Configure integrations and system service\n")
		fmt.Fprintf(os.Stderr, "\nOptions:\n")
		flag.PrintDefaults()
	}

	// Handle subcommands before flag.Parse()
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "setup":
			runSetup(os.Args[2:])
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
		autoSetupClaudeHooks()
		initSessionTracking()
	}

	globalShareStore = newShareStore()
	go func() {
		for range time.Tick(5 * time.Minute) {
			globalShareStore.reapExpired()
		}
	}()

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

	addr := fmt.Sprintf("0.0.0.0:%d", *port)
	url := fmt.Sprintf("http://localhost:%d", *port)

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
func sendFileEvent(msg fileEventMessage) {
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling %s message: %v", msg.Type, err)
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
		sendFileEvent(fileEventMessage{Type: "file_added", Path: getRelativePath(filePath), Session: sessionID})
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
	sendFileEvent(fileEventMessage{Type: "file_removed", Path: getRelativePath(filePath)})
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

	absFilePath := cleanInputPath(req.Path)

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

	var filePath string
	if defaultFile != "" {
		filePath = tildeRelPath(defaultFile, currentBrowseDir)
	}

	data := browserTemplateData{
		baseTemplateData: newBaseTemplateData(),
		Title:            title,
		Subtitle:         subtitle,
		TreeHTML:         template.HTML(treeHTML),
		Content:          content,
		ShowBackButton:   showBackButton,
		BrowsePath:       currentBrowseDir,
		FilePath:         filePath,
	}

	renderTemplate(w, r, data)
}

// handlePlanFile caches plan content for durability and whitelists/broadcasts plan files.
// Returns (canonical path, plan title). Title is empty for non-plan files.
func handlePlanFile(filePath, content, sessionID string) (string, string) {
	if !strings.HasSuffix(filePath, ".md") {
		return filePath, ""
	}
	plansDir := claudePlansDir()
	if plansDir == "" || !strings.HasPrefix(filePath, plansDir+string(os.PathSeparator)) {
		return filePath, ""
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
	planTitle := extractPlanTitle(content)
	sendFileEvent(fileEventMessage{
		Type:      "file_modified",
		Path:      getRelativePath(filePath),
		Session:   sessionID,
		PlanTitle: planTitle,
	})
	return filePath, planTitle
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
		SID    string `json:"sid"`
		Path   string `json:"path"`
		Tool   string `json:"tool"`
		Perm   string `json:"perm"`
		TUID   string `json:"tuid"`
		TS     string `json:"ts"`
		Detail string `json:"detail"`
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
	if req.SessionID == "" {
		http.Error(w, "Missing required field: session_id/sid", http.StatusBadRequest)
		return
	}

	// Heartbeat: tool call without file_path (non-edit tools like Read, Bash, Grep)
	globalHeartbeats.update(req.SessionID, req.ToolName, req.Detail)
	if req.FilePath == "" {
		detail := req.Detail
		if len(detail) > 80 {
			detail = detail[:80] + "..."
		}
		if detail != "" {
			log.Printf("Heartbeat %s: %s — %s", truncateSessionID(req.SessionID), req.ToolName, detail)
		} else {
			log.Printf("Heartbeat %s: %s (no detail)", truncateSessionID(req.SessionID), req.ToolName)
		}
		w.WriteHeader(http.StatusOK)
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

	var planTitle string
	req.FilePath, planTitle = handlePlanFile(req.FilePath, req.Content, req.SessionID)

	// Register session mapping for file (after path rewrite so plan files use local path)
	globalSessionStore.register(req.FilePath, metadata)

	// Persist to event log
	if globalEventLog != nil {
		evt := sessionEventFrom(metadata, req.FilePath)
		evt.PlanTitle = planTitle
		if err := globalEventLog.append(evt); err != nil {
			log.Printf("Warning: failed to persist session event: %v", err)
		}
	}

	log.Printf("AI session %s tracked for: %s (mode: %s)", truncateSessionID(req.SessionID), req.FilePath, req.PermissionMode)

	w.WriteHeader(http.StatusOK)
}

func handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name   string `json:"name"`
		Parent string `json:"parent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		http.Error(w, "Folder name cannot be empty", http.StatusBadRequest)
		return
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") || strings.ContainsRune(name, 0) {
		http.Error(w, "Invalid folder name", http.StatusBadRequest)
		return
	}

	fileMutex.RLock()
	root := browseDir
	fileMutex.RUnlock()

	parentDir := filepath.Join(root, req.Parent)
	validatedParent, err := validateAndResolvePath(parentDir)
	if err != nil {
		http.Error(w, "Access denied", http.StatusForbidden)
		return
	}
	// Ensure parent is within the browse directory (not just $HOME)
	validatedRoot, _ := validateAndResolvePath(root)
	if validatedRoot != "" && !strings.HasPrefix(validatedParent, validatedRoot) {
		http.Error(w, "Access denied", http.StatusForbidden)
		return
	}
	absPath := filepath.Join(validatedParent, name)

	if err := os.Mkdir(absPath, 0755); err != nil {
		if os.IsExist(err) {
			http.Error(w, "Folder already exists", http.StatusConflict)
		} else {
			http.Error(w, fmt.Sprintf("Failed to create folder: %v", err), http.StatusInternalServerError)
		}
		return
	}

	log.Printf("Created folder: %s", absPath)
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

// validateMoveSource resolves and validates a move source path.
// Returns the validated path, file info, and error message (empty on success).
func validateMoveSource(relPath, root, validatedRoot string) (string, os.FileInfo, string) {
	validated, err := validateAndResolvePath(filepath.Join(root, relPath))
	if err != nil || !strings.HasPrefix(validated, validatedRoot) {
		return "", nil, "Access denied"
	}
	info, err := os.Stat(validated)
	if err != nil {
		return "", nil, "Source not found"
	}
	if !info.IsDir() && !isWhitelistedFile(validated) {
		return "", nil, "File not found or access denied"
	}
	return validated, info, ""
}

// validateMoveDest resolves and validates a move destination directory.
func validateMoveDest(relPath, root, validatedRoot string) (string, string) {
	validated, err := validateAndResolvePath(filepath.Join(root, relPath))
	if err != nil || !strings.HasPrefix(validated, validatedRoot) {
		return "", "Access denied"
	}
	if info, err := os.Stat(validated); err != nil || !info.IsDir() {
		return "", "Destination is not a directory"
	}
	return validated, ""
}

func handleMove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Source string `json:"source"`
		Dest   string `json:"dest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	fileMutex.RLock()
	root := browseDir
	fileMutex.RUnlock()

	validatedRoot, err := validateAndResolvePath(root)
	if err != nil {
		http.Error(w, "Access denied", http.StatusForbidden)
		return
	}

	validatedSource, sourceInfo, errMsg := validateMoveSource(req.Source, root, validatedRoot)
	if errMsg != "" {
		http.Error(w, errMsg, http.StatusForbidden)
		return
	}

	validatedDest, errMsg := validateMoveDest(req.Dest, root, validatedRoot)
	if errMsg != "" {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}

	// Prevent circular move (directory into itself or its subtree)
	sep := string(filepath.Separator)
	if sourceInfo.IsDir() && strings.HasPrefix(validatedDest+sep, validatedSource+sep) {
		http.Error(w, "Cannot move a folder into itself", http.StatusBadRequest)
		return
	}

	destPath := filepath.Join(validatedDest, filepath.Base(validatedSource))
	if filepath.Dir(validatedSource) == validatedDest {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"newPath": req.Source})
		return
	}
	// Pre-check only for files — os.Rename silently overwrites files on POSIX.
	// For directories, os.Rename fails naturally (ENOTEMPTY/ENOTDIR).
	if !sourceInfo.IsDir() {
		if _, err := os.Stat(destPath); err == nil {
			http.Error(w, "File already exists at destination", http.StatusConflict)
			return
		}
	}

	if err := os.Rename(validatedSource, destPath); err != nil {
		http.Error(w, fmt.Sprintf("Move failed: %v", err), http.StatusInternalServerError)
		return
	}

	fileMutex.Lock()
	markdownFiles = collectMarkdownFiles(browseDir)
	if currentFile == validatedSource {
		currentFile = destPath
	} else if sourceInfo.IsDir() && strings.HasPrefix(currentFile, validatedSource+sep) {
		currentFile = filepath.Join(destPath, currentFile[len(validatedSource):])
	}
	fileMutex.Unlock()

	newRelPath := getRelativePath(destPath)
	log.Printf("Moved: %s → %s", req.Source, newRelPath)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"newPath": newRelPath})
}

func serveFile(w http.ResponseWriter, r *http.Request) {
	filePath := strings.TrimPrefix(r.URL.Path, "/view/")
	filePath = strings.TrimPrefix(filePath, "/")
	filePath = filepath.Clean(filePath)
	absFilePath := resolveFilePath(filePath)

	fileMutex.RLock()
	currentBrowseDir := browseDir
	fileMutex.RUnlock()

	if !isWhitelistedFile(absFilePath) {
		// Serve co-located assets (images, CSS, JS) referenced by markdown files
		ext := strings.ToLower(filepath.Ext(absFilePath))
		if _, ok := allowedAssetExts[ext]; ok {
			if validated, err := validateAndResolvePath(absFilePath); err == nil && isWithinDir(validated, currentBrowseDir) {
				http.ServeFile(w, r, validated)
				return
			}
		}
		http.NotFound(w, r)
		return
	}

	ext := strings.ToLower(filepath.Ext(absFilePath))
	isPreview := shareableRawExts[ext]

	var contentHTML template.HTML
	if isPreview {
		contentHTML = renderPreviewContent(absFilePath, filePath, ext)
	} else {
		var err error
		contentHTML, err = renderMarkdownContent(absFilePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	var treeHTML string
	if !isPartialRequest(r) {
		treeHTML = generateTreeHTML()
	}

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
		Content:          contentHTML,
		ShowBackButton:   true,
		BrowsePath:       currentBrowseDir,
		SessionData:      sessionData,
		IsPreview:        isPreview,
	}

	fileMutex.Lock()
	oldFile := currentFile
	currentFile = absFilePath
	fileMutex.Unlock()

	if oldFile != absFilePath {
		if err := fileWatcher.watch(absFilePath); err != nil {
			log.Printf("Error watching file: %v", err)
		}
	}

	renderTemplate(w, r, data)
}

func renderMarkdownContent(absFilePath string) (template.HTML, error) {
	var content []byte
	var err error
	if isPlanFile(absFilePath) {
		content, err = readPlanFile(absFilePath)
	} else {
		content, err = os.ReadFile(absFilePath)
	}
	if err != nil {
		return "", err
	}
	md := newMarkdownRenderer()
	var buf bytes.Buffer
	if err := md.Convert(content, &buf); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

func renderPreviewContent(absPath, relPath, ext string) template.HTML {
	escapedPath := pathEscapeSegments(relPath)
	switch ext {
	case ".html", ".htm":
		return template.HTML(fmt.Sprintf(
			`<iframe src="/preview-content/%s" style="width:100%%; height:calc(100vh - 200px); border:1px solid var(--borderColor-muted); border-radius:6px; background:white;"></iframe>`,
			escapedPath))
	case ".svg":
		return template.HTML(fmt.Sprintf(
			`<div style="text-align:center; padding:16px;"><img src="/preview-content/%s" style="max-width:100%%; max-height:calc(100vh - 200px);" alt="%s"></div>`,
			escapedPath, esc(filepath.Base(absPath))))
	case ".txt":
		content, err := os.ReadFile(absPath)
		if err != nil {
			return ""
		}
		return template.HTML(fmt.Sprintf(
			`<pre style="white-space:pre-wrap; font-family:var(--font-mono); padding:16px; background:var(--bgColor-default); border-radius:6px; border:1px solid var(--borderColor-muted); overflow:auto; max-height:calc(100vh - 200px);">%s</pre>`,
			esc(string(content))))
	default:
		return ""
	}
}

// servePreviewContent serves HTML/SVG/TXT files and co-located assets for iframe embedding.
func servePreviewContent(w http.ResponseWriter, r *http.Request) {
	relPath := strings.TrimPrefix(r.URL.Path, "/preview-content/")
	if relPath == "" {
		http.NotFound(w, r)
		return
	}

	cleaned := filepath.Clean(relPath)
	absPath := resolveFilePath(cleaned)

	validated, err := validateAndResolvePath(absPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Directory request: serve index.html (handles http.ServeFile's index.html redirect)
	if info, statErr := os.Stat(validated); statErr == nil && info.IsDir() {
		for _, idx := range []string{"index.html", "index.htm"} {
			indexPath := filepath.Join(validated, idx)
			if isWhitelistedFile(indexPath) {
				http.ServeFile(w, r, indexPath)
				return
			}
		}
		http.NotFound(w, r)
		return
	}

	if isWhitelistedFile(validated) {
		http.ServeFile(w, r, validated)
		return
	}

	// Co-located assets: allowed extensions within browseDir
	ext := strings.ToLower(filepath.Ext(validated))
	if _, ok := allowedAssetExts[ext]; !ok {
		http.NotFound(w, r)
		return
	}

	fileMutex.RLock()
	dir := browseDir
	fileMutex.RUnlock()

	if !isWithinDir(validated, dir) {
		http.NotFound(w, r)
		return
	}

	http.ServeFile(w, r, validated)
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

// isCollectableFile returns true for file types shown in the sidebar tree.
func isCollectableFile(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".md") {
		return true
	}
	return shareableRawExts[filepath.Ext(lower)]
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

		if !info.IsDir() && isCollectableFile(info.Name()) {
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

// parseTimestamp parses a timestamp string using RFC3339Nano then RFC3339.
func parseTimestamp(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func parseTimestampOrNow(ts string) time.Time {
	if t, ok := parseTimestamp(ts); ok {
		return t
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

func generateTreeHTML() string {
	// Get state snapshot (thread-safe)
	fileMutex.RLock()
	currentBrowseDir := browseDir
	currentMarkdownFiles := make([]string, len(markdownFiles))
	copy(currentMarkdownFiles, markdownFiles)
	fileMutex.RUnlock()

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

		ensureDirChain(strings.Split(filepath.Dir(relPath), string(filepath.Separator)), dirNodes)

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

	// Add real directories that aren't already in the tree (user-created empty folders)
	addRealDirs(absDir, dirNodes)

	// Clean phantom dirs (intermediate path nodes with no file descendants that don't exist on disk)
	cleanEmptyDirs(root, absDir)
	sortTree(root)

	// Generate HTML
	var buf bytes.Buffer

	// Prepend smart folders or tracking hint (if AI tracking is active)
	if globalEventLog != nil {
		folders := generateSmartFolders()
		if html := generateSmartFolderHTML(folders); html != "" {
			buf.WriteString(html)
		} else {
			buf.WriteString(`<div class="ai-tracking-hint">` +
				`<svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor">` +
				`<path d="M1.5 8a6.5 6.5 0 1 0 13 0 6.5 6.5 0 0 0-13 0ZM8 0a8 8 0 1 1 0 16A8 8 0 0 1 8 0Zm.5 4.75a.75.75 0 0 0-1.5 0v3.5a.75.75 0 0 0 .37.65l2.5 1.5a.75.75 0 1 0 .76-1.3L8.5 7.82V4.75Z"/>` +
				`</svg> AI edits will appear here` +
				`</div><div class="smart-folders-separator"></div>`)
		}
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
			buf.WriteString(fmt.Sprintf(`<div class="tree-node" draggable="true" data-dir-path="%s"><span class="tree-directory" onclick="toggleDir(this)" data-path="%s">`,
				template.HTMLEscapeString(node.path), template.HTMLEscapeString(node.path)))

			// Chevron icon
			if collapsed {
				buf.WriteString(`<span class="expand-icon">▶</span>`)
			} else {
				buf.WriteString(`<span class="expand-icon">▼</span>`)
			}

			jsPath := strings.ReplaceAll(template.HTMLEscapeString(node.path), "'", "&#39;")
			buf.WriteString(fmt.Sprintf(`<span class="dir-name">%s</span></span>`+
				`<button class="tree-folder-action" onclick="event.stopPropagation();startNewFolder('%s')" title="New Folder">+</button></div>`,
				template.HTMLEscapeString(node.name), jsPath))

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
			buf.WriteString(fmt.Sprintf(`<div class="tree-node" draggable="true" data-file-path="%s"><span class="tree-file">`,
				template.HTMLEscapeString(node.path)))
			buf.WriteString(fmt.Sprintf(`<a href="/view/%s" draggable="false">%s</a>`, pathEscapeSegments(node.path), template.HTMLEscapeString(node.name)))
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

func ensureDirChain(parts []string, dirNodes map[string]*fileNode) {
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
			node := &fileNode{name: part, path: currentPath, isDir: true}
			dirNodes[currentPath] = node
			if parent, ok := dirNodes[parentPath]; ok {
				parent.children = append(parent.children, node)
			}
		}
	}
}

func addRealDirs(absDir string, dirNodes map[string]*fileNode) {
	customPatterns := getIgnorePatterns(absDir)
	filepath.Walk(absDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		if path == absDir {
			return nil
		}
		if isExcludedDir(info.Name(), customPatterns) {
			return filepath.SkipDir
		}
		relPath, err := filepath.Rel(absDir, path)
		if err != nil {
			return nil
		}
		if _, exists := dirNodes[relPath]; exists {
			return nil
		}
		ensureDirChain(strings.Split(relPath, string(filepath.Separator)), dirNodes)
		return nil
	})
}

func cleanEmptyDirs(node *fileNode, browseRoot string) bool {
	if !node.isDir {
		return true // Keep files
	}

	// Recursively clean children
	kept := make([]*fileNode, 0)
	for _, child := range node.children {
		if cleanEmptyDirs(child, browseRoot) {
			kept = append(kept, child)
		}
	}
	node.children = kept

	if len(node.children) > 0 || node.name == "." {
		return true
	}
	// Keep empty dirs that actually exist on disk (user-created)
	if browseRoot != "" && node.path != "" {
		absPath := filepath.Join(browseRoot, node.path)
		if info, err := os.Stat(absPath); err == nil && info.IsDir() {
			return true
		}
	}
	return false
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
