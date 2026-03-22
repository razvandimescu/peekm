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
		fmt.Println("  autostart    Install as background service [--remove]")
		os.Exit(1)
	}

	switch args[0] {
	case "claude-code":
		setupClaudeCode(args[1:])
	case "autostart":
		setupAutostart(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown setup target: %s\n", args[0])
		fmt.Println("Available: claude-code, autostart")
		os.Exit(1)
	}
}

func setupClaudeCode(args []string) {
	setupFlags := flag.NewFlagSet("setup claude-code", flag.ExitOnError)
	remove := setupFlags.Bool("remove", false, "Remove peekm hooks from Claude Code")
	hookPort := setupFlags.Int("port", 6419, "Port peekm runs on")
	setupFlags.Parse(args)

	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot determine home directory: %v\n", err)
		os.Exit(1)
	}

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
		fmt.Printf("    Added %d PostToolUse hook(s)\n", added)
	} else {
		fmt.Printf("    Hooks already configured (no changes)\n")
	}

	fmt.Println("\n  Setup complete. Restart Claude Code to activate.")
	fmt.Println("  To verify: modify a file with Claude Code and check peekm")
	fmt.Println("  for the AI session badge.")
	fmt.Println()
}

// installClaudeHooks creates the hook script and merges PostToolUse hooks into settings.json.
// Returns the number of hooks added and any error encountered.
func installClaudeHooks(claudeDir string, hookPort int) (int, error) {
	hookScriptPath := filepath.Join(claudeDir, "peekm-hook.sh")
	settingsPath := filepath.Join(claudeDir, "settings.json")

	hookScript := fmt.Sprintf(`#!/bin/bash
# peekm hook: Persist edit events to JSONL, heartbeat all tool calls
json=$(cat)
session_id=$(echo "$json" | jq -r '.session_id // empty')
tool_name=$(echo "$json" | jq -r '.tool_name // empty')

[ -z "$session_id" ] || [ -z "$tool_name" ] && exit 0

notify() {
    for port in %d 6419 8080 3000; do
        curl -sf -X POST -H 'Content-Type: application/json' \
            -d "$1" --max-time 0.05 \
            "http://localhost:$port/hook/file-modified" >/dev/null 2>&1 && return
    done
}

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

	settings, postToolUse, err := loadPostToolUseHooks(settingsPath)
	if err != nil {
		return 0, err
	}

	added := 0
	for _, matcher := range peekmHookMatchers {
		if hasPeekmHook(postToolUse, matcher, hookScriptPath) {
			continue
		}

		entry := map[string]interface{}{
			"matcher": matcher,
			"hooks":   []interface{}{hookEntry},
		}
		postToolUse = append(postToolUse, entry)
		added++
	}

	if added == 0 {
		return 0, nil
	}

	hooks := settings["hooks"].(map[string]interface{})
	hooks["PostToolUse"] = postToolUse

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("serializing settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, append(out, '\n'), 0644); err != nil {
		return 0, fmt.Errorf("writing %s: %w", settingsPath, err)
	}

	return added, nil
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

// peekmHookMatchers defines the PostToolUse tool names that peekm hooks into.
// Edit tools (Write, Edit, NotebookEdit) get full JSONL persistence.
// Other tools send heartbeats only (active badge + last tool display).
var peekmHookMatchers = []string{
	"Write", "Edit", "NotebookEdit",
	"Read", "Bash", "Grep", "Glob", "Agent",
}

// loadPostToolUseHooks reads settings.json and extracts the PostToolUse entries.
// Returns (settings, postToolUse, error). Missing file returns empty structures, nil error.
func loadPostToolUseHooks(settingsPath string) (map[string]interface{}, []interface{}, error) {
	settings := make(map[string]interface{})
	data, err := os.ReadFile(settingsPath)
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return nil, nil, fmt.Errorf("parsing %s: %w", settingsPath, err)
		}
	}

	hooks, _ := settings["hooks"].(map[string]interface{})
	if hooks == nil {
		hooks = make(map[string]interface{})
		settings["hooks"] = hooks
	}

	postToolUse, _ := hooks["PostToolUse"].([]interface{})
	return settings, postToolUse, nil
}

// hasPeekmHook checks if a PostToolUse entry for this matcher already has a peekm hook.
func hasPeekmHook(entries []interface{}, matcher, scriptPath string) bool {
	for _, entry := range entries {
		e, ok := entry.(map[string]interface{})
		if !ok || e["matcher"] != matcher {
			continue
		}
		hooks, ok := e["hooks"].([]interface{})
		if ok && containsPeekmHook(hooks, scriptPath) {
			return true
		}
	}
	return false
}

// filterPeekmHooks returns PostToolUse entries that don't reference the peekm hook script.
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

func removeClaudeCodeSetup(settingsPath, hookScriptPath string) {
	fmt.Println("\n  Removing AI Session Tracking")
	fmt.Println("  " + strings.Repeat("\u2500", 30))

	// Remove hook script
	if err := os.Remove(hookScriptPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "    Warning: %v\n", err)
	} else if err == nil {
		fmt.Printf("    Removed %s\n", hookScriptPath)
	}

	// Remove hooks from settings.json
	settings, postToolUse, err := loadPostToolUseHooks(settingsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "    Error parsing settings: %v\n", err)
		os.Exit(1)
	}
	if postToolUse == nil {
		fmt.Println("    No PostToolUse hooks found")
		fmt.Print("\n  Done.\n\n")
		return
	}

	// Filter out entries whose hooks reference the peekm script
	filtered, removed := filterPeekmHooks(postToolUse, hookScriptPath)

	if removed > 0 {
		hooks := settings["hooks"].(map[string]interface{})
		hooks["PostToolUse"] = filtered
		out, _ := json.MarshalIndent(settings, "", "  ")
		os.WriteFile(settingsPath, append(out, '\n'), 0644)
		fmt.Printf("    Removed %d hook(s) from settings.json\n", removed)
	} else {
		fmt.Println("    No peekm hooks found in settings")
	}

	fmt.Print("\n  Done.\n\n")
}

func setupAutostart(args []string) {
	setupFlags := flag.NewFlagSet("setup autostart", flag.ExitOnError)
	remove := setupFlags.Bool("remove", false, "Remove autostart service")
	setupFlags.Parse(args)

	if *remove {
		removeAutostart()
	} else {
		installAutostart()
	}
}

func installAutostart() {
	binPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot determine peekm binary path: %v\n", err)
		os.Exit(1)
	}
	binPath, err = filepath.EvalSymlinks(binPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot resolve binary path: %v\n", err)
		os.Exit(1)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot determine home directory: %v\n", err)
		os.Exit(1)
	}

	switch detectServiceManager() {
	case "launchd":
		installLaunchd(binPath, homeDir)
	case "systemd":
		installSystemd(binPath, homeDir)
	case "startup-folder":
		installStartupFolder(binPath, homeDir)
	default:
		fmt.Fprintf(os.Stderr, "Error: unsupported platform (need launchd on macOS, systemd on Linux, or Windows)\n")
		os.Exit(1)
	}
}

func removeAutostart() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot determine home directory: %v\n", err)
		os.Exit(1)
	}

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

func installLaunchd(binPath, homeDir string) {
	plistDir := filepath.Join(homeDir, "Library", "LaunchAgents")
	plistPath := filepath.Join(plistDir, launchdLabel+".plist")
	logPath := filepath.Join(homeDir, ".peekm", "peekm.log")

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>-browser=false</string>
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
`, launchdLabel, binPath, logPath, logPath, homeDir)

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
	printAutostartSuccess()
}

