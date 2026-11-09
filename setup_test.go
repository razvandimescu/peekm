package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func readHooks(t *testing.T, settingsPath string) map[string]interface{} {
	t.Helper()
	settings, hooks, err := loadHooks(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if settings["model"] != "opus" {
		t.Errorf("unrelated setting lost: %v", settings["model"])
	}
	return hooks
}

func TestInstallAndRemoveClaudeHooks(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	scriptPath := filepath.Join(dir, "peekm-hook.sh")
	userStop := `{"hooks":[{"type":"command","command":"say done"}]}`
	os.WriteFile(settingsPath, []byte(`{"model":"opus","hooks":{"Stop":[`+userStop+`]}}`), 0644)

	added, err := installClaudeHooks(dir, 6419)
	if err != nil || added != len(peekmHooks) {
		t.Fatalf("first install: added=%d err=%v, want %d", added, err, len(peekmHooks))
	}
	if added, _ := installClaudeHooks(dir, 6419); added != 0 {
		t.Errorf("second install added %d, want 0", added)
	}

	hooks := readHooks(t, settingsPath)
	stop := hooks["Stop"].([]interface{})
	if len(stop) != 2 {
		t.Fatalf("Stop entries = %d, want user + peekm", len(stop))
	}
	if _, has := stop[1].(map[string]interface{})["matcher"]; has {
		t.Error("Stop entry has a matcher; Stop does not support one")
	}
	notification := hooks["Notification"].([]interface{})[0].(map[string]interface{})
	if notification["matcher"] != "permission_prompt|elicitation_dialog|elicitation_url_dialog" {
		t.Errorf("Notification matcher = %v", notification["matcher"])
	}

	removeClaudeCodeSetup(settingsPath, scriptPath)

	hooks = readHooks(t, settingsPath)
	if len(hooks) != 1 || len(hooks["Stop"].([]interface{})) != 1 {
		t.Errorf("after remove, hooks = %v; want only the user's Stop hook", hooks)
	}
	if fileExists(scriptPath) {
		t.Error("hook script not removed")
	}
}

// The generated script must forward lifecycle hooks as compact state events and
// print nothing: UserPromptSubmit stdout would become Claude's context.
func TestHookScriptForwardsLifecycleEvents(t *testing.T) {
	for _, bin := range []string{"bash", "jq", "curl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	bodies := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- string(b)
	}))
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	dir := t.TempDir()
	if _, err := installClaudeHooks(dir, port); err != nil {
		t.Fatal(err)
	}
	longReply := strings.Repeat("x", 400)

	tests := []struct {
		name, input string
		want        map[string]interface{}
	}{
		{"notification",
			`{"session_id":"s1","cwd":"/p/peekm","hook_event_name":"Notification","message":"Claude needs your permission to use Bash","notification_type":"permission_prompt"}`,
			map[string]interface{}{"sid": "s1", "event": "Notification", "cwd": "/p/peekm", "detail": "Claude needs your permission to use Bash"}},
		{"stop truncates reply",
			`{"session_id":"s1","cwd":"/p/peekm","hook_event_name":"Stop","last_assistant_message":"` + longReply + `","background_tasks":[]}`,
			map[string]interface{}{"sid": "s1", "event": "Stop", "cwd": "/p/peekm", "detail": longReply[:300]}},
		{"stop with background task",
			`{"session_id":"s1","hook_event_name":"Stop","background_tasks":[{"id":"t1"}]}`,
			map[string]interface{}{"sid": "s1", "event": "Stop", "bg": float64(1)}},
		{"prompt submit",
			`{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt":"hello"}`,
			map[string]interface{}{"sid": "s1", "event": "UserPromptSubmit"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command("bash", filepath.Join(dir, "peekm-hook.sh"))
			cmd.Stdin = strings.NewReader(tt.input)
			out, err := cmd.Output()
			if err != nil || len(out) != 0 {
				t.Fatalf("script: err=%v stdout=%q", err, out)
			}
			var got map[string]interface{}
			if err := json.Unmarshal([]byte(<-bodies), &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Errorf("payload = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("%s = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}
