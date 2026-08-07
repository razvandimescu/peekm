package main

// pi harness integration: auto-installed extension that reports tool activity
// to /hook/file-modified (mirroring the Claude Code hook), plus a parser for
// pi's v3 session JSONL so transcripts and the timeline cover pi sessions.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
)

// ---------- extension auto-setup ----------

func piAgentDir() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(homeDir, ".pi", "agent")
}

func piSessionsDir() string {
	agentDir := piAgentDir()
	if agentDir == "" {
		return ""
	}
	return filepath.Join(agentDir, "sessions")
}

// piExtensionTemplate is the TypeScript extension pi auto-loads from
// ~/.pi/agent/extensions. It mirrors peekm-hook.sh: edit events are persisted
// to ~/.peekm/events.jsonl then POSTed; other tools send heartbeats only.
// No template literals — the Go raw string cannot contain backticks.
const piExtensionTemplate = `// Managed by peekm - rewritten on every peekm startup. Do not edit.
// Reports pi tool activity to peekm's AI session tracking.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { appendFileSync, mkdirSync } from "node:fs";
import { homedir } from "node:os";
import { isAbsolute, join, resolve } from "node:path";

const PORTS = [%d, 6419, 8080, 3000];
const TOOL_NAMES: Record<string, string> = %s;
const EDIT_TOOLS = new Set(["edit", "write"]);

async function notify(payload: unknown): Promise<void> {
  const body = JSON.stringify(payload);
  for (const port of PORTS) {
    try {
      await fetch("http://127.0.0.1:" + port + "/hook/file-modified", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body,
        signal: AbortSignal.timeout(150),
      });
      return;
    } catch {
      // peekm not listening on this port - try the next
    }
  }
}

export default function (pi: ExtensionAPI) {
  pi.on("tool_result", async (event, ctx) => {
    const sid = ctx.sessionManager.getSessionId();
    if (!sid) return;
    const input = (event.input || {}) as Record<string, unknown>;
    const tool = TOOL_NAMES[event.toolName] || event.toolName;
    const base = { sid, tool, ts: new Date().toISOString(), src: "pi" };
    const rawPath = typeof input.path === "string" ? input.path : "";

    if (EDIT_TOOLS.has(event.toolName) && rawPath && !event.isError) {
      const evt = {
        ...base,
        path: isAbsolute(rawPath) ? rawPath : resolve(ctx.cwd, rawPath),
        tuid: event.toolCallId,
        cwd: ctx.cwd,
      };
      try {
        const dir = join(homedir(), ".peekm");
        mkdirSync(dir, { recursive: true });
        appendFileSync(join(dir, "events.jsonl"), JSON.stringify(evt) + "\n");
      } catch {
        // best-effort persistence; the POST below still notifies a live peekm
      }
      await notify(evt);
    } else {
      const detail = String(input.command || input.pattern || rawPath || "").slice(0, 300);
      await notify({ ...base, detail });
    }
  });
}
`

func piExtensionPath(agentDir string) string {
	return filepath.Join(agentDir, "extensions", "peekm.ts")
}

// piToolNamesTS renders piToolNames as a TypeScript object literal so the
// extension and the transcript parser share one mapping. Keys are sorted so
// installPiExtension's content comparison stays stable.
func piToolNamesTS() string {
	keys := make([]string, 0, len(piToolNames))
	for k := range piToolNames {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{ ")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s: %q", k, piToolNames[k])
	}
	b.WriteString(" }")
	return b.String()
}

// installPiExtension writes the peekm extension into pi's global extensions
// directory. Returns true when the file did not exist before.
func installPiExtension(agentDir string, hookPort int) (bool, error) {
	extPath := piExtensionPath(agentDir)
	source := []byte(fmt.Sprintf(piExtensionTemplate, hookPort, piToolNamesTS()))

	existing, err := os.ReadFile(extPath)
	created := os.IsNotExist(err)
	if err == nil && bytes.Equal(existing, source) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(extPath), 0755); err != nil {
		return false, fmt.Errorf("creating pi extensions dir: %w", err)
	}
	if err := os.WriteFile(extPath, source, 0644); err != nil {
		return false, fmt.Errorf("writing pi extension: %w", err)
	}
	return created, nil
}

// autoSetupPiHooks keeps the pi extension up-to-date. Silent when pi is not
// installed — unlike Claude Code, pi is not advertised.
func autoSetupPiHooks() {
	agentDir := piAgentDir()
	if agentDir == "" {
		return
	}
	if _, err := os.Stat(agentDir); os.IsNotExist(err) {
		return
	}
	created, err := installPiExtension(agentDir, *port)
	if err != nil {
		log.Printf("Warning: pi auto-setup failed: %v", err)
		return
	}
	if created {
		log.Printf("Connected to pi (extension installed). Restart pi to activate.")
	}
}

