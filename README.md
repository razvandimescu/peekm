# peekm

> Zen-like markdown viewer with centered layout and live reload

A lightweight, fast markdown viewer that displays your markdown files with a clean, centered layout. Browse directories, view individual files, and enjoy live reload. Perfect for reading documentation, writing content, or previewing markdown in real-time.

## Features

- 📁 **Directory Browser** - Browse and navigate all markdown files with an interactive tree
  - 🌲 Collapsible directories (auto-collapsed at depth 2+)
  - 📄 Pagination with "Load More" button (shows 5 items initially)
  - 📊 File sizes displayed for each markdown file
  - 🧭 Console-like navigation (λ button) - navigate between directories within $HOME
- 🎨 **Theme Support** - Light, dark, and auto (system) themes
  - 🌙 Dark mode with authentic GitHub styling
  - ☀️ Light mode with clean, bright design
  - 💻 Auto mode follows system preferences
  - 💾 Theme preference saved in browser
- ✨ **Centered Layout** - Clean, readable 900px max-width design
- 🔄 **Live Reload** - Auto-refresh when file changes via Server-Sent Events
- 🖊️ **GitHub-Flavored Markdown** - Full GFM support with syntax highlighting
- ⚡ **Fast & Lightweight** - Single binary with embedded resources (zero runtime dependencies)
- 🖥️ **Cross-Platform** - Works on macOS, Linux, and Windows
- 🔒 **Secure** - Symlink validation, whitelist-based file access, path traversal protection, $HOME boundary enforcement

## Installation

### Pre-built Binaries (Recommended)

