# peek

> Zen-like markdown viewer with centered layout and live reload

A lightweight, fast markdown viewer that displays your markdown files with a clean, centered layout. Perfect for reading documentation, writing content, or previewing markdown in real-time.

## Features

- 📁 **Directory Browser** - Browse and navigate all markdown files in a directory with a visual tree
- ✨ **Centered Layout** - Clean, readable 900px max-width design
- 🔄 **Live Reload** - Auto-refresh when file changes
- 🎨 **GitHub-Flavored Markdown** - Full GFM support with syntax highlighting
- ⚡ **Fast & Lightweight** - Single binary, zero dependencies
- 🖥️ **Cross-Platform** - Works on macOS, Linux, and Windows

## Installation

```bash
npm install -g peek
```

Or use with `npx` (no installation required):

```bash
npx peek README.md
```

## Usage

```bash
# Browse all markdown files in current directory
peek

# Browse a specific directory
peek ../docs

# View a single markdown file
peek README.md

# Custom port
peek -port 8080 document.md

# Don't auto-open browser
peek -browser=false notes.md

# Show version
peek -version
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

Visit [github.com/rd/peek](https://github.com/rd/peek) for full documentation.
