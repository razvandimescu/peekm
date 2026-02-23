# peekm

[![Go Report Card](https://goreportcard.com/badge/github.com/razvandimescu/peekm?v=2)](https://goreportcard.com/report/github.com/razvandimescu/peekm)
[![GitHub Release](https://img.shields.io/github/v/release/razvandimescu/peekm)](https://github.com/razvandimescu/peekm/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> See every file your AI touches — live, tracked, reviewable

![peekm demo](assets/hero-demo.gif)

A markdown viewer that knows when AI agents modify your files. Smart folders surface unreviewed AI edits. A timeline shows the full history. Toast notifications arrive the instant something changes. Everything persists across restarts.

```bash
npx @peekm/peekm .
```

Zero config. Single binary. Works without AI too — live reload, GitHub styling, dark/light themes.

**Perfect for:**

- **Claude Code / AI agent users** — Smart folders show which files AI touched and which you haven't reviewed yet
- **Multi-session projects** — Timeline view shows every AI modification, grouped by day, across all sessions
- **Anyone who reads markdown** — Zero-config live preview with GitHub styling and directory browsing

[Quick Start](#quick-start) • [What You Get](#what-you-get) • [Comparison](#peekm-vs-the-world)

## Quick Start

```bash
npx @peekm/peekm .
```

**That's it.** No config, no runtime dependencies. Or `brew install razvandimescu/tap/peekm` for a permanent install.

**Connect to Claude Code** (one command, one time):

```bash
peekm setup claude-code
```

Smart folders, timeline, and toast notifications activate automatically. [Details](#ai-session-tracking)

## What You Get

### Smart Folders

The sidebar shows two virtual folders that update in real time:
- **Recent AI Edits** — files AI created or modified in the current session
- **Unreviewed** — files AI touched that you haven't opened yet

### AI Timeline

A chronological, color-coded view of every AI file modification, grouped by day. Each entry shows the file, operation type (Write/Edit), session ID, and relative timestamp.

### Session Persistence

AI session events persist to `~/.peekm/events.jsonl`. Restart peekm, and your smart folders and timeline are still there.

### Toast Notifications

The instant AI creates or modifies a markdown file, a toast notification slides in. Click it to jump to the file. Notification history is accessible from the bell icon.

## peekm vs. The World

| Capability | Glow | grip | VS Code Preview | peekm |
|------------|------|------|-----------------|-------|
| **AI file tracking** | — | — | — | Smart folders + timeline |
| **Unreviewed file detection** | — | — | — | Built-in |
| **Session persistence** | — | — | — | Survives restarts |
| **Toast notifications** | — | — | — | SSE-based, click to navigate |
| **Live reload** | — | Manual | Yes | SSE with event replay |
| **Directory browser** | TUI list | — | File explorer | Web UI tree + smart folders |
| **Startup** | Fast | ~2s | Editor launch | < 100ms |
| **Dependencies** | Single binary | Python | VS Code | Single binary |
| **Offline** | Yes | No (GitHub API) | Yes | Yes |

## Features

### Navigation
- **VS Code-style sidebar** — 280px tree view, collapsible folders, toggle with Cmd/Ctrl+B
- **Smart defaults** — auto-opens README.md or most recent file
- **Fuzzy file search** — Cmd/Ctrl+P to find files

### Live Workflow
- **Auto-reload on save** — see changes instantly via Server-Sent Events
- **Theme switching** — Light/Dark/Auto with persistence
- **Live editing** — edit markdown files directly in browser
- **HTML export** — download self-contained HTML for sharing

### Production-Ready
- Single binary, cross-platform (macOS, Linux, Windows), ~8MB memory footprint
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

**Quick Install (macOS/Linux)**

```bash
curl -L https://github.com/razvandimescu/peekm/releases/latest/download/peekm_$(uname -s)_$(uname -m).tar.gz | tar xz && sudo mv peekm /usr/local/bin/
```

**Go Install**

```bash
go install github.com/razvandimescu/peekm@latest
```

**Download Binary**

[Releases page](https://github.com/razvandimescu/peekm/releases) — macOS, Linux, Windows.

## Usage

```bash
peekm README.md        # View a file (opens with sidebar)
peekm .                # Browse a directory
peekm -port 8080 .     # Custom port
peekm -browser=false .  # Don't auto-open browser
```

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `-port` | `6419` | Port to serve on |
| `-browser` | `true` | Auto-open browser |
| `-version` | `false` | Show version |
| `-no-ai-tracking` | `false` | Disable AI session tracking |

## AI Session Tracking

The tracking endpoint is always active. Connect your AI assistant with one command:

```bash
peekm setup claude-code              # Configure integration
peekm setup claude-code --port 8080  # Custom port
peekm setup claude-code --remove     # Remove integration
```

The setup is idempotent (safe to run multiple times) and non-destructive (preserves your existing Claude Code settings).

## Ignoring Directories

peekm automatically excludes `.*`, `node_modules`, `vendor`, `dist`, `venv`, and similar. Add custom exclusions with `.peekmignore`:

```
# .peekmignore
target
_site
*.tmp
```

```bash
peekm --show-ignored    # See all exclusions
```

## How It Works

1. **Parse** — Converts markdown to HTML using [goldmark](https://github.com/yuin/goldmark)
2. **Serve** — Starts a local HTTP server with graceful shutdown
3. **Watch** — Monitors file changes using [fsnotify](https://github.com/fsnotify/fsnotify)
4. **Reload** — Sends live updates via Server-Sent Events with event replay
5. **Track** — Receives AI session metadata and correlates with file changes

## Get Started

```bash
npx @peekm/peekm .
```

Using Claude Code? One more command:

```bash
peekm setup claude-code
```

[Open an issue](https://github.com/razvandimescu/peekm/issues) if something breaks. [Star the repo](https://github.com/razvandimescu/peekm) if it doesn't.

## Development

- Go 1.21 or higher
- `go build -o peekm && go test -race ./...`

## Contributing

Contributions welcome — submit a Pull Request.

## License

MIT — see [LICENSE](LICENSE).

## Acknowledgments

- [goldmark](https://github.com/yuin/goldmark) — Markdown parser
- [fsnotify](https://github.com/fsnotify/fsnotify) — File watching
- [chroma](https://github.com/alecthomas/chroma) — Syntax highlighting

## Related Projects

- [glow](https://github.com/charmbracelet/glow) — Terminal markdown renderer
- [grip](https://github.com/joeyespo/grip) — GitHub-flavored markdown preview
- [VS Code Markdown Preview](https://code.visualstudio.com/docs/languages/markdown) — Built-in editor preview
