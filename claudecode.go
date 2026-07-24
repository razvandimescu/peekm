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
	replyTimeout = 5 * time.Minute
	// Read-only toolset (v1): replies analyse and answer, they never mutate the
	// filesystem. `claude -p` output isn't surfaced turn-by-turn, so silent Edits
	// would be invisible and un-undoable — steering stays read-only until the
	// transcript view can show and gate writes.
	replyAllowed   = "Read Grep Glob"
	maxReplyLength = 8000
	// maxOwnedBranches bounds the set of peekm-owned branch ids. A personal tool
	// never approaches this; the cap just keeps a long-lived process from growing
	// without limit. Overflow clears the set — a stale branch merely re-forks.
	maxOwnedBranches = 512
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{7,}$`)

// sessionSteerer serialises replies and tracks which session ids are peekm-owned
// branches (safe to resume in place versus a session that may be live in a terminal).
type sessionSteerer struct {
	mu    sync.Mutex // serialises replies and guards owned
	owned map[string]bool
}

var steerer = &sessionSteerer{owned: map[string]bool{}}

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

// reply continues a session with fork-once-then-chain semantics: a session that
// isn't already a peekm-owned branch is forked (leaving a session that may be live
// in a terminal untouched); a branch we own is resumed in place. The caller
// re-renders the returned branch, so subsequent replies target that branch and
// chain onto the same transcript. Serialised globally. Returns the branch id and
// the assistant's final text.
func (s *sessionSteerer) reply(cwd, session, text string) (branch, result string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fork := !s.owned[session]
	newID, result, err := s.runReply(cwd, session, text, fork)
	if err != nil {
		return "", "", err
	}
	if newID == "" {
		newID = session // resume-in-place keeps the id; guard a missing session_id
	}
	if fork {
		if len(s.owned) >= maxOwnedBranches {
			s.owned = map[string]bool{}
		}
		s.owned[newID] = true
		log.Printf("reply: forked session %s -> branch %s", truncateSessionID(session), truncateSessionID(newID))
	}
	return newID, result, nil
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
