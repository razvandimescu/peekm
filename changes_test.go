package main

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeTranscriptChangesTab(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	project := filepath.Join(home, "proj")
	setBrowseDir(t, project)

	const sid = "0199aaaa-bbbb-cccc-dddd-eeeeffff0001"
	edit, _ := json.Marshal(map[string]any{
		"type": "user", "timestamp": "2026-11-06T10:00:01Z",
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}}},
		"toolUseResult": map[string]any{
			"filePath": filepath.Join(project, "main.go"), "oldString": "a()", "newString": "b()",
			"originalFile": "package main\n\nfunc a() {}\n", "structuredPatch": []any{},
		},
	})
	store := filepath.Join(home, ".claude", "projects", "-proj")
	mustMkdir(t, store)
	mustWrite(t, filepath.Join(store, sid+".jsonl"),
		`{"type":"user","timestamp":"2026-11-06T10:00:00Z","message":{"role":"user","content":"rename a"}}`+"\n"+string(edit)+"\n")

	get := func(query string) string {
		rec := httptest.NewRecorder()
		serveTranscript(rec, httptest.NewRequest("GET", "/transcript?session="+sid+query, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", query, rec.Code)
		}
		return rec.Body.String()
	}

	page := get("&view=changes")
	for _, want := range []string{
		`aria-current="page">Changes`,
		`class="changes-path">main.go<`,
		`<span class="diff-remove"><span class="diff-ln"></span>- func a() {}</span>`,
		`<span class="diff-add"><span class="diff-ln">3</span>+ func b() {}</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("Changes tab missing %q", want)
		}
	}
	if strings.Contains(page, `<div class="transcript-conversation">`) {
		t.Error("Changes tab also rendered the conversation")
	}

	page = get("")
	if !strings.Contains(page, `aria-current="page">Conversation`) || !strings.Contains(page, `<div class="transcript-conversation">`) {
		t.Error("default view is not the conversation")
	}
}
