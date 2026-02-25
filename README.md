# peekm

[![Go Report Card](https://goreportcard.com/badge/github.com/razvandimescu/peekm?v=2)](https://goreportcard.com/report/github.com/razvandimescu/peekm)
[![GitHub Release](https://img.shields.io/github/v/release/razvandimescu/peekm)](https://github.com/razvandimescu/peekm/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A markdown viewer that tracks what AI coding agents change in your project.

![peekm demo](assets/hero-demo.gif)

peekm watches your project and shows you exactly which files AI coding agents changed, when they changed them, and what the agent was thinking. It's also a fast markdown viewer with live reload, GitHub styling, and dark/light themes — but the AI tracking is why it exists.

**All data stays local. Nothing leaves your machine.**

```bash
npx @peekm/peekm .                  # start viewing
peekm setup claude-code              # connect Claude Code (one time)
```

Or `brew install razvandimescu/tap/peekm` for a permanent install.

## AI Tracking

When connected, peekm tracks every file modification made by AI agents:

- **Smart folders** in the sidebar — "Recent AI Edits" surfaces files touched by AI in the last 24 hours
- **Timeline** (`/timeline`) — chronological view of all AI file modifications, grouped by day, color-coded by operation type
- **Toast notifications** — appear the instant AI modifies a file, click to navigate
- **Session info panel** — per-file dropdown showing session ID, tool, permission mode, timestamp
- **Transcript viewer** — read the full AI conversation for any session
- **Persistent history** — events survive restarts (`~/.peekm/events.jsonl`)

## Comparison

| | Glow | grip | VS Code Preview | Ohai | peekm |
|---|------|------|-----------------|------|-------|
| **AI tracking** | — | — | — | — | Smart folders, timeline, toasts |
| **Session history** | — | — | — | — | Persistent timeline + transcripts |
| **Live reload** | — | Manual | Yes | Yes | SSE with event replay |
| **Directory browser** | TUI list | — | File explorer | — | Web tree + smart folders |
| **Cross-platform** | Yes | Yes | Yes | macOS only | Yes |
| **Price** | Free | Free | Free (with VS Code) | $3.99 | Free |
| **Startup** | Fast | ~2s | Editor launch | Fast | < 100ms |
| **Dependencies** | Single binary | Python | VS Code | Mac App Store | Single binary |

## What It Does

**AI session tracking** (with Claude Code):
- **Smart folders** — "Recent AI Edits" surfaces files touched by AI in the last 24h
- **Timeline** — chronological view of all AI modifications, color-coded by operation
- **Toast notifications** — instant alerts when AI modifies a file, click to navigate
- **Session panel** — per-file dropdown with session ID, tool, permission mode, timestamp
- **Transcript viewer** — read the full AI conversation for any session
- **Persistent history** — events survive restarts (`~/.peekm/events.jsonl`)

**Markdown viewer**:
- VS Code-style sidebar with tree view and fuzzy search (Cmd/Ctrl+P)
- Live reload via Server-Sent Events with event replay
- Light/Dark/Auto themes, persisted
- In-browser editing with auto-save
- HTML export
- Single binary, cross-platform, < 100ms startup

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
| `peekm setup claude-code --port 8080` | Custom port |
| `peekm setup claude-code --remove` | Remove hooks |

Setup is idempotent and non-destructive. Creates `~/.claude/peekm-hook.sh` and adds PostToolUse hooks to `~/.claude/settings.json`.

## Ignoring Directories

peekm excludes `.*`, `node_modules`, `vendor`, `dist`, `venv` by default. Custom exclusions go in `.peekmignore`:

```
target
_site
*.tmp
```

`peekm --show-ignored` to see all active exclusions.

<details>
<summary><strong>How It Works</strong></summary>

1. **Parse** — markdown to HTML via [goldmark](https://github.com/yuin/goldmark)
2. **Serve** — local HTTP server with graceful shutdown
3. **Watch** — file changes via [fsnotify](https://github.com/fsnotify/fsnotify)
4. **Reload** — SSE with event replay
5. **Track** — correlates AI session metadata with file changes

</details>

## Development

Go 1.23+. `go build -o peekm && go test -race ./...`

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
- [Ohai](https://ohai.dev) — macOS markdown viewer for AI workflows
