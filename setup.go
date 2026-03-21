package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// runSetup handles the "peekm setup" subcommand
func runSetup(args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: peekm setup claude-code [--remove] [--port PORT]")
		fmt.Println("\nConfigures Claude Code to send file modification events to peekm.")
		os.Exit(1)
	}

	switch args[0] {
	case "claude-code":
		setupClaudeCode(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown setup target: %s\n", args[0])
		fmt.Println("Available: claude-code")
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

if [ -n "$file_path" ]; then
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
    detail=$(echo "$json" | jq -r '.tool_input | .description // .command // .file_path // .pattern // .prompt // empty' | head -c 120)
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

// isClaudeHooksInstalled checks if peekm hooks are fully configured.
func isClaudeHooksInstalled(claudeDir string) bool {
	hookScriptPath := filepath.Join(claudeDir, "peekm-hook.sh")
	if _, err := os.Stat(hookScriptPath); err != nil {
		return false
	}

	settingsPath := filepath.Join(claudeDir, "settings.json")
	_, postToolUse, err := loadPostToolUseHooks(settingsPath)
	if err != nil || postToolUse == nil {
		return false
	}

	for _, matcher := range peekmHookMatchers {
		if !hasPeekmHook(postToolUse, matcher, hookScriptPath) {
			return false
		}
	}
	return true
}

// autoSetupClaudeHooks silently configures Claude Code hooks on first run.
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

	if isClaudeHooksInstalled(claudeDir) {
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
