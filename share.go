package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// shareableRawExts are file types that can be shared directly (served as-is, no markdown rendering).
var shareableRawExts = map[string]bool{
	".html": true, ".htm": true, ".svg": true, ".txt": true,
}

// allowedAssetExts maps extensions to content types for share asset passthrough.
// Only these file types can be served as assets alongside a shared HTML file.
var allowedAssetExts = map[string]string{
	".js":    "application/javascript",
	".mjs":   "application/javascript",
	".css":   "text/css",
	".json":  "application/json",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".svg":   "image/svg+xml",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".map":   "application/json",
	".wasm":  "application/wasm",
}

// isWithinDir checks if path is contained within baseDir after resolving symlinks.
func isWithinDir(path, baseDir string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	resolvedBase, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		return false
	}
	return strings.HasPrefix(resolved, resolvedBase+string(filepath.Separator))
}

// shareEntry represents one active share
type shareEntry struct {
	Token           string
	FilePath        string
	ResolvedBaseDir string // cached filepath.EvalSymlinks(filepath.Dir(FilePath))
	ExpiresAt       time.Time
	CreatedAt       time.Time
}

// shareStore maintains active shares keyed by token
type shareStore struct {
	mu      sync.RWMutex
	entries map[string]*shareEntry
	tunnels map[string]context.CancelFunc // token → cancel
}

func newShareStore() *shareStore {
	return &shareStore{
		entries: make(map[string]*shareEntry),
		tunnels: make(map[string]context.CancelFunc),
	}
}

func generateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("failed to generate share token: %v", err)
	}
	return hex.EncodeToString(b)
}

func (s *shareStore) findByPath(filePath string) (*shareEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.entries {
		if e.FilePath == filePath && time.Now().Before(e.ExpiresAt) {
			return e, true
		}
	}
	return nil, false
}

func (s *shareStore) create(filePath string, ttl time.Duration) *shareEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.FilePath == filePath && time.Now().Before(e.ExpiresAt) {
			return e
		}
	}
	resolvedBase, _ := filepath.EvalSymlinks(filepath.Dir(filePath))
	entry := &shareEntry{
		Token:           generateToken(),
		FilePath:        filePath,
		ResolvedBaseDir: resolvedBase,
		ExpiresAt:       time.Now().Add(ttl),
		CreatedAt:       time.Now(),
	}
	s.entries[entry.Token] = entry
	return entry
}

func (s *shareStore) get(token string) (*shareEntry, bool) {
	s.mu.RLock()
	entry, ok := s.entries[token]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.ExpiresAt) {
		s.cancelTunnel(token)
		s.mu.Lock()
		delete(s.entries, token)
		s.mu.Unlock()
		return nil, false
	}
	return entry, true
}

func (s *shareStore) revoke(token string) {
	s.cancelTunnel(token)
	s.mu.Lock()
	delete(s.entries, token)
	s.mu.Unlock()
}

func (s *shareStore) startTunnelForToken(token string) {
	s.mu.Lock()
	if _, exists := s.tunnels[token]; exists {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.tunnels[token] = cancel
	s.mu.Unlock()
	go startTunnel(ctx, token, *port)
}

func (s *shareStore) fakeTunnelForToken(token string) {
	s.mu.Lock()
	s.tunnels[token] = func() {} // no-op cancel
	s.mu.Unlock()
}

func (s *shareStore) hasTunnel(token string) bool {
	s.mu.RLock()
	_, exists := s.tunnels[token]
	s.mu.RUnlock()
	return exists
}

func (s *shareStore) reapExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for token, entry := range s.entries {
		if now.After(entry.ExpiresAt) {
			if cancel, ok := s.tunnels[token]; ok {
				cancel()
				delete(s.tunnels, token)
			}
			delete(s.entries, token)
		}
	}
}

func (s *shareStore) cancelTunnel(token string) {
	s.mu.Lock()
	if cancel, ok := s.tunnels[token]; ok {
		cancel()
		delete(s.tunnels, token)
	}
	s.mu.Unlock()
}

var lanIP string
var lanIPOnce sync.Once

func detectLANIP() string {
	lanIPOnce.Do(func() {
		ifaces, err := net.Interfaces()
		if err != nil {
			return
		}
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil && !ipnet.IP.IsLoopback() {
					lanIP = ipnet.IP.String()
					return
				}
			}
		}
	})
	return lanIP
}

func buildShareURL(token string) string {
	host := detectLANIP()
	if host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("http://%s:%d/s/%s", host, *port, token)
}

