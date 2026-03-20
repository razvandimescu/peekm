# peekm

**Real-time observability for AI coding sessions.**

[![GitHub stars](https://img.shields.io/github/stars/razvandimescu/peekm)](https://github.com/razvandimescu/peekm)
[![Go Report Card](https://goreportcard.com/badge/github.com/razvandimescu/peekm?v=2)](https://goreportcard.com/report/github.com/razvandimescu/peekm)
[![GitHub Release](https://img.shields.io/github/v/release/razvandimescu/peekm)](https://github.com/razvandimescu/peekm/releases)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

**[peekm.dev](https://peekm.dev)** — Track every AI coding session across all your projects in real time.

![peekm — watching a live Claude Code session across multiple projects](assets/hero-demo.gif)

> Claude Code edits 50+ files in a single session. All you get is a git diff.
>
> peekm gives you a live timeline of every session, every file edit, and every AI conversation — across all your projects, from one local UI.

**All data stays local. No accounts. No telemetry. Open source (Apache 2.0).**

## Getting Started

```bash
npx @peekm/peekm .
```

Requires [Claude Code](https://claude.ai/download) for AI tracking. Setup is automatic on first run.

Start a Claude Code session in any project. Watch the timeline update in real time.

Or install globally: `npm i -g @peekm/peekm` | `brew install razvandimescu/tap/peekm`

Works with Claude Code today. More agents coming.

## What You Get

**AI session observability:**

- **Timeline** — every AI session across all projects, grouped by day. Duration, file counts, tool breakdown, All/Edits-only filter
- **Transcript viewer** — read the full AI conversation for any session. Tool calls, code diffs, and reasoning rendered inline
- **Memory browser** — see what Claude remembers about each of your projects
- **Real-time notifications** — toast the instant AI modifies a file, with live reload via SSE

**Also included:**

- **Sharing** — share any file (Markdown, HTML, SVG, TXT) via LAN or public URL via `share.peekm.dev` (opt-in, 1-hour TTL, no account). HTML shares include co-located assets (CSS, JS, images)
- **File viewer** — VS Code-style sidebar with Markdown, HTML, SVG, and TXT support. Fuzzy search (Cmd/Ctrl+P), in-browser editing (Markdown), syntax highlighting, light/dark/auto themes
- **Smart folders** — "Recent AI Edits" surfaces files touched by AI in the last 24 hours
- **Persistent history** — events survive restarts via `~/.peekm/events.jsonl`

## Why This Exists

There's no tool that shows you what AI coding agents are doing *while they're doing it*. Git diffs show results. Transcript viewers show history. peekm shows the live session — across all your projects simultaneously, with session-level grouping and conversation replay.

## How It Works

peekm installs a PostToolUse hook into Claude Code that reports session metadata to a local HTTP server. File changes are correlated with session data and served through a local web UI with SSE for live updates. No data leaves your machine unless you opt into public sharing.

Setup is automatic — peekm detects `~/.claude` on first run and configures hooks. Run `peekm setup claude-code --remove` to undo.

## Installation

**npm** (recommended — manages updates automatically)
```bash
npm i -g @peekm/peekm
```

Try without installing: `npx @peekm/peekm .` | Upgrade: `npm update -g @peekm/peekm`

**Homebrew**
```bash
brew install razvandimescu/tap/peekm
```

**curl**
```bash
curl -L https://github.com/razvandimescu/peekm/releases/latest/download/peekm_$(uname -s)_$(uname -m).tar.gz | tar xz && sudo mv peekm /usr/local/bin/
```

**Go**
```bash
go install github.com/razvandimescu/peekm@latest
```

[All releases](https://github.com/razvandimescu/peekm/releases) — macOS, Linux, Windows.

## Usage

```bash
peekm .                 # browse current directory — AI tracking auto-starts
peekm README.md         # view a single file
peekm -port 8080 .      # custom port
```

AI tracking is on by default when `~/.claude` exists. Disable with `-no-ai-tracking`.

| Flag | Default | Description |
|------|---------|-------------|
| `-port` | `6419` | Port to serve on |
| `-browser` | `true` | Auto-open browser |
| `-no-ai-tracking` | `false` | Disable AI tracking |

**Sharing** — click the share button in the top bar.

- **LAN** (default): token-scoped URL on your local network. Recipients see a read-only rendered view with live reload.
- **Public** (opt-in): click "Make public" to tunnel through `share.peekm.dev`. HTTPS URL, expires after 1 hour, no account needed.

<details>
<summary><strong>Ignoring Directories</strong></summary>

peekm excludes `.*`, `node_modules`, `vendor`, `dist`, `venv` by default. Custom exclusions go in `.peekmignore`:

```
target
_site
*.tmp
```

`peekm --show-ignored` to see all active exclusions.

</details>

## Requirements

Claude Code (for AI tracking). macOS, Linux, or Windows. No runtime dependencies — peekm is a single static binary.

## FAQ

**Does peekm send my code anywhere?**
No. Everything stays on your machine. Public sharing is opt-in and only shares the specific file through a relay — your codebase is never exposed.

**Why not just read the JSONL or git log?**
You can. peekm adds session correlation (which changes belong to which AI conversation), real-time notifications, and a visual timeline that makes it practical to monitor multiple projects simultaneously.

**Does it work with Cursor / Copilot / Aider?**
Not yet. Claude Code exposes PostToolUse hooks that make real-time tracking possible — we're adding more agents as they expose similar capabilities. [Open an issue](https://github.com/razvandimescu/peekm/issues) to request yours.

## Development

Go 1.23+. `go build -o peekm && go test -race ./...`

## Contributing

PRs welcome. [Open an issue](https://github.com/razvandimescu/peekm/issues) if something breaks.

## License

Apache 2.0 — see [LICENSE](LICENSE).

---

Try it now: `npx @peekm/peekm .`
