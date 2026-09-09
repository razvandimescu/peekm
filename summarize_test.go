package main

import "testing"

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