// setupPi handles "peekm setup pi [--remove] [--port PORT]".
func setupPi(args []string) {
	setupFlags := flag.NewFlagSet("setup pi", flag.ExitOnError)
	remove := setupFlags.Bool("remove", false, "Remove peekm extension from pi")
	hookPort := setupFlags.Int("port", 6419, "Port peekm runs on")
	setupFlags.Parse(args)

	agentDir := piAgentDir()
	if agentDir == "" {
		fmt.Fprintln(os.Stderr, "Error: cannot determine home directory")
		os.Exit(1)
	}
	extPath := piExtensionPath(agentDir)

	if *remove {
		if err := os.Remove(extPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Removed %s\n", extPath)
		return
	}

	if _, err := os.Stat(agentDir); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "Error: pi not found (~/.pi/agent missing)")
		os.Exit(1)
	}
	if _, err := installPiExtension(agentDir, *hookPort); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Installed %s\nRestart pi to activate.\n", extPath)
}

// ---------- transcript resolution ----------

// encodePiProjectDir maps an absolute directory to pi's session folder name:
// slashes become dashes, wrapped in "--" ("/a/b" → "--a-b--").
func encodePiProjectDir(dir string) string {
	return "--" + strings.ReplaceAll(strings.TrimPrefix(dir, "/"), "/", "-") + "--"
}

// piSessionIDFromName extracts the session UUID from "<timestamp>_<uuid>.jsonl".
func piSessionIDFromName(name string) string {
	base := strings.TrimSuffix(name, ".jsonl")
	idx := strings.LastIndexByte(base, '_')
	if idx < 0 {
		return ""
	}
	id := base[idx+1:]
	if !sessionIDRe.MatchString(id) {
		return ""
	}
	return id
}

// resolvePiTranscriptPath finds a pi session file by UUID. Tries the current
// browseDir's project folder first, then scans all of ~/.pi/agent/sessions.
func resolvePiTranscriptPath(sessionID string) string {
	if !sessionIDRe.MatchString(sessionID) {
		return ""
	}
	root := piSessionsDir()
	if root == "" {
		return ""
	}
	suffix := "_" + sessionID + ".jsonl"

	fileMutex.RLock()
	dir := browseDir
	fileMutex.RUnlock()
	if p := piSessionFileIn(filepath.Join(root, encodePiProjectDir(dir)), suffix); p != "" {
		return p
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if p := piSessionFileIn(filepath.Join(root, entry.Name()), suffix); p != "" {
			return p
		}
	}
	return ""
}

func piSessionFileIn(dir, suffix string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), suffix) {
			return filepath.Join(dir, entry.Name())
		}
	}
	return ""
}

// ---------- transcript parsing ----------

