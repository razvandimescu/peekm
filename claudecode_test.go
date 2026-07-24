package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandleTranscriptReplyValidation(t *testing.T) {
	cases := []struct {
		name   string
		method string
		body   string
		want   int
	}{
		{"wrong method", http.MethodGet, "", http.StatusMethodNotAllowed},
		{"bad json", http.MethodPost, `{`, http.StatusBadRequest},
		{"missing fields", http.MethodPost, `{"session":"","text":""}`, http.StatusBadRequest},
		{"bad session id", http.MethodPost, `{"session":"--evil","text":"hi"}`, http.StatusBadRequest},
		{"unknown session", http.MethodPost, `{"session":"00000000-0000-0000-0000-000000000000","text":"hi"}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/transcript/reply", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			handleTranscriptReply(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d (body=%q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestBlockTunnel(t *testing.T) {
	var called bool
	h := blockTunnel(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	// A tunnel request is rejected before reaching the handler.
	req := httptest.NewRequest(http.MethodPost, "/transcript/reply", nil)
	req.Header.Set("X-Tunnel", "true")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusForbidden || called {
		t.Fatalf("tunnel request: got %d called=%v, want 403 with handler not called", rec.Code, called)
	}

	// A normal request passes through.
	called = false
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/transcript/reply", nil))
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("normal request: got %d called=%v, want 200 with handler called", rec.Code, called)
	}
}

func TestDeriveSessionCwdPrefersEncodedMatch(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	encoded := encodeProjectDir(home)
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

// TestResolveEncodedPathFoldsUnderscore guards the property deriveSessionCwd relies on:
// Claude Code folds '_' (and '.') into '-' when encoding project dirs, so recovering the
// real cwd requires trying both separators and preferring the longest on-disk match.
// A plain "/"->"-" comparison (the earlier bug) never matched an underscore-named project.
func TestResolveEncodedPathFoldsUnderscore(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"rinkt_neville", "rinkt_bot", "rinkt_bot_api"} {
		if err := os.MkdirAll(filepath.Join(root, "projects", p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		encoded string
		want    string
	}{
		{"projects-rinkt-neville", filepath.Join(root, "projects", "rinkt_neville")},
		{"projects-rinkt-bot", filepath.Join(root, "projects", "rinkt_bot")},
		{"projects-rinkt-bot-api", filepath.Join(root, "projects", "rinkt_bot_api")}, // longest match
	}
	for _, tc := range cases {
		got, ok := resolveEncodedPath(strings.Split(tc.encoded, "-"), 0, root)
		if !ok || got != tc.want {
			t.Errorf("resolveEncodedPath(%q) = %q ok=%v, want %q", tc.encoded, got, ok, tc.want)
		}
	}
}

func TestSteererOwnership(t *testing.T) {
	s := &sessionSteerer{owned: map[string]bool{}}
	const orig = "orig-session-id"
	if s.owned[orig] {
		t.Fatal("a fresh session must not be owned (first reply must fork)")
	}
	s.owned["branch-id"] = true
	if !s.owned["branch-id"] {
		t.Fatal("a branch, once owned, must resume in place (no re-fork)")
	}

	// Overflow clears the set rather than growing without bound.
	for i := 0; i < maxOwnedBranches; i++ {
		if len(s.owned) >= maxOwnedBranches {
			s.owned = map[string]bool{}
		}
		s.owned[string(rune(i))] = true
	}
	if len(s.owned) > maxOwnedBranches {
		t.Fatalf("owned grew to %d, want <= %d", len(s.owned), maxOwnedBranches)
	}
}
