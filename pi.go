package main

// pi harness integration: auto-installed extension that reports tool activity
// to /hook/file-modified (mirroring the Claude Code hook), plus a parser for
// pi's v3 session JSONL so transcripts and the timeline cover pi sessions.

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/razvandimescu/peekm/transcript"
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
const TOOL_NAMES: Record<string, string> = { %s };
const EDIT_TOOLS = new Set(["edit", "write"]);

let lastPort = 0; // last port that answered - tried first to skip dead probes

async function notify(payload: unknown): Promise<void> {
  const body = JSON.stringify(payload);
  const ports = lastPort ? [lastPort, ...PORTS.filter((p) => p !== lastPort)] : PORTS;
  for (const port of ports) {
    try {
      await fetch("http://127.0.0.1:" + port + "/hook/file-modified", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body,
        signal: AbortSignal.timeout(150),
      });
      lastPort = port;
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

// piToolNamesTS renders transcript.PiToolNames as a TS record body, keeping
// the extension's tool naming derived from the one canonical map.
func piToolNamesTS() string {
	names := make([]string, 0, len(transcript.PiToolNames))
	for name := range transcript.PiToolNames {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, len(names))
	for i, name := range names {
		pairs[i] = fmt.Sprintf("%s: %q", name, transcript.PiToolNames[name])
	}
	return strings.Join(pairs, ", ")
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
	probe := func(dir string) string { return piSessionFileIn(dir, suffix) }
	return findInSessionStore(root, encodePiProjectDir(currentBrowseDir()), probe)
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

// ---------- timeline discovery ----------

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

	// Dir names encode the session cwd, so any session under baseDir lives in
	// a dir sharing baseDir's encoded prefix. Dash-encoding false positives
	// (e.g. /a/bc vs /a/b) are fine — the header cwd check below still runs.
	prefix := strings.TrimSuffix(encodePiProjectDir(baseDir), "--")

	var sessions []timelineSession
	for _, dir := range dirs {
		if !dir.IsDir() || !strings.HasPrefix(dir.Name(), prefix) {
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
		cwd := transcript.PiSessionCwd(path)
		if cwd == "" || !dirWithinBase(cwd, baseDir) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		sessions = append(sessions, conversationSession(
			path, sessionID, filepath.Base(cwd), transcript.HarnessPi, info.ModTime()))
	}
	return sessions
}
