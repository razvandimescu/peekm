package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// shareEntry represents one active LAN share
type shareEntry struct {
	Token     string
	FilePath  string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// shareStore maintains active LAN shares keyed by token
type shareStore struct {
	mu      sync.RWMutex
	entries map[string]*shareEntry
}

func newShareStore() *shareStore {
	return &shareStore{entries: make(map[string]*shareEntry)}
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
	if existing, ok := s.findByPath(filePath); ok {
		return existing
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := &shareEntry{
		Token:     generateToken(),
		FilePath:  filePath,
		ExpiresAt: time.Now().Add(ttl),
		CreatedAt: time.Now(),
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
		s.mu.Lock()
		delete(s.entries, token)
		s.mu.Unlock()
		return nil, false
	}
	return entry, true
}

func (s *shareStore) revoke(token string) {
	s.mu.Lock()
	delete(s.entries, token)
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
	ExpiresAt string `json:"expires_at,omitempty"`
}

func newShareResponse(entry *shareEntry) shareResponse {
	return shareResponse{
		Active:    true,
		Token:     entry.Token,
		URL:       buildShareURL(entry.Token),
		ExpiresAt: entry.ExpiresAt.Format(time.RFC3339),
	}
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

func handleShareCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	absPath, err := validateAndResolvePath(cleanInputPath(req.Path))
	if err != nil || !isWhitelistedFile(absPath) {
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
	IsExpired bool
}

func serveSharedFile(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/s/")
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
	content, err := os.ReadFile(entry.FilePath)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	md := newMarkdownRenderer()
	var buf bytes.Buffer
	if err := md.Convert(content, &buf); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	// Content-only partial for SSE live reload
	if isPartialRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(buf.Bytes())
		return
	}
	data := sharedViewData{
		baseTemplateData: newBaseTemplateData(),
		Content:          template.HTML(buf.String()),
		Token:            token,
		ExpiresAt:        entry.ExpiresAt.Format(time.RFC3339),
		FileName:         filepath.Base(entry.FilePath),
		FilePath:         entry.FilePath,
	}
	var renderBuf bytes.Buffer
	if err := sharedViewTmpl.Execute(&renderBuf, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	renderBuf.WriteTo(w)
}