func removeLaunchd(homeDir string) {
	plistPath := filepath.Join(homeDir, "Library", "LaunchAgents", launchdLabel+".plist")

	fmt.Println("\n  Removing peekm autostart (launchd)")
	fmt.Println("  ===================================")

	if err := runCommand("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel)); err != nil {
		fmt.Println("    Service was not running")
	} else {
		fmt.Println("    Service stopped")
	}

	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "    Error removing plist: %v\n", err)
	} else {
		fmt.Printf("    Removed %s\n", plistPath)
	}

	fmt.Print("\n  Done.\n\n")
}

const systemdUnit = "peekm"

func installSystemd(binPath, homeDir string) {
	unitDir := filepath.Join(homeDir, ".config", "systemd", "user")
	unitPath := filepath.Join(unitDir, systemdUnit+".service")

	unit := fmt.Sprintf(`[Unit]
Description=peekm markdown viewer with AI session tracking
After=network.target

[Service]
ExecStart=%s -browser=false
Restart=on-failure
RestartSec=5
WorkingDirectory=%s

[Install]
WantedBy=default.target
`, binPath, homeDir)

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
	if err := runCommand("systemctl", "--user", "enable", "--now", systemdUnit); err != nil {
		fmt.Fprintf(os.Stderr, "    Error enabling service: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("    Service enabled and started")
	fmt.Println("    Logs: journalctl --user -u peekm -f")
	printAutostartSuccess()
}

func removeSystemd(homeDir string) {
	unitPath := filepath.Join(homeDir, ".config", "systemd", "user", systemdUnit+".service")

	fmt.Println("\n  Removing peekm autostart (systemd)")
	fmt.Println("  ===================================")

	if err := runCommand("systemctl", "--user", "disable", "--now", systemdUnit); err != nil {
		fmt.Println("    Service was not running")
	} else {
		fmt.Println("    Service stopped and disabled")
	}

	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "    Error removing unit file: %v\n", err)
	} else {
		fmt.Printf("    Removed %s\n", unitPath)
	}

	runCommand("systemctl", "--user", "daemon-reload")
	fmt.Print("\n  Done.\n\n")
}

func startupFolderPath(homeDir string) string {
	return filepath.Join(homeDir, "AppData", "Roaming", "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "peekm.vbs")
}

func installStartupFolder(binPath, homeDir string) {
	vbsPath := startupFolderPath(homeDir)
	logPath := filepath.Join(homeDir, ".peekm", "peekm.log")

	// VBScript wrapper runs peekm hidden (no console window)
	vbs := fmt.Sprintf(`Set WshShell = CreateObject("WScript.Shell")
WshShell.Run """%s"" -browser=false > ""%s"" 2>&1", 0, False
`, binPath, logPath)

	fmt.Println("\n  peekm autostart (Windows Startup folder)")
	fmt.Println("  =========================================")

	os.MkdirAll(filepath.Join(homeDir, ".peekm"), 0755)

	if err := os.WriteFile(vbsPath, []byte(vbs), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "    Error writing startup script: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("    Created %s\n", vbsPath)
	fmt.Printf("    Logs: %s\n", logPath)
	printAutostartSuccess()
}

func removeStartupFolder(homeDir string) {
	vbsPath := startupFolderPath(homeDir)

	fmt.Println("\n  Removing peekm autostart (Windows)")
	fmt.Println("  ====================================")

	if err := os.Remove(vbsPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "    Error removing startup script: %v\n", err)
	} else {
		fmt.Printf("    Removed %s\n", vbsPath)
	}

	fmt.Print("\n  Done.\n\n")
}

func printAutostartSuccess() {
	fmt.Print("\n  peekm will now start automatically on login.\n")
	fmt.Print("  Remove with: peekm setup autostart --remove\n\n")
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
