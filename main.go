package main

import (
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

	// Flags
	port        = flag.Int("port", 6419, "Port to serve on")
	openBrowser = flag.Bool("browser", true, "Open browser automatically")
	showVersion = flag.Bool("version", false, "Show version information")

	// State (global for single-user CLI simplicity; protected by mutexes)
	currentHTML  string
	htmlMutex    sync.RWMutex
	clients      = make(map[chan string]bool)
	clientsMutex sync.RWMutex

	// Browser mode
	browserMode   bool
	markdownFiles []string
	currentFile   string
	fileMutex     sync.RWMutex
	browseDir     string
	fileWatcher   watcherManager
	dirWatcher    watcherManager

	// Event debouncing for directory watching
	refreshTimer *time.Timer
	refreshMutex sync.Mutex

	// Templates, CSS, and JavaScript (loaded once at startup)
	githubCSS       string
	themeOverrides  string
	themeManagerJS  string
	singleFileTmpl  *template.Template
	fileBrowserTmpl *template.Template
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
}

// singleFileTemplateData is used for rendering single markdown files
type singleFileTemplateData struct {
	baseTemplateData
	Title   string
	Content template.HTML
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
}

// fileEventMessage is used for SSE notifications about file changes
type fileEventMessage struct {
	Type string `json:"type"` // "file_added" or "file_removed"
	Path string `json:"path"`
}

