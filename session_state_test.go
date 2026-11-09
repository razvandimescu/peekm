package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func postHook(t *testing.T, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	handleClaudeHook(rec, httptest.NewRequest(http.MethodPost, "/hook/file-modified", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s: status %d", body, rec.Code)
	}
}

// nextActivity decodes the broadcast a hook POST sent (broadcasts are synchronous).
func nextActivity(t *testing.T, ch chan string) sessionActivity {
	t.Helper()
	var msg sessionActivity
	select {
	case raw := <-ch:
		_, data, _ := strings.Cut(raw, "data: ")
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			t.Fatalf("SSE payload %q: %v", raw, err)
		}
	default:
		t.Fatal("no SSE broadcast")
	}
	return msg
}

func TestSessionLifecycleHooks(t *testing.T) {
	ch := sseClient(t)
	const sid = "lifecycle-test"
	steps := []struct {
		name, body string
		wantState  string
		wantKept   bool
		wantDetail string
	}{
		{"tool call", `{"sid":"` + sid + `","tool":"Bash","detail":"go test"}`, "", true, ""},
		{"permission prompt", `{"sid":"` + sid + `","event":"Notification","cwd":"/p/peekm","detail":"Claude needs your permission to use Bash"}`,
			sessionWaiting, true, "Claude needs your permission to use Bash"},
		{"approved tool runs", `{"sid":"` + sid + `","tool":"Bash"}`, "", true, ""},
		{"stop with background work", `{"sid":"` + sid + `","event":"Stop","bg":2}`, "", true, ""},
		{"stop", `{"sid":"` + sid + `","event":"Stop","detail":"All tests pass."}`, sessionDone, true, "All tests pass."},
		{"next prompt", `{"sid":"` + sid + `","event":"UserPromptSubmit"}`, "", true, ""},
		{"session end", `{"sid":"` + sid + `","event":"SessionEnd"}`, "", false, ""},
	}
	for _, step := range steps {
		postHook(t, step.body)
		hb, kept := globalHeartbeats.get(sid)
		if kept != step.wantKept || hb.State != step.wantState {
			t.Errorf("%s: state=%q kept=%v, want %q kept=%v", step.name, hb.State, kept, step.wantState, step.wantKept)
		}
		msg := nextActivity(t, ch)
		if msg.Type != "session_activity" || msg.Session != sid || msg.State != step.wantState {
			t.Errorf("%s: broadcast %+v, want session_activity with state %q", step.name, msg, step.wantState)
		}
		if step.wantState != "" && msg.Detail != step.wantDetail {
			t.Errorf("%s: detail %q, want %q", step.name, msg.Detail, step.wantDetail)
		}
		if step.wantState == sessionWaiting && msg.Project != "peekm" {
			t.Errorf("%s: project %q, want peekm", step.name, msg.Project)
		}
	}
}

// A lifecycle state outlives the 5-minute activity window and replaces the
// Active badge, even when the transcript was written after the Stop.
func TestTimelineShowsSessionState(t *testing.T) {
	const sid = "badge-test"
	globalHeartbeats.update(sid, heartbeat{State: sessionWaiting, Detail: "Claude needs your permission to use Bash"})
	t.Cleanup(func() { globalHeartbeats.remove(sid) })

	s := timelineSession{FullSessionID: sid, SessionType: "conversation", newestTime: time.Now()}
	markSessionState(&s, time.Now().Add(time.Hour))
	if s.State != sessionWaiting || s.IsActive {
		t.Fatalf("state=%q active=%v, want waiting and not active", s.State, s.IsActive)
	}

	var buf bytes.Buffer
	data := timelineTemplateData{
		baseTemplateData: baseTemplateData{Partial: true},
		Groups:           []timelineDayGroup{{Label: "Today", Sessions: []timelineSession{s}}},
	}
	if err := timelinePartialTmpl.Execute(&buf, data); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{"timeline-session-waiting", ">Needs you</span>", "Claude needs your permission to use Bash"} {
		if !strings.Contains(html, want) {
			t.Errorf("timeline HTML lacks %q", want)
		}
	}
	if strings.Contains(html, "timeline-active-badge") {
		t.Error("waiting session also shows the Active badge")
	}
}