type shareResponse struct {
	Active    bool   `json:"active"`
	Token     string `json:"token,omitempty"`
	URL       string `json:"url,omitempty"`
	PublicURL string `json:"public_url,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

func newShareResponse(entry *shareEntry) shareResponse {
	resp := shareResponse{
		Active:    true,
		Token:     entry.Token,
		URL:       buildShareURL(entry.Token),
		ExpiresAt: entry.ExpiresAt.Format(time.RFC3339),
	}
	if globalShareStore.hasTunnel(entry.Token) {
		resp.PublicURL = fmt.Sprintf("https://%s/%s", relayHost, entry.Token)
	}
	return resp
}

func cleanInputPath(raw string) string {
	return resolveFilePath(filepath.Clean(strings.TrimPrefix(strings.TrimSpace(raw), "/")))
}

// handleShare dispatches GET (status), POST (create), DELETE (revoke) for LAN sharing
func handleShare(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		handleShareStatus(w, r)
	case http.MethodPost:
		withCSRFCheck(handleShareCreate).ServeHTTP(w, r)
	case http.MethodDelete:
		withCSRFCheck(handleShareRevoke).ServeHTTP(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// isShareableFile checks if a file can be shared: either whitelisted markdown or
// an HTML/SVG/text file within the browse directory.
func isShareableFile(absPath string) bool {
	if isWhitelistedFile(absPath) {
		return true
	}
	ext := strings.ToLower(filepath.Ext(absPath))
	if !shareableRawExts[ext] {
		return false
	}
	fileMutex.RLock()
	dir := browseDir
	fileMutex.RUnlock()
	return isWithinDir(absPath, dir)
}

func handleShareCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	absPath, err := validateAndResolvePath(cleanInputPath(req.Path))
	if err != nil || !isShareableFile(absPath) {
		http.Error(w, "file not found or not accessible", http.StatusNotFound)
		return
	}
	entry := globalShareStore.create(absPath, 1*time.Hour)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(newShareResponse(entry))
}

func handleShareRevoke(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	globalShareStore.revoke(req.Token)
	w.WriteHeader(http.StatusNoContent)
}

func handleShareMakePublic(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	entry, ok := globalShareStore.get(req.Token)
	if !ok {
		http.Error(w, "share not found", http.StatusNotFound)
		return
	}
	if *demoMode {
		globalShareStore.fakeTunnelForToken(entry.Token)
	} else {
		globalShareStore.startTunnelForToken(entry.Token)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(newShareResponse(entry))
}

func handleShareStatus(w http.ResponseWriter, r *http.Request) {
	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		http.Error(w, "missing path parameter", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	absPath, err := validateAndResolvePath(cleanInputPath(filePath))
	if err != nil {
		json.NewEncoder(w).Encode(shareResponse{})
		return
	}
	found, ok := globalShareStore.findByPath(absPath)
	if !ok {
		json.NewEncoder(w).Encode(shareResponse{})
		return
	}
	json.NewEncoder(w).Encode(newShareResponse(found))
}

// sharedViewData is used for rendering the shared file view
type sharedViewData struct {
	baseTemplateData
	Content   template.HTML
	Token     string
	ExpiresAt string
	FileName  string
	FilePath  string // relative path for SSE event filtering
	TitleHead string // lead heading up to and including its separator (or the whole title)
	TitleTail string // accented remainder after the last separator, empty when there is none
	ReadMin   int    // estimated reading time in minutes
	IsExpired bool
	IsTunnel  bool // true when served via tunnel (suppresses SSE)
}

// headingSeparators split a title into a plain lead and an accented tail, longest first.
var headingSeparators = []string{" — ", " – ", " - ", ": ", " · "}

// headingInlineStripper removes emphasis/code markers so the hero title renders as plain text.
var headingInlineStripper = strings.NewReplacer("`", "", "*", "")

// firstMarkdownH1 returns the text of the first ATX H1, skipping fenced code blocks.
// Returns "" when the document has no top-level heading.
func firstMarkdownH1(src []byte) string {
	inFence := false
	for _, line := range strings.Split(string(src), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || !strings.HasPrefix(t, "# ") {
			continue
		}
		cleaned := headingInlineStripper.Replace(strings.TrimPrefix(t, "# "))
		return strings.Trim(cleaned, " #")
	}
	return ""
}

// splitHeading divides a title so the tail (after the last separator) can be accented.
// The separator stays with the head so the template can render head + <em>tail</em>.
func splitHeading(title string) (head, tail string) {
	for _, sep := range headingSeparators {
		if i := strings.LastIndex(title, sep); i >= 0 {
			return title[:i+len(sep)], strings.TrimSpace(title[i+len(sep):])
		}
	}
	return title, ""
}

// readingMinutes estimates reading time at 200 words/min, never below one minute.
func readingMinutes(src []byte) int {
	if m := len(strings.Fields(string(src))) / 200; m > 1 {
		return m
	}
	return 1
}

func serveSharedFile(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/s/")
	parts := strings.SplitN(path, "/", 2)
	token := parts[0]
	assetPath := ""
	if len(parts) == 2 {
		assetPath = parts[1]
	}

	if token == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	entry, ok := globalShareStore.get(token)
	if !ok {
		data := sharedViewData{
			baseTemplateData: newBaseTemplateData(),
			FileName:         "Expired Link",
			IsExpired:        true,
		}
		var renderBuf bytes.Buffer
		if err := sharedViewTmpl.Execute(&renderBuf, data); err != nil {
			http.Error(w, "This shared link has expired.", http.StatusGone)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusGone)
		renderBuf.WriteTo(w)
		return
	}

	ext := strings.ToLower(filepath.Ext(entry.FilePath))

	// Raw markdown source download (the ".md" action on the shared view). Path-based,
	// not a query param, so it survives the relay — which forwards path suffixes but
	// drops query strings.
	if assetPath == "raw" && !shareableRawExts[ext] {
		serveSharedSource(w, r, entry)
		return
	}

	// Vendored libraries (DOCX export deps), embedded in the binary and served
	// same-origin so the shared view needs no third-party CDN. Reserved prefix,
	// checked before user-asset passthrough.
	if name := strings.TrimPrefix(assetPath, "_vendor/"); name != assetPath {
		serveVendoredAsset(w, r, name)
		return
	}

	// Asset passthrough: /s/{token}/style.css
	if assetPath != "" {
		serveSharedAsset(w, r, entry, assetPath)
		return
	}

	// HTML/SVG/text files: serve raw content (no peekm wrapper)
	if shareableRawExts[ext] {
		serveSharedRawFile(w, r, entry, ext)
		return
	}

	// Markdown: render through goldmark with peekm shared-view template
	serveSharedMarkdown(w, r, entry, token)
}

// serveSharedRawFile serves HTML/SVG/text files as-is (no peekm wrapper).
func serveSharedRawFile(w http.ResponseWriter, r *http.Request, entry *shareEntry, ext string) {
	// Redirect to trailing slash so relative URLs resolve correctly
	if (ext == ".html" || ext == ".htm") && !strings.HasSuffix(r.URL.Path, "/") {
		http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
		return
	}
	ct := mime.TypeByExtension(ext)
	if ct == "" {
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	http.ServeFile(w, r, entry.FilePath)
}

// serveSharedSource serves the raw markdown source as a download. The link's download
// attribute supplies the filename through the relay (which strips Content-Disposition);
// the header still covers direct LAN access and CLI clients.
func serveSharedSource(w http.ResponseWriter, r *http.Request, entry *shareEntry) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(entry.FilePath)))
	http.ServeFile(w, r, entry.FilePath)
}

