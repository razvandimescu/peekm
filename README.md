# peekm

> Beautiful markdown reading that just works — no config, no friction, pure focus

**Built for AI-Assisted Development** — Get instant notifications when Claude Code, Cursor, or Copilot create markdown files. Click to view. Watch live as AI writes. No hunting through file trees, no manual refresh, no breaking your flow.

**For everyone else:** The fastest way to preview markdown with live reload and beautiful GitHub styling.

```bash
peekm README.md    # One command. That's it.
```

**Perfect for:**

- 🤖 **AI coding workflows** — Instant toast notifications when AI generates docs
- 📖 **Documentation reading** — Centered layout, distraction-free
- ✍️ **Writing & previewing** — Live reload as you save
- 🔍 **Directory browsing** — Visual tree, collapsible folders
- 📚 **PR reviews** — Beautiful rendering for documentation changes

[Install in 10 seconds](#quick-start) • [See comparison](#peekm-vs-the-world) • [Why peekm?](#why-peekm-over-alternatives)

## Quick Start

```bash
# macOS/Linux — Install in 10 seconds
curl -L https://github.com/razvandimescu/peekm/releases/latest/download/peekm_$(uname -s)_$(uname -m).tar.gz | tar xz && sudo mv peekm /usr/local/bin/

# Try it immediately
peekm README.md
```

**That's it.** You're reading beautiful markdown.

> "Finally, a markdown viewer that understands modern AI workflows. Game changer for Claude Code users."

<!-- Add GitHub stars badge when available -->
<!-- [![GitHub stars](https://img.shields.io/github/stars/razvandimescu/peekm?style=social)](https://github.com/razvandimescu/peekm) -->

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

### 🎯 **Persistent Navigation with Optional Zen Mode**

Navigate your documentation with a familiar sidebar, just like VS Code or GitHub. Need full-width focus? Hit `Cmd/Ctrl+B` to hide the sidebar — zen mode on demand.

- **Sidebar visible by default** — navigate projects without hunting
- **280px tree view** — fits perfectly in left margin of centered content
- **Cmd/Ctrl+B toggles zen mode** — hide sidebar for distraction-free reading
- **Breadcrumb trail** — stay oriented even when sidebar is hidden
- **Current file highlighting** — see your location in the project
- **Smart state** — remembers your sidebar preference

### ⚡ **Zero Friction**
- **Single binary** — download and run, nothing to install
- **No configuration** — works perfectly out of the box
- **Instant startup** — under 100ms to first render

### 🔄 **Live Workflow**
- **Auto-reload on save** — see changes instantly via Server-Sent Events
- **Directory browser** — navigate projects without leaving the page
  - 🌲 Collapsible directories (auto-collapsed at depth 1+)
  - 📄 Pagination with "Load More" button (shows 5 items initially)
  - 🧭 Console-like navigation (λ button) - navigate between directories
- **Theme switching** — comfortable reading any time of day (Light/Dark/Auto)

### 🔒 **Production-Ready**
- **Secure** — symlink validation, path traversal protection, $HOME boundary enforcement
- **Fast** — ~8MB memory footprint, embedded resources
- **Cross-platform** — works on macOS, Linux, and Windows
- **GitHub-Flavored Markdown** — full GFM support with syntax highlighting

## Installation

**Option 1: Quick Install (10 seconds)**

```bash
# macOS/Linux
curl -L https://github.com/razvandimescu/peekm/releases/latest/download/peekm_$(uname -s)_$(uname -m).tar.gz | tar xz && sudo mv peekm /usr/local/bin/
```

**Option 2: Go Install**

```bash
go install github.com/razvandimescu/peekm@latest
```

**Option 3: Download Binary**

Download from the [releases page](https://github.com/razvandimescu/peekm/releases) for your platform (macOS, Linux, Windows).

**Option 4: From Source**

```bash
git clone https://github.com/razvandimescu/peekm.git
cd peekm
go build
```

*Homebrew and npm packages coming Q1 2025*

## Usage

### Single File Mode

View a specific markdown file with live reload:

```bash
# View a markdown file
peekm README.md

# Custom port
peekm -port 8080 document.md

# Don't auto-open browser
peekm -browser=false notes.md
```

### Directory Browser Mode

Browse all markdown files in a directory with a visual tree:

```bash
# Browse current directory
peekm

# Browse a specific directory
peekm ../docs

# Browse with custom port
peekm -port 8080 ~/Documents/notes
```

The browser mode shows:
- 📂 Interactive directory tree with all `.md` files
- 🌲 Collapsible folders - click ▶/▼ to expand/collapse directories
- 📄 Pagination - loads 5 items at a time with "Load More" button
- 🔗 Clickable file links for easy navigation
- 📊 File sizes displayed for each markdown file
- 🔍 Smart scanning (skips hidden dirs, node_modules, vendor, dist, venv)
- 🎨 Theme toggle (light/dark/auto) in top-right corner
- 🧭 Directory navigation (λ button) in top-left corner - navigate to any directory within $HOME

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `-port` | `6419` | Port to serve on |
| `-browser` | `true` | Automatically open browser |
| `-version` | `false` | Show version information |
| `-show-ignored` | `false` | Show all excluded directories and exit |

## Ignoring Directories (Optional)

peekm automatically excludes common directories:
- `.*` (hidden directories like `.git`, `.vscode`)
- `node_modules` (Node.js dependencies)
- `vendor` (Go dependencies)
- `dist` (build output)
- `venv`, `env`, `virtualenv` (Python virtual environments)

To add custom exclusions, create `.peekmignore` in your project root:

```
# .peekmignore - Project-specific exclusions
# Commit this file to share with your team

# Build artifacts
target
_site
out

# Temporary files
*.tmp
*.cache
```

**Syntax:**
- One pattern per line
- Simple paths: `build`, `output`, `temp`
- Wildcards: `*.tmp`, `test_*` (matches any characters)
- Comments: Lines starting with `#`
- Not supported: Path separators (`/`, `\`), globstar (`**`)

Your custom patterns **add to** the defaults (they don't replace them).

**See what's being excluded:**
```bash
peekm --show-ignored
# Shows hardcoded exclusions and .peekmignore patterns if present

peekm --show-ignored ~/projects/myapp
# Check exclusions for a specific directory
```

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

peekm follows Go best practices with production-ready, hardened architecture:

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

When you run `peekm README.md`, your markdown is displayed in a clean, centered layout with:

- GitHub-style formatting
- Syntax-highlighted code blocks
- Responsive design (mobile-friendly)
- Automatic table of contents via heading IDs

## peekm vs. The World

**The only markdown viewer built for modern AI-assisted development workflows.** Live reload, browser rendering, directory navigation, and instant notifications when AI agents create new files.

| What You Need | Glow | grip | VS Code | peekm |
|----------------|------|------|---------|------|
| **Best For** | Terminal purists | GitHub preview | VS Code users | AI workflows + modern dev |
| **Live reload on file change** | ❌ Static | ❌ Manual refresh | ✅ | ✅ SSE-based |
| **AI agent notifications** | ❌ | ❌ | ❌ | ✅ Toast popups |
| **Persistent sidebar + zen mode** | ❌ | ❌ | ❌ Persistent only | ✅ Visible by default, Cmd/Ctrl+B to hide |
| **Comfortable reading layout** | ❌ Terminal only | ❌ Full-width | ❌ Splits editor | ✅ Centered 900px |
| **Interactive directory browser** | ✅ TUI list | ❌ Single file | ❌ File explorer | ✅ Web UI tree |
| **Quick preview without editor** | ✅ | ✅ | ❌ Launches editor | ✅ |
| **Works offline** | ✅ | ❌ GitHub API | ✅ | ✅ |
| **Zero dependencies** | ✅ Single binary | ❌ Python runtime | ❌ Needs VS Code | ✅ Single binary |
| **Startup time** | Fast | ~2s | Editor launch | < 100ms |

**Choose peekm when you:**
- Work with **AI coding assistants** (Claude Code, Cursor, Copilot) and want instant notifications for new markdown files
- Need **live reload** as you write — no manual refresh, no breaking flow
- Want **browser-quality rendering** with centered layout for comfortable reading
- Need to **browse documentation directories** with a visual tree interface
- Want **one command** that just works — no Python, no VS Code, no configuration

### Philosophy

- **Zero Friction** — One command, instant preview, smart defaults
- **Speed** — Fast startup (< 100ms), instant reload
- **Focus** — Centered layout for better readability
- **Zero Dependencies** — Self-contained binary, no Python/Node/external files required
- **Production Quality** — Proper error handling, security hardening, resource management

## Development

### Requirements

- Go 1.21 or higher

### Building

```bash
# Standard build
go build -o peekm

# Build with version info
go build -ldflags "-X main.version=1.0.0 -X main.commit=$(git rev-parse HEAD) -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o peekm
```

### Project Structure

```
peekm/
├── main.go                    # Single-file Go implementation with embedded resources
│   ├── Types                  # Named types with composition (baseTemplateData, etc.)
│   ├── Helpers                # validateAndResolvePath, withRecovery, route registration
│   ├── Factories              # newMarkdownRenderer, newBaseTemplateData
│   └── Core Functions         # serveBrowser, serveFile, collectMarkdownFiles, etc.
└── theme/                     # Embedded resources (loaded at build time)
    ├── github-markdown.css    # Official GitHub markdown CSS
    ├── theme-overrides.css    # Theme switching CSS
    ├── theme-manager.js       # Shared theme management logic
    ├── navigation.js          # SPA navigation and download functionality
    ├── editor.js              # Markdown editing functionality
    └── file-browser.html      # Unified template (browser + file views)
```

### Testing

```bash
# Test single file mode
./peekm README.md

# Test directory browser mode
./peekm .

# Test with custom port
./peekm -port 8080 README.md

# Test graceful shutdown
./peekm README.md
# Press Ctrl+C - should see "Shutting down gracefully..."
```

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request. For major changes, please open an issue first to discuss what you would like to change.

### Development Guidelines

- **Focused scope** - Resist feature creep, every feature must solve a real user problem
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

- [glow](https://github.com/charmbracelet/glow) - Terminal markdown renderer (21k+ stars) — Choose this if you prefer terminal TUI over browser UI
- [grip](https://github.com/joeyespo/grip) - GitHub-flavored markdown preview (6.7k stars) — Python-based, requires GitHub API
- [VS Code Markdown Preview](https://code.visualstudio.com/docs/languages/markdown) - Built-in editor preview — Choose this if you're already in VS Code

---

**Made with ❤️ for a better markdown reading experience**