// newBaseTemplateData creates a baseTemplateData with embedded resources
func newBaseTemplateData() baseTemplateData {
	return baseTemplateData{
		GitHubCSS:      template.CSS(githubCSS),
		ThemeOverrides: template.CSS(themeOverrides),
		ThemeManagerJS: template.JS(themeManagerJS),
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
		watcher.Close()
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
		watcher.Close()
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
			watcher.Close()
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
		watcher.Close()
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
			if strings.HasPrefix(name, ".") && path != rootDir {
				return filepath.SkipDir
			}
			if name == "node_modules" || name == "vendor" || name == "dist" {
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

// registerBrowserModeRoutes registers HTTP routes for browser mode
func registerBrowserModeRoutes() {
	http.HandleFunc("/", withRecovery(serveBrowser))
	http.HandleFunc("/view/", withRecovery(serveFile))
	http.HandleFunc("/navigate", withRecovery(handleNavigate))
	http.HandleFunc("/delete", withRecovery(handleDelete))
	http.HandleFunc("/events", withRecovery(serveSSE))
}

// registerSingleFileModeRoutes registers HTTP routes for single file mode
func registerSingleFileModeRoutes() {
	http.HandleFunc("/", withRecovery(serveHTML))
	http.HandleFunc("/events", withRecovery(serveSSE))
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

	// Load HTML templates
	singleFileHTML, err := themeFS.ReadFile("theme/single-file.html")
	if err != nil {
		log.Fatalf("Failed to load single-file template: %v", err)
	}
	singleFileTmpl = template.Must(template.New("single-file").Parse(string(singleFileHTML)))

	fileBrowserHTML, err := themeFS.ReadFile("theme/file-browser.html")
	if err != nil {
		log.Fatalf("Failed to load file-browser template: %v", err)
	}
	fileBrowserTmpl = template.Must(template.New("file-browser").Parse(string(fileBrowserHTML)))
}

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Printf("peekm %s (commit: %s, built: %s)\n", version, commit, date)
		os.Exit(0)
	}

	if flag.NArg() < 1 {
		// Browser mode - show tree of all markdown files in current directory
		browserMode = true
		var err error
		browseDir, err = filepath.Abs(".")
		if err != nil {
			log.Fatalf("Error getting absolute path: %v", err)
		}
		markdownFiles = collectMarkdownFiles(browseDir)

		if len(markdownFiles) == 0 {
			fmt.Println("No markdown files found in current directory.")
			fmt.Println("\nUsage: peekm [options] <markdown-file|directory>")
			fmt.Println("\nOptions:")
			flag.PrintDefaults()
			os.Exit(1)
		}

		// Watch for new markdown files
		if err := dirWatcher.watchDirectory(browseDir); err != nil {
			log.Printf("Warning: Cannot watch directory for changes: %v", err)
		}

		registerBrowserModeRoutes()
	} else {
		// Check if argument is a file or directory
		filePath := flag.Arg(0)

		info, err := os.Stat(filePath)
		if os.IsNotExist(err) {
			log.Fatalf("Path not found: %s", filePath)
		}
		if err != nil {
			log.Fatalf("Error accessing path: %v", err)
		}

		if info.IsDir() {
			// Directory mode - browse markdown files in directory
			browserMode = true
			browseDir, err = filepath.Abs(filePath)
			if err != nil {
				log.Fatalf("Error getting absolute path: %v", err)
			}
			markdownFiles = collectMarkdownFiles(browseDir)

			if len(markdownFiles) == 0 {
				fmt.Printf("No markdown files found in directory: %s\n", filePath)
				os.Exit(1)
			}

			// Watch for new markdown files
			if err := dirWatcher.watchDirectory(browseDir); err != nil {
				log.Printf("Warning: Cannot watch directory for changes: %v", err)
			}

			registerBrowserModeRoutes()
		} else {
			// Single file mode
			// Initial render
			if err := renderMarkdown(filePath); err != nil {
				log.Fatalf("Error rendering markdown: %v", err)
			}

			// Watch for file changes
			if err := fileWatcher.watch(filePath); err != nil {
				log.Fatalf("Error watching file: %v", err)
			}

			registerSingleFileModeRoutes()
		}
	}

	addr := fmt.Sprintf("localhost:%d", *port)
	url := fmt.Sprintf("http://%s", addr)

	if browserMode {
		fmt.Printf("peekm file browser at %s\n", url)
		if browseDir == "." {
			fmt.Printf("Browsing current directory - found %d markdown file(s)\n", len(markdownFiles))
		} else {
			fmt.Printf("Browsing %s - found %d markdown file(s)\n", browseDir, len(markdownFiles))
		}
	} else {
		fmt.Printf("Serving %s at %s\n", flag.Arg(0), url)
	}
	fmt.Println("Press Ctrl+C to quit")

	if *openBrowser {
		go func() {
			time.Sleep(500 * time.Millisecond)
			openURL(url)
		}()
	}

	// Setup graceful shutdown
	server := &http.Server{
		Addr:         addr,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Handle shutdown signals
	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt, syscall.SIGTERM)
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

func renderMarkdown(filePath string) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	md := newMarkdownRenderer()

	var buf bytes.Buffer
	if err := md.Convert(content, &buf); err != nil {
		return err
	}

	var htmlBuf bytes.Buffer
	data := singleFileTemplateData{
		baseTemplateData: newBaseTemplateData(),
		Title:            filePath,
		Content:          template.HTML(buf.String()),
	}

	if err := singleFileTmpl.Execute(&htmlBuf, data); err != nil {
		return err
	}

	htmlMutex.Lock()
	currentHTML = htmlBuf.String()
	htmlMutex.Unlock()

	return nil
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
				log.Println("File modified, reloading...")
				if err := renderMarkdown(filePath); err != nil {
					log.Printf("Error rendering markdown: %v", err)
					continue
				}
				notifyClients()
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

					// Use json.Marshal for proper escaping (prevents JSON injection)
					msg := fileEventMessage{
						Type: "file_added",
						Path: relPath,
					}
					msgBytes, err := json.Marshal(msg)
					if err != nil {
						log.Printf("Error marshaling file added message: %v", err)
					} else {
						notifyClientsWithMessage(string(msgBytes))
					}

					// Schedule a refresh to properly sort the list and ensure consistency
					scheduleRefresh()
				}
			}

			// Handle REMOVE events for deleted/moved files
			if event.Op&fsnotify.Remove == fsnotify.Remove {
				if strings.HasSuffix(strings.ToLower(event.Name), ".md") {
					log.Printf("Markdown file changed: %s (refresh scheduled)", event.Name)
					scheduleRefresh()
				}
			}

			// Handle RENAME events for renamed files
			// Note: Rename generates two events - RENAME for old name, CREATE for new name
			if event.Op&fsnotify.Rename == fsnotify.Rename {
				if strings.HasSuffix(strings.ToLower(event.Name), ".md") {
					log.Printf("Markdown file changed: %s (refresh scheduled)", event.Name)
					scheduleRefresh()
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

// scheduleRefresh schedules a directory refresh with debouncing
// Multiple rapid calls within 300ms are batched into a single refresh
func scheduleRefresh() {
	refreshMutex.Lock()
	defer refreshMutex.Unlock()

	if refreshTimer != nil {
		refreshTimer.Stop()
	}

	refreshTimer = time.AfterFunc(300*time.Millisecond, func() {
		refreshFileList()
		refreshMutex.Lock()
		refreshTimer = nil
		refreshMutex.Unlock()
	})
}

// refreshFileList rescans the directory and notifies clients
func refreshFileList() {
	// Re-scan directory structure
	fileMutex.RLock()
	currentBrowseDir := browseDir
	fileMutex.RUnlock()

	newMarkdownFiles := collectMarkdownFiles(currentBrowseDir)

	// Update state thread-safely
	fileMutex.Lock()
	markdownFiles = newMarkdownFiles
	fileMutex.Unlock()

	log.Printf("Updated file list: %d markdown file(s)", len(newMarkdownFiles))

	// Note: No need to restart watcher - new directories are already added
	// via watcher.Add() in watchDirectoryWithContext() event handler

	// Note: No need to notify clients with reload - the file_added notification
	// is already sent and the frontend will insert the file dynamically
}

func serveHTML(w http.ResponseWriter, r *http.Request) {
	htmlMutex.RLock()
	html := currentHTML
	htmlMutex.RUnlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, html)
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

	clientChan := make(chan string)

	clientsMutex.Lock()
	clients[clientChan] = true
	clientCount := len(clients)
	clientsMutex.Unlock()

	log.Printf("SSE client connected from %s (total: %d)", r.RemoteAddr, clientCount)

	defer func() {
		clientsMutex.Lock()
		delete(clients, clientChan)
		clientCount := len(clients)
		clientsMutex.Unlock()
		close(clientChan)
		log.Printf("SSE client disconnected from %s (remaining: %d)", r.RemoteAddr, clientCount)
	}()

	// Send initial comment to establish connection
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	// Keep connection alive
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case message := <-clientChan:
			str := fmt.Sprintf("data: %s\n\n", message)
			if _, err := fmt.Fprint(w, str); err != nil {
				log.Printf("SSE write error from %s: %v", r.RemoteAddr, err)
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				log.Printf("SSE keepalive error from %s: %v", r.RemoteAddr, err)
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			log.Printf("SSE context cancelled for %s", r.RemoteAddr)
			return
		}
	}
}

