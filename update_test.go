package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// brewLink lays out peekm <ver> in a Homebrew prefix the way `brew install`
// does: the keg in Cellar/peekm/<ver>, with bin/peekm and opt/peekm symlinked
// into it.
func brewLink(t *testing.T, prefix, ver string) {
	t.Helper()
	keg := filepath.Join(prefix, "Cellar", "peekm", ver)
	if err := os.MkdirAll(filepath.Join(keg, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keg, "bin", "peekm"), []byte(ver), 0o755); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		filepath.Join(prefix, "bin", "peekm"): filepath.Join("..", "Cellar", "peekm", ver, "bin", "peekm"),
		filepath.Join(prefix, "opt", "peekm"): filepath.Join("..", "Cellar", "peekm", ver),
	}
	for link, target := range links {
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServiceBinaryPathSurvivesBrewUpgrade(t *testing.T) {
	prefix := t.TempDir()
	brewLink(t, prefix, "0.1.34")

	svc, err := serviceBinaryPath(filepath.Join(prefix, "bin", "peekm"))
	if err != nil {
		t.Fatal(err)
	}

	// `brew upgrade` links the new keg, then `brew cleanup peekm` deletes the old one.
	brewLink(t, prefix, "0.1.35")
	if err := os.RemoveAll(filepath.Join(prefix, "Cellar", "peekm", "0.1.34")); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(svc)
	if err != nil {
		t.Fatalf("autostart points at %s, which brew upgrade deleted: %v", svc, err)
	}
	if string(got) != "0.1.35" {
		t.Errorf("autostart runs %s, want 0.1.35", got)
	}
}

func TestUpgradeCommands(t *testing.T) {
	brew := [][]string{{"brew", "update"}, {"brew", "upgrade", "razvandimescu/tap/peekm"}}
	npm := [][]string{{"npm", "install", "-g", "@peekm/peekm@latest"}}
	goInstall := [][]string{{"go", "install", "github.com/razvandimescu/peekm@latest"}}
	cases := []struct {
		exe         string
		goInstalled bool
		want        [][]string
	}{
		{"/opt/homebrew/Cellar/peekm/0.1.34/bin/peekm", false, brew},
		{"/home/linuxbrew/.linuxbrew/Cellar/peekm/0.1.34/bin/peekm", false, brew},
		{"/usr/local/lib/node_modules/@peekm/peekm/node_modules/@peekm/linux-x64/bin/peekm", false, npm},
		{"/Users/u/.npm/_npx/1a2b/node_modules/@peekm/peekm/node_modules/@peekm/darwin-arm64/bin/peekm", false, nil},
		{"/Users/u/go/bin/peekm", true, goInstall},
		{"/Users/u/go/bin/peekm", false, nil},
		{"/usr/local/bin/peekm", false, nil},
	}
	for _, c := range cases {
		got := upgradeCommands(filepath.FromSlash(c.exe), c.goInstalled)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("upgradeCommands(%q, %v) = %v, want %v", c.exe, c.goInstalled, got, c.want)
		}
	}
}
