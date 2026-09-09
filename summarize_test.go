package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLLMEndpoint(t *testing.T) {
	tests := []struct {
		name        string
		url         string
		allowRemote string
		want        string
		wantErr     bool
	}{
		{name: "default is loopback", want: defaultLLMEndpoint},
		{name: "loopback IP", url: "http://127.0.0.1:8080/v1/chat/completions", want: "http://127.0.0.1:8080/v1/chat/completions"},
		{name: "IPv6 loopback", url: "http://[::1]:1234/v1/chat/completions", want: "http://[::1]:1234/v1/chat/completions"},
		{name: "remote host refused", url: "https://api.example.com/v1/chat/completions", wantErr: true},
		{name: "remote host allowed by opt-in", url: "https://api.example.com/v1/chat/completions", allowRemote: "1", want: "https://api.example.com/v1/chat/completions"},
		{name: "missing scheme", url: "localhost:11434/v1/chat/completions", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PEEKM_LLM_URL", tt.url)
			t.Setenv("PEEKM_LLM_ALLOW_REMOTE", tt.allowRemote)

			got, err := llmEndpoint()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnvOrDefault(t *testing.T) {
	t.Setenv("PEEKM_LLM_MODEL", "  ")
	if got := envOrDefault("PEEKM_LLM_MODEL", defaultLLMModel); got != defaultLLMModel {
		t.Errorf("blank value should fall back, got %q", got)
	}
	t.Setenv("PEEKM_LLM_MODEL", "custom-model")
	if got := envOrDefault("PEEKM_LLM_MODEL", defaultLLMModel); got != "custom-model" {
		t.Errorf("got %q, want custom-model", got)
	}
}

// TestRunLLMSummarizeRequestShape pins the OpenAI-compatible wire format so a
// runtime swap cannot silently regress it.
func TestRunLLMSummarizeRequestShape(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"  - [note] hello  "}}]}`)
	}))
	defer srv.Close()

	t.Setenv("PEEKM_LLM_URL", srv.URL+"/v1/chat/completions")
	t.Setenv("PEEKM_LLM_MODEL", "test-model")

	out, err := runLLMSummarize(context.Background(), "summarize this")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "- [note] hello" {
		t.Errorf("content not trimmed: %q", out)
	}
	if got["model"] != "test-model" {
		t.Errorf("model = %v, want test-model", got["model"])
	}
	if got["stream"] != false {
		t.Errorf("stream = %v, want false", got["stream"])
	}
	if got["reasoning_effort"] != "none" {
		t.Errorf("reasoning_effort = %v, want none", got["reasoning_effort"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want 1", got["messages"])
	}
	m, _ := msgs[0].(map[string]any)
	if m["role"] != "user" || m["content"] != "summarize this" {
		t.Errorf("message = %v", m)
	}
}

func TestRunLLMSummarizeErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
	}{
		{name: "no choices", body: `{"choices":[]}`, code: 200},
		{name: "empty content", body: `{"choices":[{"message":{"content":"   "}}]}`, code: 200},
		{name: "upstream error", body: `{"error":"model not found"}`, code: 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.code)
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			t.Setenv("PEEKM_LLM_URL", srv.URL)
			if _, err := runLLMSummarize(context.Background(), "x"); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

// TestLiveSummarize runs the whole pipeline against a real local runtime.
// Skipped unless PEEKM_LIVE_LLM=1; PEEKM_LIVE_TRANSCRIPT names a session JSONL.
func TestLiveSummarize(t *testing.T) {
	if os.Getenv("PEEKM_LIVE_LLM") != "1" {
		t.Skip("set PEEKM_LIVE_LLM=1 to run against a local runtime")
	}
	path := os.Getenv("PEEKM_LIVE_TRANSCRIPT")
	if path == "" {
		t.Skip("set PEEKM_LIVE_TRANSCRIPT to a session transcript JSONL")
	}

	ss := &summaryStore{
		summaries:  make(map[string]*sessionSummary),
		daily:      make(map[string]*dailySummary),
		filePath:   filepath.Join(t.TempDir(), "summaries.json"),
		heartbeats: newHeartbeatStore(),
		prevActive: make(map[string]bool),
	}

	start := time.Now()
	if err := ss.generateSummary("live-test", "peekm", path); err != nil {
		t.Fatalf("generateSummary: %v", err)
	}
	got, ok := ss.get("live-test")
	if !ok {
		t.Fatal("no summary stored")
	}
	t.Logf("elapsed=%s outcome=%q domain=%q files=%d\n%s",
		time.Since(start).Round(time.Second), got.Outcome, got.Domain, len(got.FilesModified), got.Summary)
	if strings.Contains(got.Summary, "</think>") || strings.Contains(got.Summary, "done thinking") {
		t.Error("thinking markers leaked into summary")
	}
}
