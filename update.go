package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
)

const releasesURL = "https://github.com/razvandimescu/peekm/releases/latest"

// goModuleVersion returns the version of a `go install pkg@version` build, or
// "". Such a build has no -X main.version, but its main module has a
// checksum, which a build from a local checkout does not.
func goModuleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Sum == "" {
		return ""
	}
	return strings.TrimPrefix(info.Main.Version, "v")
}

// upgradeCommands returns the commands that upgrade the peekm binary at the
// resolved path exe, or nil when no package manager installed it.
func upgradeCommands(exe string, goInstalled bool) [][]string {
	p := filepath.ToSlash(exe)
	switch {
	case strings.Contains(p, brewKeg):
		// Before an upgrade, brew refreshes its taps only once a day.
		return [][]string{{"brew", "update"}, {"brew", "upgrade", "razvandimescu/tap/peekm"}}
	case strings.Contains(p, "/node_modules/@peekm/") && !strings.Contains(p, "/_npx/"):
		return [][]string{{"npm", "install", "-g", "@peekm/peekm@latest"}}
	case goInstalled:
		return [][]string{{"go", "install", "github.com/razvandimescu/peekm@latest"}}
	}
	return nil
}

func runUpdate() {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot determine peekm binary path: %v\n", err)
		os.Exit(1)
	}
	cmds := upgradeCommands(exe, goModuleVersion() != "")
	if cmds == nil {
		fmt.Fprintf(os.Stderr, "peekm cannot update %s: Homebrew, npm, or go install did not install it.\n", exe)
		fmt.Fprintf(os.Stderr, "Download the latest release from %s\n", releasesURL)
		os.Exit(1)
	}
	if fetchLatestVersion(versionCachePath()) == version {
		fmt.Printf("peekm %s is the latest version.\n", version)
		return
	}
	// Resolve before upgrading: brew deletes the keg that exe points into.
	svc, err := serviceBinaryPath(exe)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot resolve binary path: %v\n", err)
		os.Exit(1)
	}
	for _, c := range cmds {
		fmt.Printf("$ %s\n", strings.Join(c, " "))
		if err := runCommand(c[0], c[1:]...); err != nil {
			fmt.Fprintf(os.Stderr, "Update failed: %v\n", err)
			os.Exit(1)
		}
	}
	if !restartAutostart(svc, mustHomeDir()) {
		fmt.Println("Restart peekm to use the new version.")
	}
}