// piEntry is one line of a pi session file (excluding the header).
type piEntry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  string          `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

// piMessage is the AgentMessage payload of a "message" entry. Fields cover the
// roles peekm renders: user, assistant, toolResult, bashExecution.
type piMessage struct {
	Role       string          `json:"role"`
	Model      string          `json:"model"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"toolCallId"`
	Command    string          `json:"command"`
	Output     string          `json:"output"`
	ExitCode   *int            `json:"exitCode"`
}

// parsePiTranscript reads a pi session file and returns conversation turns,
// reusing the Claude transcript post-processing pipeline.
func parsePiTranscript(path string) ([]transcriptTurn, error) {
	entries, err := readPiEntries(path)
	if err != nil {
		return nil, err
	}
	entries = piActiveBranch(entries)

	md := newSafeMarkdownRenderer()
	var turns []transcriptTurn
	for _, e := range entries {
		if e.Type != "message" {
			continue
		}
		turn, ok := piMessageToTurn(e, md)
		if !ok {
			continue
		}
		turns = append(turns, turn)
	}
	turns = pairToolResults(turns)
	turns = removeEmptyTurns(turns)
	turns = mergeConsecutiveTurns(turns)
	turns = markTurnCollapsible(turns)
	turns = expandFinalTurn(turns)
	return turns, nil
}

func readPiEntries(path string) ([]piEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := newJSONLScanner(f)
	var entries []piEntry
	for scanner.Scan() {
		var e piEntry
		if json.Unmarshal(scanner.Bytes(), &e) != nil {
			continue
		}
		if e.Type == "session" || e.ID == "" {
			continue // header or malformed line
		}
		entries = append(entries, e)
	}
	return entries, scanner.Err()
}

// piActiveBranch filters entries to the live conversation path. Entries form a
// tree via parentId; every append moves the leaf, so the last entry in the file
// is the current leaf. Walking its parent chain to the root selects the active
// branch and drops abandoned ones.
func piActiveBranch(entries []piEntry) []piEntry {
	if len(entries) == 0 {
		return entries
	}
	parent := make(map[string]string, len(entries))
	for _, e := range entries {
		parent[e.ID] = e.ParentID
	}

	onBranch := make(map[string]bool)
	for id := entries[len(entries)-1].ID; id != "" && !onBranch[id]; id = parent[id] {
		onBranch[id] = true
	}

	filtered := entries[:0]
	for _, e := range entries {
		if onBranch[e.ID] {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

func piMessageToTurn(entry piEntry, md goldmark.Markdown) (transcriptTurn, bool) {
	var msg piMessage
	if json.Unmarshal(entry.Message, &msg) != nil {
		return transcriptTurn{}, false
	}

	var role string
	var blocks []contentBlock
	switch msg.Role {
	case "user":
		role = "user"
		blocks = piTextBlocks(msg.Content, md)
	case "assistant":
		role = "assistant"
		blocks = piAssistantBlocks(msg.Content, md)
	case "toolResult":
		role = "user"
		blocks = piToolResultBlocks(msg, md)
	case "bashExecution":
		role = "user"
		blocks = []contentBlock{piBashBlock(msg, md)}
	default:
		return transcriptTurn{}, false
	}
	if len(blocks) == 0 {
		return transcriptTurn{}, false
	}
	return transcriptTurn{Role: role, Blocks: blocks, Model: msg.Model, Timestamp: entry.Timestamp}, true
}

// extractPiContent handles pi's content field: a plain string or an array of
// {type:"text"|"image"} blocks (images carry data/mimeType, unlike Claude's
// nested source object).
func extractPiContent(raw json.RawMessage) (string, []imageData) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", nil
	}
	var buf strings.Builder
	var images []imageData
	for _, p := range parts {
		switch p.Type {
		case "text":
			if buf.Len() > 0 {
				buf.WriteByte('\n')
			}
			buf.WriteString(p.Text)
		case "image":
			if p.Data != "" && p.MimeType != "" {
				images = append(images, imageData{MediaType: p.MimeType, Data: p.Data})
			}
		}
	}
	return buf.String(), images
}

func piTextBlocks(raw json.RawMessage, md goldmark.Markdown) []contentBlock {
	text, _ := extractPiContent(raw)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []contentBlock{newTextBlock(md, text, false)}
}

func piAssistantBlocks(raw json.RawMessage, md goldmark.Markdown) []contentBlock {
	var parts []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var blocks []contentBlock
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				blocks = append(blocks, newTextBlock(md, p.Text, true))
			}
		case "thinking":
			if p.Thinking != "" {
				blocks = append(blocks, newThinkingBlock(p.Thinking))
			}
		case "toolCall":
			blocks = append(blocks, piToolUseBlock(p.ID, p.Name, p.Arguments))
		}
	}
	return blocks
}

func piToolUseBlock(id, name string, args json.RawMessage) contentBlock {
	displayName, m := normalizePiToolInput(name, parseToolInput(args))
	return contentBlock{
		Type:            "tool_use",
		ToolName:        displayName,
		ToolInput:       formatToolInputFromMap(m, args),
		ToolID:          id,
		ToolDisplayName: displayName,
		ToolSummary:     toolSummaryFromMap(displayName, m),
		ToolInputHTML:   formatStructuredFromMap(displayName, m),
	}
}

func piToolResultBlocks(msg piMessage, md goldmark.Markdown) []contentBlock {
	text, images := extractPiContent(msg.Content)
	if text == "" && len(images) == 0 {
		return nil
	}
	return []contentBlock{newToolResultBlock(md, msg.ToolCallID, text, images)}
}

// piBashBlock renders a user-invoked shell command (pi's "!" prefix) as a Bash
// tool call with its captured output attached as the paired result.
func piBashBlock(msg piMessage, md goldmark.Markdown) contentBlock {
	block := contentBlock{
		Type:            "tool_use",
		ToolName:        "Bash",
		ToolDisplayName: "Bash",
		ToolInput:       msg.Command,
		ToolSummary:     "$ " + truncateString(msg.Command, 80),
		ToolInputHTML:   formatBashInput(map[string]interface{}{"command": msg.Command}),
	}
	output := msg.Output
	if msg.ExitCode != nil && *msg.ExitCode != 0 {
		output = fmt.Sprintf("%s\n(exit code %d)", output, *msg.ExitCode)
	}
	if strings.TrimSpace(output) != "" {
		// Truncate before fencing so the closing fence survives.
		result := contentBlock{
			Type: "tool_result",
			HTML: renderMarkdownToHTML(md, "```\n"+truncateString(output, toolResultMaxChars)+"\n```"),
		}
		block.Result = &result
	}
	return block
}

// piToolNames maps pi's built-in tools to their Claude Code equivalents so
// summaries, icons, and structured input rendering reuse existing formatters.
var piToolNames = map[string]string{
	"bash":  "Bash",
	"read":  "Read",
	"edit":  "Edit",
	"write": "Write",
	"grep":  "Grep",
	"find":  "Glob",
	"ls":    "List",
}

// normalizePiToolInput rewrites a pi tool call into Claude Code naming: the
// tool name and argument keys (path → file_path, edits[].oldText → old_string)
// so downstream formatters treat both harnesses identically. args is mutated
// in place — callers own the freshly parsed map.
func normalizePiToolInput(name string, args map[string]interface{}) (string, map[string]interface{}) {
	display, known := piToolNames[name]
	if !known {
		display = capitalizeFirst(name)
	}
	if args == nil {
		return display, nil
	}
	switch name {
	case "read", "edit", "write", "ls":
		if p, ok := args["path"].(string); ok {
			args["file_path"] = p
			delete(args, "path")
		}
	}
	if name == "edit" {
		display = normalizePiEdits(args)
	}
	return display, args
}

// normalizePiEdits converts pi's edits[]{oldText,newText} to Claude key names.
// A single edit renders as Edit, multiple as MultiEdit.
func normalizePiEdits(m map[string]interface{}) string {
	edits, ok := m["edits"].([]interface{})
	if !ok || len(edits) == 0 {
		return "Edit"
	}
	converted := make([]interface{}, 0, len(edits))
	for _, e := range edits {
		em, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		converted = append(converted, map[string]interface{}{
			"old_string": em["oldText"],
			"new_string": em["newText"],
		})
	}
	if len(converted) == 1 {
		single := converted[0].(map[string]interface{})
		m["old_string"] = single["old_string"]
		m["new_string"] = single["new_string"]
		delete(m, "edits")
		return "Edit"
	}
	m["edits"] = converted
	return "MultiEdit"
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}

// ---------- timeline discovery ----------

// readPiSessionCwd reads the working directory from a pi session header line.
func readPiSessionCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var header struct {
		Type string `json:"type"`
		CWD  string `json:"cwd"`
	}
	if json.NewDecoder(bufio.NewReaderSize(f, 4096)).Decode(&header) != nil {
		return ""
	}
	if header.Type != "session" {
		return ""
	}
	return header.CWD
}

// discoverPiSessions scans pi's session store for sessions under baseDir not
// already tracked via events.jsonl, mirroring discoverTranscriptSessions.
func discoverPiSessions(baseDir string, knownSessionIDs map[string]bool) []timelineSession {
	root := piSessionsDir()
	if root == "" {
		return nil
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	// Cheap dir-name pre-filter: a session cwd within baseDir encodes to the
	// base encoding itself or extends it with more dashed segments. Encoding is
	// lossy ("-" is ambiguous), so matches are confirmed against the header cwd
	// in piSessionsIn; misses are definitive and skip all file opens.
	enc := encodePiProjectDir(baseDir)
	prefix := strings.TrimSuffix(enc, "--") + "-"

	var sessions []timelineSession
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		if name := dir.Name(); name != enc && !strings.HasPrefix(name, prefix) {
			continue
		}
		sessions = append(sessions, piSessionsIn(filepath.Join(root, dir.Name()), baseDir, knownSessionIDs)...)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].newestTime.After(sessions[j].newestTime)
	})
	return sessions
}

// piSessionsIn builds a timeline session per pi session file in one project
// directory. The header's cwd (not the encoded dir name) scopes it to baseDir.
func piSessionsIn(sessionsDir, baseDir string, knownSessionIDs map[string]bool) []timelineSession {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil
	}
	var sessions []timelineSession
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		sessionID := piSessionIDFromName(name)
		if sessionID == "" || knownSessionIDs[sessionID] {
			continue
		}
		path := filepath.Join(sessionsDir, name)
		cwd := readPiSessionCwd(path)
		if cwd == "" || !dirWithinBase(cwd, baseDir) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		sessions = append(sessions, conversationSession(sessionID, filepath.Base(cwd), "pi", path, info.ModTime()))
	}
	return sessions
}
