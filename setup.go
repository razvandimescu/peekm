package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// runSetup handles the "peekm setup" subcommand
func runSetup(args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: peekm setup <target> [options]")
		fmt.Println("\nTargets:")
		fmt.Println("  claude-code  Configure Claude Code hooks [--remove] [--port PORT]")
		fmt.Println("  pi           Configure pi extension [--remove] [--port PORT]")
		os.Exit(1)
	}

	switch args[0] {
	case "claude-code":
		setupClaudeCode(args[1:])
	case "pi":
		setupPi(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown setup target: %s\n", args[0])
		fmt.Println("Available: claude-code, pi")
		os.Exit(1)
	}
}

func setupClaudeCode(args []string) {
	setupFlags := flag.NewFlagSet("setup claude-code", flag.ExitOnError)
	remove := setupFlags.Bool("remove", false, "Remove peekm hooks from Claude Code")
	hookPort := setupFlags.Int("port", 6419, "Port peekm runs on")
	setupFlags.Parse(args)

	homeDir := mustHomeDir()

	claudeDir := filepath.Join(homeDir, ".claude")
	settingsPath := filepath.Join(claudeDir, "settings.json")
	hookScriptPath := filepath.Join(claudeDir, "peekm-hook.sh")

	if *remove {
		removeClaudeCodeSetup(settingsPath, hookScriptPath)
		return
	}

	fmt.Println("\n  AI Session Tracking Setup")
	fmt.Println("  " + strings.Repeat("\u2500", 25))

	added, err := installClaudeHooks(claudeDir, *hookPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "    Error: %v\n", err)
		os.Exit(1)
	}

	if added > 0 {
		fmt.Printf("    Added %d hook(s)\n", added)
	} else {
		fmt.Printf("    Hooks already configured (no changes)\n")
	}

	fmt.Println("\n  Setup complete. Restart Claude Code to activate.")
	fmt.Println("  To verify: modify a file with Claude Code and check peekm")
	fmt.Println("  for the AI session badge.")
	fmt.Println()
}