func notifyClients() {
	notifyClientsWithMessage("reload")
}

func notifyClientsWithMessage(message string) {
	clientsMutex.RLock()
	clientCount := len(clients)
	clientsMutex.RUnlock()

	log.Printf("Notifying %d SSE client(s): %s", clientCount, message)

	clientsMutex.RLock()
	defer clientsMutex.RUnlock()

	for clientChan := range clients {
		select {
		case clientChan <- message:
		default:
		}
	}
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

	treeHTML := generateTreeHTML()

	subtitle := fmt.Sprintf("Found %d markdown file(s)", len(currentMarkdownFiles))
	if currentBrowseDir != "." {
		subtitle = fmt.Sprintf("%s - %d file(s)", currentBrowseDir, len(currentMarkdownFiles))
	}

	data := browserTemplateData{
		baseTemplateData: newBaseTemplateData(),
		Title:            "Markdown Files",
		Subtitle:         subtitle,
		TreeHTML:         template.HTML(treeHTML),
		BrowsePath:       currentBrowseDir,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := fileBrowserTmpl.Execute(w, data); err != nil {
		log.Printf("Template execution error: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
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

	// Delete the file
	if err := os.Remove(targetPath); err != nil {
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

	// Get current browse directory (thread-safe)
	fileMutex.RLock()
	currentBrowseDir := browseDir
	fileMutex.RUnlock()

	// Convert relative path to absolute by joining with browseDir
	var absFilePath string
	if filepath.IsAbs(filePath) {
		absFilePath = filePath
	} else {
		absFilePath = filepath.Join(currentBrowseDir, filePath)
	}

	// Clean the absolute path
	absFilePath = filepath.Clean(absFilePath)

	// Security: check if file is in our markdown files whitelist
	// This prevents directory traversal attacks by only allowing pre-scanned files
	fileMutex.RLock()
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

	data := browserTemplateData{
		baseTemplateData: newBaseTemplateData(),
		Title:            filepath.Base(absFilePath),
		Subtitle:         absFilePath,
		Content:          template.HTML(buf.String()),
		ShowBackButton:   true,
		BrowsePath:       currentBrowseDir,
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
	if err := fileBrowserTmpl.Execute(w, data); err != nil {
		log.Printf("Template execution error: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

func collectMarkdownFiles(rootDir string) []string {
	var files []string

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

		// Skip hidden directories and common build/dependency directories
		if info.IsDir() {
			name := info.Name()
			// Skip hidden dirs, but allow the root directory even if it starts with "."
			if strings.HasPrefix(name, ".") && path != rootDir {
				return filepath.SkipDir
			}
			if name == "node_modules" || name == "vendor" || name == "dist" {
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
	generateTreeHTMLRecursive(root, "", true, true, 0, &buf)
	return buf.String()
}

func generateTreeHTMLRecursive(node *fileNode, prefix string, isLast bool, isRoot bool, depth int, buf *bytes.Buffer) {
	if !isRoot {
		connector := "├── "
		if isLast {
			connector = "└── "
		}

		buf.WriteString(`<div class="tree-item"`)
		if depth > 0 {
			buf.WriteString(fmt.Sprintf(` data-depth="%d"`, depth))
		}
		buf.WriteString(`>`)
		buf.WriteString(fmt.Sprintf(`<span class="tree-connector">%s%s</span>`, prefix, connector))

		if node.isDir {
			// Collapse directories at depth >= 2 by default
			collapsed := depth >= 2
			buf.WriteString(fmt.Sprintf(`<span class="tree-directory%s" onclick="toggleDir(this)" data-collapsed="%t">`,
				func() string {
					if collapsed {
						return " collapsed"
					} else {
						return ""
					}
				}(),
				collapsed))
			// Only show arrow on collapsed folders (UX: reduce clutter)
			if collapsed {
				buf.WriteString(`<span class="expand-icon">▶</span> `)
			}
			buf.WriteString(fmt.Sprintf(`%s/</span>`, template.HTMLEscapeString(node.name)))
		} else {
			buf.WriteString(`<span class="tree-file">`)
			buf.WriteString(fmt.Sprintf(`<a href="/view/%s">%s</a>`, template.URLQueryEscaper(node.path), template.HTMLEscapeString(node.name)))
			buf.WriteString(fmt.Sprintf(`<span class="file-size">%s</span>`, formatSize(node.size)))
			buf.WriteString(`</span>`)
		}

		buf.WriteString(`</div>`)
	}

	// Print children
	if node.isDir && len(node.children) > 0 {
		childPrefix := prefix
		if !isRoot {
			if isLast {
				childPrefix += "    "
			} else {
				childPrefix += "│   "
			}
		}

		for i, child := range node.children {
			isLastChild := i == len(node.children)-1
			generateTreeHTMLRecursive(child, childPrefix, isLastChild, false, depth+1, buf)
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
	exec.Start()
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

func formatSize(size int64) string {
	kb := size / 1024
	if kb == 0 {
		return fmt.Sprintf("(%d bytes)", size)
	}
	return fmt.Sprintf("(%d KB)", kb)
}
