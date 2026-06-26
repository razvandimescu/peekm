package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestHandleTranscriptReplyValidation(t *testing.T) {
	cases := []struct {
		name   string
		method string
		tunnel bool
		body   string
		want   int
	}{
		{"wrong method", http.MethodGet, false, "", http.StatusMethodNotAllowed},
		{"tunnel blocked", http.MethodPost, true, `{"session":"abcd1234","text":"hi"}`, http.StatusForbidden},
		{"bad json", http.MethodPost, false, `{`, http.StatusBadRequest},
		{"missing fields", http.MethodPost, false, `{"session":"","text":""}`, http.StatusBadRequest},
		{"bad session id", http.MethodPost, false, `{"session":"--evil","text":"hi"}`, http.StatusBadRequest},
		{"unknown session", http.MethodPost, false, `{"session":"00000000-0000-0000-0000-000000000000","text":"hi"}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/transcript/reply", strings.NewReader(tc.body))
			if tc.tunnel {
				req.Header.Set("X-Tunnel", "true")
			}
			rec := httptest.NewRecorder()
			handleTranscriptReply(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d (body=%q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestDeriveSessionCwdPrefersEncodedMatch(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	encoded := strings.ReplaceAll(home, "/", "-")
	dir := filepath.Join(t.TempDir(), encoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonl := filepath.Join(dir, "sess.jsonl")
	// A worktree cwd appears last, but the canonical (home) cwd encodes to the parent dir.
	content := `{"type":"user","cwd":"` + home + `"}
{"type":"assistant","cwd":"` + filepath.Join(home, "sub", "worktree") + `"}
`
	if err := os.WriteFile(jsonl, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := deriveSessionCwd(jsonl)
	if err != nil {
		t.Fatal(err)
	}
	if got != home {
		t.Fatalf("got cwd %q, want %q (should prefer the encoded-match over last-seen)", got, home)
	}
}

func TestSteererForkOnceThenChain(t *testing.T) {
	s := &sessionSteerer{branches: map[string]string{}, locks: map[string]*sync.Mutex{}}
	const orig = "orig-session-id"
	if s.branchOf(orig) != "" {
		t.Fatal("expected no branch initially (first reply must fork)")
	}
	s.setBranch(orig, "branch-id")
	if got := s.branchOf(orig); got != "branch-id" {
		t.Fatalf("got branch %q, want branch-id (subsequent replies must chain)", got)
	}
	l1, l2 := s.lockFor(orig), s.lockFor(orig)
	if l1 != l2 {
		t.Fatal("lockFor must return the same mutex per session")
	}
}