// installClaudeHooks creates the hook script and merges peekmHooks into settings.json.
// Returns the number of hooks added and any error encountered.
func installClaudeHooks(claudeDir string, hookPort int) (int, error) {
	hookScriptPath := filepath.Join(claudeDir, "peekm-hook.sh")
	settingsPath := filepath.Join(claudeDir, "settings.json")

	hookScript := fmt.Sprintf(`#!/bin/bash
# peekm hook: Persist edit events to JSONL, heartbeat all tool calls, report session state
json=$(cat)
session_id=$(echo "$json" | jq -r '.session_id // empty')
[ -z "$session_id" ] && exit 0

notify() {
    for port in %d 6419 8080 3000; do
        curl -sf -X POST -H 'Content-Type: application/json' \
            -d "$1" --max-time 0.05 \
            "http://localhost:$port/hook/file-modified" >/dev/null 2>&1 && return
    done
    # No peekm running is normal; a non-zero exit shows "hook failed" in Claude Code
    return 0
}

case $(echo "$json" | jq -r '.hook_event_name // empty') in
Notification|Stop|UserPromptSubmit|SessionEnd)
    # Session state. Print nothing: UserPromptSubmit stdout becomes Claude's context.
    notify "$(echo "$json" | jq -c '{sid:.session_id,event:.hook_event_name,cwd:.cwd,
        detail:((.message // .last_assistant_message // "")[0:300]),
        bg:((.background_tasks // [])|length)}|with_entries(select(.value!="" and .value!=0 and .value!=null))')"
    exit 0;;
esac

tool_name=$(echo "$json" | jq -r '.tool_name // empty')
[ -z "$tool_name" ] && exit 0

file_path=$(echo "$json" | jq -r '.tool_input.file_path // .tool_input.notebook_path // empty')
ts=$(date -u +"%%Y-%%m-%%dT%%H:%%M:%%SZ")
is_edit=false
case "$tool_name" in Write|Edit|NotebookEdit) is_edit=true;; esac

if [ -n "$file_path" ] && [ "$is_edit" = true ]; then
    # Edit event: persist to JSONL + notify
    perm_mode=$(echo "$json" | jq -r '.permission_mode // empty')
    tool_use_id=$(echo "$json" | jq -r '.tool_use_id // empty')
    cwd=$(echo "$json" | jq -r '.cwd // empty')
    content=$(echo "$json" | jq -r '.tool_input.content // empty')

    event=$(jq -nc --arg sid "$session_id" --arg path "$file_path" \
        --arg tool "$tool_name" --arg perm "$perm_mode" \
        --arg tuid "$tool_use_id" --arg cwd "$cwd" --arg ts "$ts" \
        '{sid:$sid,path:$path,tool:$tool,perm:$perm,tuid:$tuid,cwd:$cwd,ts:$ts}|with_entries(select(.value!=""))')

    mkdir -p ~/.peekm
    echo "$event" >> ~/.peekm/events.jsonl 2>/dev/null

    if echo "$file_path" | grep -q '\.claude/plans/.*\.md$' && [ -n "$content" ]; then
        notify "$(echo "$json" | jq -c '{session_id, tool_name, file_path: .tool_input.file_path, content: .tool_input.content, ts: "'"$ts"'"}')"
    else
        notify "$event"
    fi
else
    # Heartbeat: non-edit tool call, just notify (no JSONL)
    detail=$(echo "$json" | jq -r '.tool_input | .description // .command // .file_path // .pattern // .prompt // empty' | head -c 300)
    notify "$(jq -nc --arg sid "$session_id" --arg tool "$tool_name" --arg d "$detail" \
        '{sid:$sid,tool:$tool,detail:$d}|with_entries(select(.value!=""))')"
fi
`, hookPort)

	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return 0, fmt.Errorf("creating %s: %w", claudeDir, err)
	}

	if err := os.WriteFile(hookScriptPath, []byte(hookScript), 0755); err != nil {
		return 0, fmt.Errorf("writing hook script: %w", err)
	}

	hookEntry := map[string]interface{}{
		"type":    "command",
		"command": hookScriptPath,
		"timeout": 0.15,
	}

	settings, hooks, err := loadHooks(settingsPath)
	if err != nil {
		return 0, err
	}

	added := 0
	for _, h := range peekmHooks {
		entries, _ := hooks[h.event].([]interface{})
		if hasPeekmHook(entries, h.matcher, hookScriptPath) {
			continue
		}
		entry := map[string]interface{}{"hooks": []interface{}{hookEntry}}
		if h.matcher != "" {
			entry["matcher"] = h.matcher
		}
		hooks[h.event] = append(entries, entry)
		added++
	}

	if added == 0 {
		return 0, nil
	}
	return added, writeSettings(settingsPath, settings)
}

func writeSettings(settingsPath string, settings map[string]interface{}) error {
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("serializing settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0644); err != nil {
		return fmt.Errorf("writing %s: %w", settingsPath, err)
	}
	return nil
}

// autoSetupClaudeHooks keeps the hook script up-to-date and adds any missing matchers.
func autoSetupClaudeHooks() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}

	claudeDir := filepath.Join(homeDir, ".claude")
	if _, err := os.Stat(claudeDir); os.IsNotExist(err) {
		log.Printf("Tip: install Claude Code for AI session tracking (https://claude.ai/code)")
		return
	}

	added, err := installClaudeHooks(claudeDir, *port)
	if err != nil {
		log.Printf("Warning: auto-setup failed: %v", err)
		return
	}

	if added > 0 {
		log.Printf("Connected to Claude Code (%d hooks installed). Restart Claude Code to activate.", added)
	}
}

