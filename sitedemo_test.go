package main

import (
	"bytes"
	"flag"
	"html"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var siteDemoOut = flag.String("site-demo-out", "", "write the peekm.dev demo page to this path (make site-demo)")

const (
	demoSessionID = "a1b2c3d4-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	demoBranchID  = "e5f6a7b8-9c0d-4e1f-8a2b-3c4d5e6f7a8b"
	demoHome      = "/Users/you"
)

var mermaidScript = regexp.MustCompile(`<script src="[^"]*mermaid[^"]*"></script>\n?`)

// TestSiteDemo renders the peekm.dev demo through serveTranscript, so the site
// shows the app's own markup, CSS and JS instead of a hand-kept copy.
func TestSiteDemo(t *testing.T) {
	page := renderSiteDemo(t)
	for _, want := range []string{`id="demo-branch"`, `class="transcript-conversation"`, `id="reply-input"`} {
		if !strings.Contains(page, want) {
			t.Errorf("demo page lacks %s", want)
		}
	}
	if strings.Contains(page, "/assets/fonts/") || strings.Contains(page, os.Getenv("HOME")) {
		t.Error("demo page still points at local server paths")
	}
	if *siteDemoOut != "" {
		mustWrite(t, *siteDemoOut, page)
	}
}

// TestSiteRawLogIsDemoSession keeps the landing page's raw JSONL panel in step
// with the session the demo renders.
func TestSiteRawLogIsDemoSession(t *testing.T) {
	index, err := os.ReadFile("site/index.html")
	if err != nil {
		t.Fatal(err)
	}
	session := string(readDemoFixture(t, "session.jsonl"))
	lines := regexp.MustCompile(`(?s)<p>(\{.*?\})</p>`).FindAllSubmatch(index, -1)
	if len(lines) == 0 {
		t.Fatal("no raw JSONL lines found in site/index.html")
	}
	for _, m := range lines {
		if !strings.Contains(session, html.UnescapeString(string(m[1]))+"\n") {
			t.Errorf("site/index.html shows a JSONL line that testdata/sitedemo/session.jsonl lacks: %.80s", m[1])
		}
	}
}

func renderSiteDemo(t *testing.T) string {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	writeDemoTranscripts(t, home)
	useDemoWorkspace(t, home)

	page := string(getDemoTranscript(t, demoSessionID, false))
	branch := string(getDemoTranscript(t, demoBranchID, true))

	page = strings.Replace(page, "<head>", "<head>\n<script>\n"+string(readDemoFixture(t, "demo.js"))+"</script>", 1)
	page = strings.Replace(page, "</body>", `<template id="demo-branch" data-session="`+demoBranchID+`">`+branch+"</template>\n</body>", 1)
	page = mermaidScript.ReplaceAllString(page, "")
	page = strings.ReplaceAll(page, "/assets/fonts/", "fonts/")
	return strings.ReplaceAll(page, home, demoHome)
}

// writeDemoTranscripts stores the session and its reply fork where
// resolveTranscriptPath looks. The fork repeats the history under its own ID,
// as `claude --fork-session` does.
func writeDemoTranscripts(t *testing.T, home string) {
	session := readDemoFixture(t, "session.jsonl")
	fork := bytes.ReplaceAll(session, []byte(demoSessionID), []byte(demoBranchID))
	fork = append(fork, readDemoFixture(t, "reply.jsonl")...)

	store := filepath.Join(home, ".claude", "projects", "-Users-you-acme-api")
	mustMkdir(t, store)
	mustWrite(t, filepath.Join(store, demoSessionID+".jsonl"), string(session))
	mustWrite(t, filepath.Join(store, demoBranchID+".jsonl"), string(fork))
}

// useDemoWorkspace points the sidebar, edit stats and clock at a fixed demo
// home, restoring the globals afterwards.
func useDemoWorkspace(t *testing.T, home string) {
	withCleanWhitelist(t)
	setBrowseDir(t, home)
	for _, rel := range []string{"acme-api/README.md", "acme-api/docs/pagination.md", "billing-service/README.md", "web-dashboard/README.md"} {
		path := filepath.Join(home, rel)
		mustMkdir(t, filepath.Dir(path))
		mustWrite(t, path, "# "+rel+"\n")
		addToWhitelist(path)
	}

	edited := filepath.Join(home, "acme-api", "src", "widgets.ts")
	edit := func(ts string) SessionEvent {
		at, _ := time.Parse(time.RFC3339, ts)
		return SessionEvent{FilePath: edited, ToolName: "Edit", SessionID: demoSessionID, CWD: filepath.Dir(filepath.Dir(edited)), Timestamp: at}
	}
	prevLog, prevLocal := globalEventLog, time.Local
	globalEventLog = &eventLog{events: []SessionEvent{edit("2026-10-28T14:22:11Z"), edit("2026-10-28T14:23:40Z")}}
	time.Local = time.UTC
	t.Cleanup(func() { globalEventLog, time.Local = prevLog, prevLocal })
}

func getDemoTranscript(t *testing.T, session string, partial bool) []byte {
	req := httptest.NewRequest("GET", "/transcript?session="+session, nil)
	if partial {
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
	}
	rec := httptest.NewRecorder()
	serveTranscript(rec, req)
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte("transcript-conversation")) {
		t.Fatalf("transcript %s: status %d, no conversation rendered", session, rec.Code)
	}
	return rec.Body.Bytes()
}

func readDemoFixture(t *testing.T, name string) []byte {
	b, err := os.ReadFile(filepath.Join("testdata", "sitedemo", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
