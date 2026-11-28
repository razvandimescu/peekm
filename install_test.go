package main

import (
	"encoding/xml"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestServiceFlags(t *testing.T) {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.Bool("browser", true, "")
	fs.Int("port", 6419, "")
	fs.String("trusted-cidr", "", "")
	fs.String("allow-host", "", "")
	if err := fs.Parse([]string{"-browser=true", "-port", "7000", "-trusted-cidr", "100.64.0.0/10"}); err != nil {
		t.Fatal(err)
	}
	got := serviceFlags(fs)
	want := []string{"-browser=false", "-port=7000", "-trusted-cidr=100.64.0.0/10"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("serviceFlags = %v, want %v", got, want)
	}
}

func TestServiceCommandKeepsInstallArgs(t *testing.T) {
	home := t.TempDir()
	legacy := []string{"/bin/peekm", "-browser=false"}
	if got := serviceCommand("/bin/peekm", home); !reflect.DeepEqual(got, legacy) {
		t.Errorf("service installed before stored args: got %v, want %v", got, legacy)
	}

	args := []string{"-browser=false", "-port=7000", "/home/u/notes"}
	if err := saveServiceArgs(home, args); err != nil {
		t.Fatal(err)
	}
	got := serviceCommand("/opt/peekm/bin/peekm", home)
	want := append([]string{"/opt/peekm/bin/peekm"}, args...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("serviceCommand = %v, want %v", got, want)
	}
}

func TestLaunchdPlistProgramArguments(t *testing.T) {
	cmd := []string{"/Users/a&b/bin/peekm", "-browser=false", "-allow-host=<x>", "/Users/a&b/My Notes"}
	plist := launchdPlist(cmd, "/Users/a&b/.peekm/peekm.log", "/Users/a&b")

	var doc struct {
		Dict struct {
			Items []struct {
				XMLName xml.Name
				Text    string   `xml:",chardata"`
				Strings []string `xml:"string"`
			} `xml:",any"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal([]byte(plist), &doc); err != nil {
		t.Fatalf("plist is not valid XML: %v\n%s", err, plist)
	}
	items := doc.Dict.Items
	for i := 0; i+1 < len(items); i++ {
		if items[i].XMLName.Local == "key" && items[i].Text == "ProgramArguments" {
			if got := items[i+1].Strings; !reflect.DeepEqual(got, cmd) {
				t.Errorf("ProgramArguments = %q, want %q", got, cmd)
			}
			return
		}
	}
	t.Fatalf("no ProgramArguments in plist:\n%s", plist)
}

func TestSystemdUnitExecStart(t *testing.T) {
	unit := systemdUnitFile([]string{"/home/u/bin/peekm", "-browser=false", `/home/u/My Notes $5 100% a"b\c`}, "/home/u")
	want := `ExecStart="/home/u/bin/peekm" "-browser=false" "/home/u/My Notes $$5 100%% a\"b\\c"`
	if !strings.Contains(unit, "\n"+want+"\n") {
		t.Errorf("unit lacks %s:\n%s", want, unit)
	}
}

func TestStartupVBSRedirectsThroughCmd(t *testing.T) {
	vbs := startupVBS([]string{`C:\Users\u\peekm.exe`, "-browser=false", `C:\Users\u\My Notes`}, `C:\Users\u\.peekm\peekm.log`)
	want := `WshShell.Run "cmd /s /c """"C:\Users\u\peekm.exe"" ""-browser=false"" ""C:\Users\u\My Notes"" > ""C:\Users\u\.peekm\peekm.log"" 2>&1""", 0, False`
	if !strings.Contains(vbs, "\n"+want+"\n") {
		t.Errorf("script lacks\n%s\ngot\n%s", want, vbs)
	}
}

func TestOpenInRunningNavigatesDirectory(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(homeDir, "peekm_handoff_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# hi"), 0644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.Mkdir(empty, 0755); err != nil {
		t.Fatal(err)
	}
	dir, _ = filepath.EvalSymlinks(dir)

	fileMutex.Lock()
	oldBrowseDir, oldFiles := browseDir, markdownFiles
	fileMutex.Unlock()
	defer func() {
		dirWatcher.close()
		fileMutex.Lock()
		browseDir, markdownFiles = oldBrowseDir, oldFiles
		fileMutex.Unlock()
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/navigate", handleNavigate)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	viewPath, err := openInRunning(srv.Client(), srv.URL, dir, true)
	if err != nil {
		t.Fatalf("openInRunning(dir): %v", err)
	}
	if viewPath != "" {
		t.Errorf("view path = %q, want the root", viewPath)
	}
	fileMutex.RLock()
	got := browseDir
	fileMutex.RUnlock()
	if got != dir {
		t.Errorf("browse root = %s, want %s", got, dir)
	}

	if _, err := openInRunning(srv.Client(), srv.URL, empty, true); err == nil || !strings.Contains(err.Error(), "No markdown files") {
		t.Errorf("directory without markdown: got error %v", err)
	}
}