// peekmHooks lists the Claude Code hooks peekm installs. PostToolUse on edit
// tools persists events to JSONL; on other tools it sends heartbeats (active
// badge + last tool). The rest report session state: Notification = waiting
// for you, Stop = turn done, UserPromptSubmit = working again, SessionEnd = closed.
var peekmHooks = []struct{ event, matcher string }{
	{"PostToolUse", "Write"}, {"PostToolUse", "Edit"}, {"PostToolUse", "NotebookEdit"},
	{"PostToolUse", "Read"}, {"PostToolUse", "Bash"}, {"PostToolUse", "Grep"},
	{"PostToolUse", "Glob"}, {"PostToolUse", "Agent"},
	{"Notification", "permission_prompt|elicitation_dialog|elicitation_url_dialog"},
	{"Stop", ""}, {"UserPromptSubmit", ""}, {"SessionEnd", ""},
}

// loadHooks reads settings.json and returns it with its "hooks" map (created
// if absent). A missing file returns empty structures and a nil error.
func loadHooks(settingsPath string) (settings, hooks map[string]interface{}, err error) {
	settings = make(map[string]interface{})
	data, err := os.ReadFile(settingsPath)
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return nil, nil, fmt.Errorf("parsing %s: %w", settingsPath, err)
		}
	}

	hooks, _ = settings["hooks"].(map[string]interface{})
	if hooks == nil {
		hooks = make(map[string]interface{})
		settings["hooks"] = hooks
	}
	return settings, hooks, nil
}

// hasPeekmHook checks if a hook entry for this matcher already has a peekm hook.
func hasPeekmHook(entries []interface{}, matcher, scriptPath string) bool {
	for _, entry := range entries {
		e, ok := entry.(map[string]interface{})
		if m, _ := e["matcher"].(string); !ok || m != matcher {
			continue
		}
		hooks, ok := e["hooks"].([]interface{})
		if ok && containsPeekmHook(hooks, scriptPath) {
			return true
		}
	}
	return false
}

// filterPeekmHooks returns hook entries that don't reference the peekm hook script.
func filterPeekmHooks(entries []interface{}, hookScriptPath string) (filtered []interface{}, removed int) {
	for _, entry := range entries {
		e, ok := entry.(map[string]interface{})
		if !ok {
			filtered = append(filtered, entry)
			continue
		}
		entryHooks, ok := e["hooks"].([]interface{})
		if !ok {
			filtered = append(filtered, entry)
			continue
		}
		if containsPeekmHook(entryHooks, hookScriptPath) {
			removed++
		} else {
			filtered = append(filtered, entry)
		}
	}
	return
}

func containsPeekmHook(hooks []interface{}, hookScriptPath string) bool {
	for _, h := range hooks {
		hook, ok := h.(map[string]interface{})
		if !ok {
			continue
		}
		if cmd, ok := hook["command"].(string); ok && cmd == hookScriptPath {
			return true
		}
	}
	return false
}

// removeAndReport deletes path and prints the outcome. A missing file is silent.
func removeAndReport(path string) {
	if err := os.Remove(path); err == nil {
		fmt.Printf("    Removed %s\n", path)
	} else if !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "    Warning: %v\n", err)
	}
}

func removeClaudeCodeSetup(settingsPath, hookScriptPath string) {
	fmt.Println("\n  Removing AI Session Tracking")
	fmt.Println("  " + strings.Repeat("\u2500", 30))

	// Remove hook script
	removeAndReport(hookScriptPath)

	settings, hooks, err := loadHooks(settingsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "    Error parsing settings: %v\n", err)
		os.Exit(1)
	}

	removed := 0
	for event, v := range hooks {
		entries, _ := v.([]interface{})
		filtered, n := filterPeekmHooks(entries, hookScriptPath)
		if n == 0 {
			continue
		}
		removed += n
		if len(filtered) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = filtered
		}
	}

	if removed == 0 {
		fmt.Println("    No peekm hooks found in settings")
	} else if err := writeSettings(settingsPath, settings); err != nil {
		fmt.Fprintf(os.Stderr, "    Error: %v\n", err)
		os.Exit(1)
	} else {
		fmt.Printf("    Removed %d hook(s) from settings.json\n", removed)
	}

	fmt.Print("\n  Done.\n\n")
}

