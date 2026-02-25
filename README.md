# peekm

[![Go Report Card](https://goreportcard.com/badge/github.com/razvandimescu/peekm?v=2)](https://goreportcard.com/report/github.com/razvandimescu/peekm)
[![GitHub Release](https://img.shields.io/github/v/release/razvandimescu/peekm)](https://github.com/razvandimescu/peekm/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A markdown viewer that tracks what AI coding agents change in your project.

![peekm demo](assets/hero-demo.gif)

```bash
npx @peekm/peekm .
```

peekm is a local markdown viewer with built-in AI session tracking. When Claude Code (or other AI agents) edits files, peekm shows you which files changed, which you haven't reviewed, and can commit session summaries to git for PR reviewers. Works without AI too — it's a solid markdown previewer with live reload, GitHub styling, and dark/light themes.

## Quick Start

```bash
npx @peekm/peekm .
```

Or `brew install razvandimescu/tap/peekm` for a permanent install.

To connect Claude Code (one time):

```bash
peekm setup claude-code
```

This installs PostToolUse hooks. Smart folders, timeline, and toast notifications start working immediately.

## AI Tracking

When connected, peekm tracks every file modification made by AI agents:

- **Smart folders** in the sidebar — "Recent AI Edits" surfaces files touched by AI in the last 24 hours
- **Timeline** (`/timeline`) — chronological view of all AI file modifications, grouped by day, color-coded by operation type
- **Toast notifications** — appear the instant AI modifies a file, click to navigate
- **Session info panel** — per-file dropdown showing session ID, tool, permission mode, timestamp

Events persist to `~/.peekm/events.jsonl` and survive restarts.

### Not Mem0 / Zep / Letta

Those tools help AI agents *remember* context. peekm helps *humans* see what AI did. Different problem. HANDOFF.md and `.context/` conventions are closer in spirit, but require manual authoring — peekm captures events automatically.

## Comparison

| | Glow | grip | VS Code Preview | peekm |
|---|------|------|-----------------|-------|
| **Live reload** | — | Manual | Yes | SSE with event replay |
| **Directory browser** | TUI list | — | File explorer | Web tree + smart folders |
| **AI tracking** | — | — | — | Smart folders, timeline, toasts |
| **Session summaries** | — | — | — | Committed to git |
| **Startup** | Fast | ~2s | Editor launch | < 100ms |
| **Dependencies** | Single binary | Python | VS Code | Single binary |
| **Offline** | Yes | No (GitHub API) | Yes | Yes |

## Features

- **VS Code-style sidebar** — tree view, collapsible folders, Cmd/Ctrl+B to toggle
- **Smart defaults** — auto-opens README.md or most recent file
- **Fuzzy search** — Cmd/Ctrl+P
- **Auto-reload on save** — Server-Sent Events with event replay
- **Theme switching** — Light/Dark/Auto, persisted
- **In-browser editing** — edit markdown directly, Ctrl+S to save
- **HTML export** — download self-contained HTML
- Single binary, cross-platform (macOS, Linux, Windows)
- Whitelist-based file access, CSRF protection, path traversal prevention

## Installation

**npm** (zero install)
```bash
npx @peekm/peekm .
```

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
peekm README.md         # view a file
peekm .                 # browse a directory
peekm -port 8080 .      # custom port
peekm -browser=false .  # don't open browser
```

| Flag | Default | Description |
|------|---------|-------------|
| `-port` | `6419` | Port to serve on |
| `-browser` | `true` | Auto-open browser |
| `-version` | `false` | Show version |
| `-no-ai-tracking` | `false` | Disable AI tracking |

| Subcommand | Description |
|------------|-------------|
| `peekm setup claude-code` | Install Claude Code hooks |
| `peekm setup claude-code --remove` | Remove hooks |

## AI Session Tracking Setup

```bash
peekm setup claude-code              # install
peekm setup claude-code --port 8080  # custom port
peekm setup claude-code --remove     # uninstall
```

Idempotent, non-destructive. Creates `~/.claude/peekm-hook.sh` and adds PostToolUse hooks to `~/.claude/settings.json`.

All data stays local. Nothing is sent anywhere.

## Ignoring Directories

peekm excludes `.*`, `node_modules`, `vendor`, `dist`, `venv` by default. Custom exclusions go in `.peekmignore`:

```
target
_site
*.tmp
```

`peekm --show-ignored` to see all active exclusions.

## How It Works

1. **Parse** — markdown to HTML via [goldmark](https://github.com/yuin/goldmark)
2. **Serve** — local HTTP server with graceful shutdown
3. **Watch** — file changes via [fsnotify](https://github.com/fsnotify/fsnotify)
4. **Reload** — SSE with event replay
5. **Track** — correlates AI session metadata with file changes

## Development

Go 1.21+. `go build -o peekm && go test -race ./...`

## Contributing

PRs welcome. [Open an issue](https://github.com/razvandimescu/peekm/issues) if something breaks.

## License

MIT — see [LICENSE](LICENSE).

## Acknowledgments

- [goldmark](https://github.com/yuin/goldmark) — Markdown parser
- [fsnotify](https://github.com/fsnotify/fsnotify) — File watching
- [chroma](https://github.com/alecthomas/chroma) — Syntax highlighting

## Related

- [glow](https://github.com/charmbracelet/glow) — Terminal markdown renderer
- [grip](https://github.com/joeyespo/grip) — GitHub-flavored markdown preview
- [VS Code Markdown Preview](https://code.visualstudio.com/docs/languages/markdown) — Built-in editor preview
