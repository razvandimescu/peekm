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

var siteDemoOut = flag.String("site-demo-out", "", "write the demo page to this path (make hero)")

const (
	demoSessionID = "a1b2c3d4-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	demoHome      = "/Users/you"
)

// TestSiteDemo renders the demo session through serveTranscript. make hero
// screenshots that page for the README and peekm.dev.
func TestSiteDemo(t *testing.T) {
	page := renderSiteDemo(t)
	if strings.Contains(page, os.Getenv("HOME")) {
		t.Error("demo page shows the test HOME")
	}
	if *siteDemoOut != "" {
		mustWrite(t, *siteDemoOut, page)
	}
}

// TestSiteRawLogIsDemoSession keeps the landing page's raw JSONL panel in step
// with the session in the screenshot beside it.
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
	store := filepath.Join(home, ".claude", "projects", "-Users-you-acme-api")
	mustMkdir(t, store)
	mustWrite(t, filepath.Join(store, demoSessionID+".jsonl"), string(readDemoFixture(t, "session.jsonl")))
	useDemoWorkspace(t, home)

	return strings.ReplaceAll(string(getDemoTranscript(t)), home, demoHome)
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

func getDemoTranscript(t *testing.T) []byte {
	rec := httptest.NewRecorder()
	serveTranscript(rec, httptest.NewRequest("GET", "/transcript?session="+demoSessionID, nil))
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte("transcript-conversation")) {
		t.Fatalf("demo transcript: status %d, no conversation rendered", rec.Code)
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