Download pre-compiled binaries from the [releases page](https://github.com/rd/peekm/releases).

**macOS/Linux** (quick install):
```bash
# Download and install latest version
curl -L https://github.com/rd/peek/releases/latest/download/peek_$(uname -s)_$(uname -m).tar.gz | tar xz
sudo mv peek /usr/local/bin/
```

**Windows**: Download the `.zip` file from [releases](https://github.com/rd/peek/releases), extract, and add to PATH.

### Homebrew (macOS/Linux)

*Coming soon*

```bash
brew install rd/tap/peek
```

### npm

*Coming soon*

```bash
npm install -g peek
```

### Using Go

```bash
go install github.com/rd/peek@latest
```

### From Source

```bash
git clone https://github.com/rd/peek.git
cd peek
go build
```

## Usage

### Single File Mode

View a specific markdown file with live reload:

```bash
# View a markdown file
peek README.md

# Custom port
peek -port 8080 document.md

# Don't auto-open browser
peek -browser=false notes.md
```

### Directory Browser Mode

Browse all markdown files in a directory with a visual tree:

```bash
# Browse current directory
peek

# Browse a specific directory
peek ../docs

# Browse with custom port
peek -port 8080 ~/Documents/notes
```

The browser mode shows:
- 📂 Interactive directory tree with all `.md` files
- 🌲 Collapsible folders - click ▶/▼ to expand/collapse directories
- 📄 Pagination - loads 5 items at a time with "Load More" button
- 🔗 Clickable file links for easy navigation
- 📊 File sizes displayed for each markdown file
- 🔍 Smart scanning (skips hidden dirs, node_modules, vendor, dist)
- 🎨 Theme toggle (light/dark/auto) in top-right corner
- 🧭 Directory navigation (λ button) in top-left corner - navigate to any directory within $HOME

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `-port` | `6419` | Port to serve on |
| `-browser` | `true` | Automatically open browser |
| `-version` | `false` | Show version information |

## How It Works

1. **Parse** - Converts markdown to HTML using [goldmark](https://github.com/yuin/goldmark)
2. **Serve** - Starts a local HTTP server with graceful shutdown
3. **Watch** - Monitors file changes using [fsnotify](https://github.com/fsnotify/fsnotify) with proper resource management
4. **Reload** - Sends live updates via Server-Sent Events (SSE)
5. **Render** - Applies GitHub styling with embedded CSS (zero runtime dependencies)

## Architecture

peek follows Go best practices with production-ready, hardened architecture:

- **Resource Management** - Proper file watcher lifecycle with context-based cancellation
- **Graceful Shutdown** - Clean resource cleanup on SIGINT/SIGTERM
- **Error Handling** - Comprehensive error handling with proper HTTP status codes
- **Panic Recovery** - Middleware prevents crashes, logs stack traces
- **Security** - Layered defense: symlink validation, path traversal protection, whitelist, $HOME boundary enforcement
- **Performance** - Embedded resources loaded once at startup for fast serving
- **Concurrency** - Thread-safe state management with RWMutex protection
- **Code Quality** - Named types with composition, DRY helpers, centralized route registration
- **HTTP Timeouts** - Read (15s), Write (15s), and Idle (60s) timeouts configured

## Screenshots

When you run `peek README.md`, your markdown is displayed in a clean, centered layout with:

- GitHub-style formatting
- Syntax-highlighted code blocks
- Responsive design (mobile-friendly)
- Automatic table of contents via heading IDs

## Comparison with Grip

| Feature | Grip | peek |
|---------|------|-------|
| Markdown rendering | ✅ | ✅ |
| GitHub styling | ✅ | ✅ |
| **Centered layout** | ❌ | ✅ |
| **Theme switching** | ❌ | ✅ (Light/Dark/Auto) |
| **Directory browser** | ❌ | ✅ (Interactive tree) |
| Live reload | ✅ | ✅ (SSE-based) |
| Syntax highlighting | ✅ | ✅ |
| Dependencies | Python runtime | None (static binary) |
| Code size | ~5000+ lines | ~1000 lines (well-structured) |
| Startup time | ~2s | < 100ms |
| Memory footprint | ~50MB | ~8MB |

## Why peek?

**peek** was created to solve a simple problem: reading markdown files should be pleasant. While tools like `grip` exist, they lack a centered, distraction-free layout and modern features. peek provides a zen-like reading experience with:

- **Directory browsing** - Navigate entire documentation trees with ease
- **Theme support** - Read comfortably in any lighting condition
- **Smart UX** - Collapsible directories and pagination for large projects
- **Zero setup** - Download and run, no dependencies or configuration

### Philosophy

- **Simplicity** - One command, one file, instant preview
- **Speed** - Fast startup (< 100ms), instant reload
- **Focus** - Centered layout for better readability
- **Minimalism** - No bloat, no configuration files
- **Quality** - Production-ready code with proper resource management

## Development

### Requirements

- Go 1.21 or higher

### Building

```bash
# Standard build
go build -o peek

# Build with version info
go build -ldflags "-X main.version=1.0.0 -X main.commit=$(git rev-parse HEAD) -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o peek
```

### Project Structure

```
peek/
├── main.go                    # Core application (~1000 lines)
│   ├── Types                  # Named types with composition (baseTemplateData, etc.)
│   ├── Helpers                # validateAndResolvePath, withRecovery, route registration
│   ├── Factories              # newMarkdownRenderer, newBaseTemplateData
│   └── Core Functions         # serveBrowser, serveFile, collectMarkdownFiles, etc.
└── theme/                     # Embedded resources (loaded at build time)
    ├── github-markdown.css    # Official GitHub markdown CSS
    ├── theme-overrides.css    # Theme switching CSS
    ├── theme-manager.js       # Shared theme management logic
    ├── single-file.html       # Single file viewer template
    └── file-browser.html      # Directory browser template
```

### Testing

```bash
# Test single file mode
./peek README.md

# Test directory browser mode
./peek .

# Test with custom port
./peek -port 8080 README.md

# Test graceful shutdown
./peek README.md
# Press Ctrl+C - should see "Shutting down gracefully..."
```

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request. For major changes, please open an issue first to discuss what you would like to change.

### Development Guidelines

- **Keep it simple** - Resist feature creep, maintain zen philosophy
- **Code quality** - Follow Go best practices (proper error handling, resource cleanup, named types)
- **DRY principle** - Extract common patterns to helpers/factories, avoid duplication
- **Performance** - Minimize memory allocations, use efficient algorithms
- **Security** - Validate all user inputs, check symlinks, prevent path traversal
- **Documentation** - Update README and `.claude/CLAUDE.md` for new features
- **Architecture** - Maintain resource management patterns (context cancellation, graceful shutdown)
- **Architecture review** - Use `solution-architect` agent for significant changes
- **Testing** - Test both single-file and directory browser modes

## License

MIT License - see [LICENSE](LICENSE) file for details

## Acknowledgments

- [goldmark](https://github.com/yuin/goldmark) - Excellent markdown parser
- [fsnotify](https://github.com/fsnotify/fsnotify) - Cross-platform file watching
- [chroma](https://github.com/alecthomas/chroma) - Syntax highlighting

## Related Projects

- [grip](https://github.com/joeyespo/grip) - GitHub-flavored markdown preview
- [marked](https://marked.js.org/) - JavaScript markdown parser
- [glow](https://github.com/charmbracelet/glow) - Terminal markdown renderer

---

**Made with ❤️ for a better markdown reading experience**