func runInstall(args []string) {
	flag.CommandLine.Parse(args)
	parseAccessFlags()
	target, _ := targetArg()
	homeDir := mustHomeDir()
	serviceArgs := append(serviceFlags(flag.CommandLine), target)
	if err := saveServiceArgs(homeDir, serviceArgs); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	installAutostart(append([]string{serviceBinaryPath(mustExecutable())}, serviceArgs...), homeDir)
}

func runUninstall() {
	homeDir := mustHomeDir()
	removeAutostart(homeDir)
	os.Remove(serviceArgsPath(homeDir))
}

// serviceFlags returns the flags set on fs as service arguments. The service
// never opens a browser.
func serviceFlags(fs *flag.FlagSet) []string {
	args := []string{"-browser=false"}
	fs.Visit(func(f *flag.Flag) {
		if f.Name != "browser" {
			args = append(args, "-"+f.Name+"="+f.Value.String())
		}
	})
	return args
}

func serviceArgsPath(homeDir string) string {
	return filepath.Join(homeDir, ".peekm", "service-args.json")
}

func saveServiceArgs(homeDir string, args []string) error {
	path := serviceArgsPath(homeDir)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, _ := json.Marshal(args)
	return os.WriteFile(path, data, 0644)
}

// serviceCommand returns the command line of the installed service for
// binPath. A service installed before peekm stored its arguments ran with
// -browser=false only.
func serviceCommand(binPath, homeDir string) []string {
	var args []string
	data, err := os.ReadFile(serviceArgsPath(homeDir))
	if err != nil || json.Unmarshal(data, &args) != nil {
		args = []string{"-browser=false"}
	}
	return append([]string{binPath}, args...)
}

// mustExecutable returns the resolved path of the running peekm binary, or exits.
func mustExecutable() string {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot determine peekm binary path: %v\n", err)
		os.Exit(1)
	}
	return exe
}

// brewKeg marks a binary inside a Homebrew keg: <prefix>/Cellar/peekm/<version>/.
const brewKeg = "/Cellar/peekm/"

// serviceBinaryPath maps the resolved binary path exe to a path that outlives
// an upgrade. A Homebrew keg path maps to <prefix>/opt/peekm, because
// `brew upgrade` deletes the old keg.
func serviceBinaryPath(exe string) string {
	prefix, kegRel, ok := strings.Cut(exe, brewKeg)
	if !ok {
		return exe
	}
	_, binRel, ok := strings.Cut(kegRel, "/")
	if !ok {
		return exe
	}
	opt := filepath.Join(prefix, "opt", "peekm", binRel)
	if !fileExists(opt) {
		return exe
	}
	return opt
}

func installAutostart(cmd []string, homeDir string) {
	switch detectServiceManager() {
	case "launchd":
		installLaunchd(cmd, homeDir)
	case "systemd":
		installSystemd(cmd, homeDir)
	case "startup-folder":
		installStartupFolder(cmd, homeDir)
	default:
		fmt.Fprintf(os.Stderr, "Error: unsupported platform (need launchd on macOS, systemd on Linux, or Windows)\n")
		os.Exit(1)
	}
}

func removeAutostart(homeDir string) {
	switch detectServiceManager() {
	case "launchd":
		removeLaunchd(homeDir)
	case "systemd":
		removeSystemd(homeDir)
	case "startup-folder":
		removeStartupFolder(homeDir)
	default:
		fmt.Fprintf(os.Stderr, "Error: unsupported platform\n")
		os.Exit(1)
	}
}

