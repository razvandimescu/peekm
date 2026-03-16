# peekm

[![Go Report Card](https://goreportcard.com/badge/github.com/razvandimescu/peekm?v=2)](https://goreportcard.com/report/github.com/razvandimescu/peekm)
[![GitHub Release](https://img.shields.io/github/v/release/razvandimescu/peekm)](https://github.com/razvandimescu/peekm/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**[peekm.dev](https://peekm.dev)** — Track every AI coding session across all your projects in real time.

![peekm demo](assets/hero-demo.gif)

Claude Code can edit 50+ files in a single session. You see a git diff at the end. peekm gives you a real-time timeline of every session, every file edit, and every AI conversation — across all your projects, from a single local UI.

**All data stays local. No accounts. No telemetry. Open source (MIT).**

```bash
npx @peekm/peekm .
```

Or install globally: `npm i -g @peekm/peekm` | `brew install razvandimescu/tap/peekm`

Works with Claude Code today. More agents coming.

## What You Get

**AI session observability:**

- **Timeline** — every AI session across all projects, grouped by day. Duration, file counts, tool breakdown, All/Edits-only filter
- **Transcript viewer** — read the full AI conversation for any session. Tool calls, code diffs, and reasoning rendered inline
- **Memory browser** — cross-project dashboard of what Claude Code has learned about each project (`~/.claude/projects/*/memory/`)
- **Real-time notifications** — toast the instant AI modifies a file, with live reload via SSE

**Also included:**

- **Sharing** — share any file (Markdown, HTML, SVG, TXT) via LAN or public URL via `share.peekm.dev` (opt-in, 1-hour TTL, no account). HTML shares include co-located assets (CSS, JS, images)
- **File viewer** — VS Code-style sidebar with Markdown, HTML, SVG, and TXT support. Fuzzy search (Cmd/Ctrl+P), in-browser editing (Markdown), syntax highlighting, light/dark/auto themes
- **Smart folders** — "Recent AI Edits" surfaces files touched by AI in the last 24 hours
- **Persistent history** — events survive restarts via `~/.peekm/events.jsonl`

## How It Compares

Most alternatives focus on post-hoc analysis: Git AI annotates commits after the fact, transcript viewers render completed sessions as static HTML. peekm is real-time — you see AI activity as it happens, across all projects simultaneously, with session-level grouping and conversation replay.

## How It Works

peekm installs a PostToolUse hook into Claude Code that reports session metadata to a local HTTP server. File changes are detected via [fsnotify](https://github.com/fsnotify/fsnotify) and correlated with session data. Everything is served through a local web UI with SSE for live updates. No data leaves your machine unless you opt into public sharing.

Setup is automatic — peekm detects `~/.claude` on first run and configures hooks. Run `peekm setup claude-code --remove` to undo.

## Installation

**npm** (recommended)
```bash
npm i -g @peekm/peekm
```

Upgrade: `npm update -g @peekm/peekm` | Try without installing: `npx @peekm/peekm .`

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

## Ignoring Directories

peekm excludes `.*`, `node_modules`, `vendor`, `dist`, `venv` by default. Custom exclusions go in `.peekmignore`:

```
target
_site
*.tmp
```

`peekm --show-ignored` to see all active exclusions.

## Requirements

Claude Code (for AI tracking). macOS, Linux, or Windows. No runtime dependencies — peekm is a single static binary.

## FAQ

**Does peekm send my code anywhere?**
No. Everything stays on your machine. Public sharing is opt-in and only shares the specific file (and its co-located assets for HTML) through a relay — your codebase is never exposed.

**Why not just read the JSONL or git log?**
You can. peekm adds session correlation (which changes belong to which AI conversation), real-time notifications, and a visual timeline that makes it practical to monitor multiple projects simultaneously.

## Development

Go 1.23+. `go build -o peekm && go test -race ./...`

## Contributing

PRs welcome. [Open an issue](https://github.com/razvandimescu/peekm/issues) if something breaks.

## License

MIT — see [LICENSE](LICENSE).
