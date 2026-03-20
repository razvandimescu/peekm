# peekm

> See what AI coding agents change across all your projects.

AI coding agents modify dozens of files per session — but you only see the final diff. peekm watches all your projects in real time and shows you exactly which files changed, when, and what the agent was doing. Single binary, local web UI, all data stays on your machine.

## AI Session Tracking

Works with Claude Code today (connect once with `peekm setup claude-code`):

- **Timeline** — chronological view of every AI file modification, grouped by day
- **Smart folders** — "Recent AI Edits" surfaces files touched by AI in the last 24h
- **Toast notifications** — instant alerts when AI modifies a file
- **Session info panel** — per-file session ID, tool, permission mode, timestamp
- **Transcript viewer** — read the full AI conversation for any session
- **Persistent history** — events survive restarts

## Also a Markdown Viewer

- VS Code-style sidebar with tree view and fuzzy search
- Live reload via Server-Sent Events
- Light/Dark/Auto themes
- In-browser editing with auto-save
- GitHub-Flavored Markdown with syntax highlighting

## Installation

```bash
npm install -g @peekm/peekm
```

Or use with `npx` (no installation required):

```bash
npx @peekm/peekm .
```

## Usage

```bash
peekm .                 # browse a directory
peekm README.md         # view a single file
peekm -port 8080 .      # custom port
peekm -browser=false .  # don't auto-open browser
peekm setup claude-code # connect Claude Code (one time)
```

## Platform Support

This package automatically downloads the correct binary for your platform:

- macOS (ARM64, x64)
- Linux (ARM64, x64)
- Windows (x64)

## License

Apache 2.0

## More Information

Visit [github.com/razvandimescu/peekm](https://github.com/razvandimescu/peekm) for full documentation.
