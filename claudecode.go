package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Session steering (v1): a reply box on the transcript page continues a Claude Code
// session via `claude -p`. Divergence with a session that is live in a terminal is
// removed by construction with --fork-session: the first reply branches into a new
// session id (the original transcript is never written), and subsequent replies resume
// that peekm-owned branch in place. A per-session lock serialises peekm against itself.

const (
	replyTimeout   = 5 * time.Minute
	replyAllowed   = "Read Grep Glob Edit" // narrow toolset so nothing blocks on approval (v1)
	maxReplyLength = 8000
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{7,}$`)

// sessionSteerer tracks the peekm-owned branch for each original session and
// serialises replies per session.
type sessionSteerer struct {
	mu       sync.Mutex
	branches map[string]string      // original session id -> peekm-owned branch id
	locks    map[string]*sync.Mutex // per original session id
}

var steerer = &sessionSteerer{
	branches: map[string]string{},
	locks:    map[string]*sync.Mutex{},
}

func (s *sessionSteerer) lockFor(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.locks[id]; ok {
		return l
	}
	l := &sync.Mutex{}
	s.locks[id] = l
	return l
}

func (s *sessionSteerer) branchOf(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.branches[id]
}

func (s *sessionSteerer) setBranch(id, branch string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.branches[id] = branch
}

// deriveSessionCwd finds the project directory a session belongs to. The JSONL's parent
// directory name is Claude Code's encoding of the project dir, where /, _ and . all collapse
// to -; resolveProjectDir decodes it back to the real path (the same resolver the memory
// browser relies on to disambiguate e.g. rinkt_bot vs rinkt_bot_api). Falls back to the last
// cwd recorded in the transcript when the encoded dir no longer resolves on disk. The result
// is validated to be within $HOME.
func deriveSessionCwd(jsonlPath string) (string, error) {
	encodedParent := filepath.Base(filepath.Dir(jsonlPath))
	if canonical := resolveProjectDir(encodedParent); canonical != "" {
		if validated, err := validateAndResolvePath(canonical); err == nil {
			return validated, nil
		}
	}

	f, err := os.Open(jsonlPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var lastCwd string
	dec := json.NewDecoder(f)
	for {
		var entry struct {
			Cwd string `json:"cwd"`
		}
		if dec.Decode(&entry) != nil {
			break
		}
		if entry.Cwd != "" {
			lastCwd = entry.Cwd
		}
	}
	if lastCwd == "" {
		return "", fmt.Errorf("no cwd in transcript")
	}

	validated, err := validateAndResolvePath(lastCwd)
	if err != nil {
		return "", fmt.Errorf("cwd outside boundary: %w", err)
	}
	return validated, nil
}

// runReply invokes `claude -p` to continue resumeID, returning the (possibly new) session
// id and the assistant's final text. The prompt is sent as plain-text stdin.
func (s *sessionSteerer) runReply(cwd, resumeID, prompt string, fork bool) (newID, result string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), replyTimeout)
	defer cancel()

	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--allowedTools", replyAllowed,
		"--resume", resumeID,
	}
	if fork {
		args = append(args, "--fork-session")
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(prompt)

	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if stderr := strings.TrimSpace(string(ee.Stderr)); stderr != "" {
				return "", "", fmt.Errorf("%w: %s", err, stderr)
			}
		}
		return "", "", err
	}

	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Result    string `json:"result"`
		}
		if dec.Decode(&ev) != nil {
			break
		}
		if ev.SessionID != "" {
			newID = ev.SessionID
		}
		if ev.Type == "result" && ev.Result != "" {
			result = ev.Result
		}
	}
	return newID, result, nil
}

// reply continues a session with fork-once-then-chain semantics: the first reply forks (the
// original transcript is never written), later replies resume the peekm-owned branch in place.
// Serialised per session. Returns the branch id and the assistant's final text.
func (s *sessionSteerer) reply(cwd, session, text string) (branch, result string, err error) {
	lock := s.lockFor(session)
	lock.Lock()
	defer lock.Unlock()

	branch = s.branchOf(session)
	fork := branch == ""
	resumeID := session
	if branch != "" {
		resumeID = branch
	}

	newID, result, err := s.runReply(cwd, resumeID, text, fork)
	if err != nil {
		return "", "", err
	}
	if fork && newID != "" {
		s.setBranch(session, newID)
		branch = newID
		log.Printf("reply: forked session %s -> branch %s", truncateSessionID(session), truncateSessionID(newID))
	}
	return branch, result, nil
}

// handleTranscriptReply continues a session from the transcript page (fork-once-then-chain).
func handleTranscriptReply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Cap the body before reading it; maxReplyLength alone is only enforced post-decode.
	// Slack covers the JSON envelope plus worst-case escaping of a max-length reply.
	r.Body = http.MaxBytesReader(w, r.Body, 2*maxReplyLength+1024)

	var req struct {
		Session string `json:"session"`
		Text    string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	session := strings.TrimSpace(req.Session)
	text := strings.TrimSpace(req.Text)
	if session == "" || text == "" {
		http.Error(w, "session and text are required", http.StatusBadRequest)
		return
	}
	if len(text) > maxReplyLength {
		http.Error(w, "reply too long", http.StatusBadRequest)
		return
	}
	if !sessionIDPattern.MatchString(session) {
		http.Error(w, "invalid session id", http.StatusBadRequest)
		return
	}

	path := resolveTranscriptPath(session)
	if path == "" {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	cwd, err := deriveSessionCwd(path)
	if err != nil {
		http.Error(w, "Access denied", http.StatusForbidden)
		return
	}

	branch, result, err := steerer.reply(cwd, session, text)
	if err != nil {
		log.Printf("reply: claude failed for session %s: %v", truncateSessionID(session), err)
		http.Error(w, "Claude failed to reply", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"branch": branch,
		"reply":  result,
	})
}