// restartAutostart reinstalls an installed autostart service so that it runs
// binPath, and reports whether it did. The Windows Startup folder starts
// peekm only at login, so it has no service to restart.
func restartAutostart(binPath, homeDir string) bool {
	switch detectServiceManager() {
	case "launchd":
		if fileExists(launchdPlistPath(homeDir)) {
			installLaunchd(serviceCommand(binPath, homeDir), homeDir)
			return true
		}
	case "systemd":
		if fileExists(systemdUnitPath(homeDir)) {
			installSystemd(serviceCommand(binPath, homeDir), homeDir)
			return true
		}
	}
	return false
}

// mustHomeDir returns $HOME or exits: setup commands cannot proceed without it.
func mustHomeDir() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot determine home directory: %v\n", err)
		os.Exit(1)
	}
	return homeDir
}

func detectServiceManager() string {
	switch runtime.GOOS {
	case "darwin":
		return "launchd"
	case "windows":
		return "startup-folder"
	default:
		if fileExists("/run/systemd/system") {
			return "systemd"
		}
		return ""
	}
}

const launchdLabel = "dev.peekm.agent"

func launchdPlistPath(homeDir string) string {
	return filepath.Join(homeDir, "Library", "LaunchAgents", launchdLabel+".plist")
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func launchdPlist(cmd []string, logPath, homeDir string) string {
	var args strings.Builder
	for _, a := range cmd {
		fmt.Fprintf(&args, "\n        <string>%s</string>", xmlEscaper.Replace(a))
	}
	logPath, homeDir = xmlEscaper.Replace(logPath), xmlEscaper.Replace(homeDir)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>%s
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
    <key>WorkingDirectory</key>
    <string>%s</string>
</dict>
</plist>
`, launchdLabel, args.String(), logPath, logPath, homeDir)
}

func installLaunchd(cmd []string, homeDir string) {
	plistPath := launchdPlistPath(homeDir)
	plistDir := filepath.Dir(plistPath)
	logPath := filepath.Join(homeDir, ".peekm", "peekm.log")
	plist := launchdPlist(cmd, logPath, homeDir)

	fmt.Println("\n  peekm autostart (launchd)")
	fmt.Println("  ========================")

	os.MkdirAll(plistDir, 0755)
	os.MkdirAll(filepath.Dir(logPath), 0755)

	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "    Error writing plist: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("    Created %s\n", plistPath)

	// Unload first (ignore errors if not loaded)
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	runCommand("launchctl", "bootout", domain+"/"+launchdLabel)
	if err := runCommand("launchctl", "bootstrap", domain, plistPath); err != nil {
		fmt.Fprintf(os.Stderr, "    Error loading service: %v\n", err)
		fmt.Println("    Try: launchctl bootstrap gui/$(id -u) " + plistPath)
		os.Exit(1)
	}
	fmt.Println("    Service loaded and started")
	fmt.Printf("    Logs: %s\n", logPath)
	printAutostartSuccess(cmd)
}

func removeLaunchd(homeDir string) {
	plistPath := launchdPlistPath(homeDir)

	fmt.Println("\n  Removing peekm autostart (launchd)")
	fmt.Println("  ===================================")

	if err := runCommand("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel)); err != nil {
		fmt.Println("    Service was not running")
	} else {
		fmt.Println("    Service stopped")
	}

	removeAndReport(plistPath)

	fmt.Print("\n  Done.\n\n")
}

const systemdUnit = "peekm"

func systemdUnitPath(homeDir string) string {
	return filepath.Join(homeDir, ".config", "systemd", "user", systemdUnit+".service")
}

var systemdEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$")

func systemdUnitFile(cmd []string, homeDir string) string {
	quoted := make([]string, len(cmd))
	for i, a := range cmd {
		quoted[i] = `"` + systemdEscaper.Replace(a) + `"`
	}
	return fmt.Sprintf(`[Unit]
Description=peekm markdown viewer with AI session tracking
After=network.target

[Service]
ExecStart=%s
Restart=on-failure
RestartSec=5
WorkingDirectory=%s

[Install]
WantedBy=default.target
`, strings.Join(quoted, " "), homeDir)
}

func installSystemd(cmd []string, homeDir string) {
	unitPath := systemdUnitPath(homeDir)
	unitDir := filepath.Dir(unitPath)
	unit := systemdUnitFile(cmd, homeDir)

	fmt.Println("\n  peekm autostart (systemd)")
	fmt.Println("  =========================")

	os.MkdirAll(unitDir, 0755)

	if err := os.WriteFile(unitPath, []byte(unit), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "    Error writing unit file: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("    Created %s\n", unitPath)

	if err := runCommand("systemctl", "--user", "daemon-reload"); err != nil {
		fmt.Fprintf(os.Stderr, "    Error reloading systemd: %v\n", err)
		os.Exit(1)
	}
	if err := runCommand("systemctl", "--user", "enable", systemdUnit); err != nil {
		fmt.Fprintf(os.Stderr, "    Error enabling service: %v\n", err)
		os.Exit(1)
	}
	// restart, not start: a running service must pick up a new command line.
	if err := runCommand("systemctl", "--user", "restart", systemdUnit); err != nil {
		fmt.Fprintf(os.Stderr, "    Error starting service: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("    Service enabled and started")
	fmt.Println("    Logs: journalctl --user -u peekm -f")
	printAutostartSuccess(cmd)
}

func removeSystemd(homeDir string) {
	unitPath := systemdUnitPath(homeDir)

	fmt.Println("\n  Removing peekm autostart (systemd)")
	fmt.Println("  ===================================")

	if err := runCommand("systemctl", "--user", "disable", "--now", systemdUnit); err != nil {
		fmt.Println("    Service was not running")
	} else {
		fmt.Println("    Service stopped and disabled")
	}

	removeAndReport(unitPath)

	runCommand("systemctl", "--user", "daemon-reload")
	fmt.Print("\n  Done.\n\n")
}

func startupFolderPath(homeDir string) string {
	return filepath.Join(homeDir, "AppData", "Roaming", "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "peekm.vbs")
}

// startupVBS returns a script that runs cmd hidden (no console window).
// WshShell.Run starts no shell, so cmd.exe has to apply the log redirection.
func startupVBS(cmd []string, logPath string) string {
	quoted := make([]string, len(cmd))
	for i, a := range cmd {
		quoted[i] = `"` + a + `"`
	}
	line := fmt.Sprintf(`cmd /s /c "%s > "%s" 2>&1"`, strings.Join(quoted, " "), logPath)
	return fmt.Sprintf("Set WshShell = CreateObject(\"WScript.Shell\")\nWshShell.Run \"%s\", 0, False\n",
		strings.ReplaceAll(line, `"`, `""`))
}

func installStartupFolder(cmd []string, homeDir string) {
	vbsPath := startupFolderPath(homeDir)
	logPath := filepath.Join(homeDir, ".peekm", "peekm.log")
	vbs := startupVBS(cmd, logPath)

	fmt.Println("\n  peekm autostart (Windows Startup folder)")
	fmt.Println("  =========================================")

	os.MkdirAll(filepath.Join(homeDir, ".peekm"), 0755)

	if err := os.WriteFile(vbsPath, []byte(vbs), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "    Error writing startup script: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("    Created %s\n", vbsPath)
	fmt.Printf("    Logs: %s\n", logPath)
	printAutostartSuccess(cmd)
}

func removeStartupFolder(homeDir string) {
	vbsPath := startupFolderPath(homeDir)

	fmt.Println("\n  Removing peekm autostart (Windows)")
	fmt.Println("  ====================================")

	removeAndReport(vbsPath)

	fmt.Print("\n  Done.\n\n")
}

func printAutostartSuccess(cmd []string) {
	fmt.Printf("\n  peekm will now start at login: %s\n", strings.Join(cmd, " "))
	fmt.Print("  Remove with: peekm uninstall\n\n")
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
