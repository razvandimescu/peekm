# peekm

> Beautiful markdown reading that just works — no config, no friction, pure focus

**The Problem:** You're constantly switching between your editor and browser to read markdown documentation. Browser tabs pile up. GitHub's narrow layout wastes your widescreen monitor. You lose context switching between files. When AI agents like Claude Code generate markdown files, you have to hunt for them in your file tree and manually open a preview. Setting up a markdown preview server feels like overkill.

**The Solution:** `peekm` — a single command that transforms any markdown file or directory into a beautiful, centered reading experience with live reload. Perfect for:

- 🤖 **AI-assisted development** — Instant notifications when AI generates new markdown files
- 📖 **Reading project documentation** during development
- ✍️ **Writing and previewing** your own markdown content
- 🔍 **Exploring documentation trees** in large projects
- 📚 **Reviewing PRs and changes** with instant preview
- 🎯 **Focused reading sessions** without distractions

```bash
# Just works — no setup required
peekm README.md           # View a single file
peekm .                   # Browse entire documentation tree
```

## Quick Start

```bash
# macOS/Linux — Install in 10 seconds
curl -L https://github.com/rd/peekm/releases/latest/download/peekm_$(uname -s)_$(uname -m).tar.gz | tar xz && sudo mv peekm /usr/local/bin/

# Try it immediately
peekm README.md
```

**That's it.** You're reading beautiful markdown.

## Why peekm Over Alternatives?

**VS Code Preview?** Splits your editor, breaks your flow, tied to VS Code
**GitHub/GitLab?** Requires pushing changes, narrow layout, needs internet
**grip?** No directory browsing, no themes, requires Python runtime
**Browser + file://?** No hot reload, no syntax highlighting, ugly rendering

**peekm gives you:**
- ✨ **Instant preview** with one command
- 🎯 **Centered, distraction-free layout** (not cramped like GitHub)
- 📁 **Navigate entire documentation trees** without opening new tabs
- 🌗 **Dark/light themes** that follow your system
- ⚡ **Zero dependencies** — just download and run

## Features That Matter

### 🎯 **Focus Mode**
- **Centered 900px layout** — optimized for reading, not scanning
- **Clean GitHub styling** — familiar and beautiful
- **Distraction-free** — no ads, popups, or navigation clutter

### ⚡ **Zero Friction**
- **Single binary** — download and run, nothing to install
- **No configuration** — works perfectly out of the box
- **Instant startup** — under 100ms to first render

### 🔄 **Live Workflow**
- **Auto-reload on save** — see changes instantly via Server-Sent Events
- **Directory browser** — navigate projects without leaving the page
  - 🌲 Collapsible directories (auto-collapsed at depth 2+)
  - 📄 Pagination with "Load More" button (shows 5 items initially)
  - 🧭 Console-like navigation (λ button) - navigate between directories
- **Theme switching** — comfortable reading any time of day (Light/Dark/Auto)

### 🔒 **Production-Ready**
- **Secure** — symlink validation, path traversal protection, $HOME boundary enforcement
- **Fast** — ~8MB memory footprint, embedded resources
- **Cross-platform** — works on macOS, Linux, and Windows
- **GitHub-Flavored Markdown** — full GFM support with syntax highlighting

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

## When You Need peekm

### Scenario 1: AI-Assisted Development (Claude Code, Cursor, GitHub Copilot)
```bash
# Start peekm in your project directory
peekm .

# Ask your AI agent: "Create a detailed API documentation in docs/api.md"
# → peekm instantly shows a toast notification
# → Click the notification to view the newly created file
# → See live updates as the AI continues writing
```
**Stop hunting for AI-generated files.** When Claude Code or other AI assistants create markdown files, peekm immediately notifies you with a clickable toast notification in the top-right corner. Click it to instantly view the new file. Watch live as the AI writes — no manual refresh, no searching through your file tree, no breaking your flow.

### Scenario 2: Onboarding to a New Project
```bash
git clone github.com/awesome/project
cd project
peekm docs/    # Instantly browse all documentation with a visual tree
```
**Navigate complex documentation structures without getting lost.** Collapsible folders keep you oriented. See file sizes to prioritize what to read. Jump between files without opening new tabs.

### Scenario 3: Writing Documentation
```bash
peekm README.md    # Edit in your favorite editor
```
**See your changes instantly.** No manual refresh. No build step. Write in your editor, preview in your browser. The way it should be.

### Scenario 4: Code Review
```bash
# Reviewing a PR with documentation changes
git checkout feature-branch
peekm CHANGELOG.md
```
**Beautiful rendering makes reviewing documentation changes a pleasure.** Compare branches by switching between them — peekm auto-reloads. Spot formatting issues before they hit main.

### Scenario 5: Learning a New Library
```bash
peekm ~/dev/library-examples/
```
**Browse through example markdown files quickly.** The tree view shows you what's available at a glance. Collapsible directories let you focus on one section at a time. Dark mode for late-night learning sessions.

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

## peekm vs. The World

| Your Pain Point | grip | VS Code | GitHub | peekm |
|----------------|------|----------|---------|------|
| **Want it working in 5 seconds** | ❌ Python setup | ❌ Extension install | ❌ Git push required | ✅ Just works |
| **Browse documentation trees** | ❌ Single file only | ❌ File explorer | ✅ But slow | ✅ Fast & local |
| **Comfortable reading layout** | ❌ Full-width | ❌ Splits editor | ❌ Narrow column | ✅ Centered 900px |
| **Works offline** | ✅ | ✅ | ❌ | ✅ |
| **Dark mode** | ❌ | ✅ | ✅ | ✅ Auto-switching |
| **Zero dependencies** | ❌ Needs Python | ❌ Needs VS Code | ❌ Needs internet | ✅ Single binary |
| **Live reload** | ✅ | ✅ | ❌ Manual push | ✅ SSE-based |
| **Startup time** | ~2s | ~1s | N/A | < 100ms |

### Philosophy

- **Simplicity** — One command, one file, instant preview
- **Speed** — Fast startup (< 100ms), instant reload
- **Focus** — Centered layout for better readability
- **Minimalism** — No bloat, no configuration files
- **Quality** — Production-ready code with proper resource management

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
