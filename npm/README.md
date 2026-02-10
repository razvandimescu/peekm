# peekm

> Zen-like markdown viewer with live reload

A lightweight, fast markdown viewer that displays your markdown files with a clean, VS Code-style layout. Perfect for reading documentation, writing content, or previewing markdown in real-time.

## Features

- **Directory Browser** - Browse and navigate all markdown files in a directory with a visual tree
- **VS Code Layout** - Fixed sidebar with independent scrolling
- **Live Reload** - Auto-refresh when file changes (SSE-based)
- **GitHub-Flavored Markdown** - Full GFM support with syntax highlighting
- **Fast & Lightweight** - Single binary, zero dependencies
- **Cross-Platform** - Works on macOS, Linux, and Windows

## Installation

```bash
npm install -g peekm
```

Or use with `npx` (no installation required):

```bash
npx peekm README.md
```

## Usage

```bash
# Browse all markdown files in current directory
peekm

# Browse a specific directory
peekm ../docs

# View a single markdown file
peekm README.md

# Custom port
peekm -port 8080

# Don't auto-open browser
peekm -browser=false

# Show version
peekm -version
```

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `-port` | `6419` | Port to serve on |
| `-browser` | `true` | Automatically open browser |
| `-version` | `false` | Show version information |

## How It Works

1. **Parse** - Converts markdown to HTML
2. **Serve** - Starts a local HTTP server
3. **Watch** - Monitors file changes
4. **Reload** - Sends live updates via Server-Sent Events

## Platform Support

This package automatically downloads the correct binary for your platform:

- macOS (ARM64, x64)
- Linux (ARM64, x64)
- Windows (x64)

## License

MIT

## More Information

Visit [github.com/razvandimescu/peekm](https://github.com/razvandimescu/peekm) for full documentation.