// serveSharedMarkdown renders a markdown file through goldmark with the shared-view template.
func serveSharedMarkdown(w http.ResponseWriter, r *http.Request, entry *shareEntry, token string) {
	content, err := os.ReadFile(entry.FilePath)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	md := newMarkdownRenderer()
	var buf bytes.Buffer
	if err := md.Convert(preprocessMermaid(content), &buf); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	if isPartialRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(buf.Bytes())
		return
	}
	fileName := filepath.Base(entry.FilePath)
	title := firstMarkdownH1(content)
	if title == "" {
		title = fileName
	}
	head, tail := splitHeading(title)
	data := sharedViewData{
		baseTemplateData: newBaseTemplateData(),
		Content:          template.HTML(buf.String()),
		Token:            token,
		ExpiresAt:        entry.ExpiresAt.Format(time.RFC3339),
		FileName:         fileName,
		FilePath:         entry.FilePath,
		TitleHead:        head,
		TitleTail:        tail,
		ReadMin:          readingMinutes(content),
		IsTunnel:         r.Header.Get("X-Tunnel") == "true",
	}
	var renderBuf bytes.Buffer
	if err := sharedViewTmpl.Execute(&renderBuf, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	renderBuf.WriteTo(w)
}

// serveSharedAsset serves a file co-located with a shared HTML file.
// Security: validates path containment, extension allowlist, and symlink resolution.
func serveSharedAsset(w http.ResponseWriter, r *http.Request, entry *shareEntry, assetPath string) {
	cleaned := filepath.Clean(assetPath)

	// Reject any traversal attempt
	if strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
		http.NotFound(w, r)
		return
	}

	// Resolve against the shared file's directory
	fullPath := filepath.Join(filepath.Dir(entry.FilePath), cleaned)

	// Resolve symlinks, then verify containment against cached base
	resolved, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if entry.ResolvedBaseDir == "" || !strings.HasPrefix(resolved, entry.ResolvedBaseDir+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}

	// Check extension allowlist
	ext := strings.ToLower(filepath.Ext(resolved))
	contentType, allowed := allowedAssetExts[ext]
	if !allowed {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", contentType)
	http.ServeFile(w, r, resolved)
}

// vendoredAssets allowlists the embedded JS libraries servable under
// /s/{token}/_vendor/. The allowlist also blocks path traversal: only these
// exact names resolve, so "../shared-view.html" and friends never read.
var vendoredAssets = map[string]bool{
	"html-docx.min.js":     true,
	"html-to-image.min.js": true,
}

// serveVendoredAsset serves a binary-embedded third-party library by allowlisted
// name. Content is frozen at build time, so it is safe to cache immutably.
func serveVendoredAsset(w http.ResponseWriter, r *http.Request, name string) {
	if !vendoredAssets[name] {
		http.NotFound(w, r)
		return
	}
	data, err := themeFS.ReadFile("theme/vendor/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(data)
}
