#!/bin/bash
# record-demo.sh — Records a hero GIF of peekm's AI session tracking workflow.
#
# Prerequisites: ffmpeg, peekm (go build -o peekm)
# Usage: ./scripts/record-demo.sh [output.gif]
#
# The script:
#   1. Creates a demo project with sample markdown
#   2. Starts peekm on a non-conflicting port
#   3. Opens Chrome in --app mode (no tab/address bar)
#   4. Records the browser window via ffmpeg (screen capture + crop)
#   5. Orchestrates demo actions via SPA navigation: file edits,
#      Claude Code hook simulation, timeline, transcript, memory browser
#   6. Converts recording to an optimized GIF

set -euo pipefail

# --------------- Configuration ---------------
OUTPUT="${1:-assets/hero-demo.gif}"
PORT=16419
RECORD_SECONDS=30
VIEWPORT_W=1600
VIEWPORT_H=900
FPS=12
GIF_WIDTH=800
MAX_GIF_SIZE_MB=5

# --------------- State ---------------
DEMO_DIR=""
TRANSCRIPT_DIR=""
PEEKM_PID=""
FFMPEG_PID=""
CHROME_PID=""
MOV_FILE=""
CHROME_DATA_DIR=""
CDP_HELPER=""
CDP_PORT=9223

# --------------- Helpers ---------------
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'
log()  { echo -e "${GREEN}[demo]${NC} $1"; }
warn() { echo -e "${YELLOW}[demo]${NC} $1"; }
err()  { echo -e "${RED}[demo]${NC} $1" >&2; }

cleanup() {
    log "Cleaning up..."
    [ -n "$PEEKM_PID" ]  && kill "$PEEKM_PID"  2>/dev/null || true
    [ -n "$FFMPEG_PID" ] && kill "$FFMPEG_PID" 2>/dev/null || true
    [ -n "$CHROME_PID" ] && kill "$CHROME_PID" 2>/dev/null && wait "$CHROME_PID" 2>/dev/null || true
    [ -n "$DEMO_DIR" ]   && rm -rf "$DEMO_DIR"
    [ -n "$TRANSCRIPT_DIR" ] && rm -rf "$TRANSCRIPT_DIR"
    for mdir in "${MEMORY_DIRS_TO_CLEAN[@]+"${MEMORY_DIRS_TO_CLEAN[@]}"}"; do
        [ -n "$mdir" ] && rm -rf "$mdir"
    done
    [ -n "$MOV_FILE" ] && [ -f "$MOV_FILE" ] && rm -f "$MOV_FILE"
    [ -n "$CDP_HELPER" ] && rm -f "$CDP_HELPER"
    [ -n "$CHROME_DATA_DIR" ] && sleep 0.5 && rm -rf "$CHROME_DATA_DIR"
    log "Done."
}
trap cleanup EXIT

# Create a Python3 CDP helper (Chrome DevTools Protocol over WebSocket).
# AppleScript can't see --user-data-dir Chrome instances, so we use CDP instead.
CDP_HELPER=$(mktemp /tmp/peekm-cdp-XXXXXX.py)
cat > "$CDP_HELPER" << 'PYTHON'
import json, socket, struct, os, sys, http.client, urllib.parse

def cdp_eval(port, js):
    conn = http.client.HTTPConnection('localhost', port, timeout=2)
    conn.request('GET', '/json')
    targets = json.loads(conn.getresponse().read())
    conn.close()
    # Find the actual page tab (skip background pages, service workers, etc.)
    page = next((t for t in targets if t.get('type') == 'page'), None)
    if not page:
        return
    ws_url = page.get('webSocketDebuggerUrl')
    if not ws_url:
        return
    parsed = urllib.parse.urlparse(ws_url)

    sock = socket.create_connection((parsed.hostname, parsed.port), timeout=5)
    key = 'dGhlIHNhbXBsZSBub25jZQ=='
    handshake = (
        f"GET {parsed.path} HTTP/1.1\r\n"
        f"Host: {parsed.hostname}:{parsed.port}\r\n"
        f"Upgrade: websocket\r\nConnection: Upgrade\r\n"
        f"Sec-WebSocket-Key: {key}\r\n"
        f"Sec-WebSocket-Version: 13\r\n\r\n"
    )
    sock.sendall(handshake.encode())
    sock.recv(4096)

    msg = json.dumps({"id": 1, "method": "Runtime.evaluate",
                       "params": {"expression": js}}).encode()
    mask = os.urandom(4)
    frame = bytearray([0x81])
    if len(msg) < 126:
        frame.append(0x80 | len(msg))
    elif len(msg) < 65536:
        frame.append(0x80 | 126)
        frame.extend(struct.pack('>H', len(msg)))
    else:
        frame.append(0x80 | 127)
        frame.extend(struct.pack('>Q', len(msg)))
    frame.extend(mask)
    frame.extend(bytes(b ^ mask[i % 4] for i, b in enumerate(msg)))
    sock.sendall(bytes(frame))
    sock.recv(4096)
    sock.close()

if __name__ == '__main__':
    try:
        cdp_eval(int(sys.argv[1]), sys.argv[2])
    except Exception:
        pass
PYTHON

# Run JavaScript in the Chrome --app window via CDP
run_js() {
    python3 "$CDP_HELPER" "$CDP_PORT" "$1" 2>/dev/null || true
}

# Show a floating caption overlay at the bottom of the page
show_caption() {
    run_js "
        var cap = document.getElementById('demo-caption');
        if (!cap) {
            cap = document.createElement('div');
            cap.id = 'demo-caption';
            cap.style.cssText = 'position:fixed;bottom:24px;left:50%;transform:translateX(-50%);background:rgba(0,0,0,0.75);color:#fff;padding:8px 20px;border-radius:8px;font-size:14px;font-weight:500;z-index:99999;pointer-events:none;letter-spacing:0.3px;transition:opacity 0.3s;';
            document.body.appendChild(cap);
        }
        cap.textContent = '$1';
        cap.style.opacity = '1';
    "
}

hide_caption() {
    run_js "
        var cap = document.getElementById('demo-caption');
        if (cap) cap.style.opacity = '0';
    "
}

# --------------- Dependency checks ---------------
for cmd in ffmpeg go; do
    if ! command -v "$cmd" &>/dev/null; then
        err "$cmd is required but not found"
        exit 1
    fi
done

# --------------- Build peekm if needed ---------------
PEEKM_BIN="./peekm"
if [ ! -f "$PEEKM_BIN" ] || [ main.go -nt "$PEEKM_BIN" ]; then
    log "Building peekm..."
    go build -o "$PEEKM_BIN"
fi

# --------------- Step 1: Create demo project ---------------
# Must be under $HOME — peekm enforces $HOME boundary for security
DEMO_DIR=$(mktemp -d "$HOME/.peekm-demo-XXXXXX")
log "Demo project at $DEMO_DIR"

mkdir -p "$DEMO_DIR/docs" "$DEMO_DIR/src" "$DEMO_DIR/tests" "$DEMO_DIR/.github/workflows"

cat > "$DEMO_DIR/README.md" << 'MD'
# Acme API

> A modern REST API for managing widgets

## Quick Start

```bash
npm install @acme/api
```

```javascript
import { createClient } from '@acme/api';

const client = createClient({ apiKey: 'sk-...' });
const widgets = await client.widgets.list();
```

## Features

- **Type-safe** — Full TypeScript support with generated types
- **Paginated** — Cursor-based pagination on all list endpoints
- **Webhooks** — Real-time event notifications

## Documentation

- [API Reference](docs/api.md) — Endpoints and parameters
- [Authentication](docs/auth.md) — API keys and OAuth
MD

cat > "$DEMO_DIR/docs/api.md" << 'MD'
# API Reference

## Widgets

### `GET /widgets`

List all widgets with optional filtering.

| Parameter | Type   | Description          |
|-----------|--------|----------------------|
| `limit`   | number | Max results (1-100)  |
| `cursor`  | string | Pagination cursor    |
| `status`  | string | Filter by status     |

### `POST /widgets`

Create a new widget.

```json
{
  "name": "My Widget",
  "type": "standard",
  "config": {}
}
```
MD

cat > "$DEMO_DIR/docs/auth.md" << 'MD'
# Authentication

## API Keys

Generate keys from the dashboard. Include in requests:

```bash
curl -H "Authorization: Bearer sk-..." https://api.acme.dev/widgets
```
MD

# Additional files to populate the sidebar tree (#12)
cat > "$DEMO_DIR/src/client.md" << 'MD'
# Client SDK

## Architecture

The TypeScript SDK wraps the REST API with type-safe methods,
automatic pagination, and retry logic.
MD

cat > "$DEMO_DIR/src/webhooks.md" << 'MD'
# Webhook System

## Event Types

- `widget.created` — New widget registered
- `widget.updated` — Widget configuration changed
- `widget.deleted` — Widget removed
MD

cat > "$DEMO_DIR/src/models.md" << 'MD'
# Data Models

## Widget

| Field    | Type   | Description           |
|----------|--------|-----------------------|
| `id`     | string | Unique identifier     |
| `name`   | string | Display name          |
| `status` | enum   | active, archived      |
MD

cat > "$DEMO_DIR/tests/integration.md" << 'MD'
# Integration Tests

## Setup

Tests run against a local PostgreSQL instance via Docker Compose.
MD

cat > "$DEMO_DIR/CONTRIBUTING.md" << 'MD'
# Contributing

## Getting Started

1. Fork the repository
2. Install dependencies: `npm install`
3. Run tests: `npm test`
MD

cat > "$DEMO_DIR/.github/workflows/ci.md" << 'MD'
# CI Pipeline

## Workflow

Runs on every PR: lint, type-check, unit tests, integration tests.
MD

# Create fake transcript for the demo session
# resolveTranscriptPath expects: ~/.claude/projects/<dir-encoded>/session-a7f3b2.jsonl
TRANSCRIPT_DIR="$HOME/.claude/projects/$(echo "$DEMO_DIR" | tr '/' '-')"
mkdir -p "$TRANSCRIPT_DIR"
cat > "$TRANSCRIPT_DIR/session-a7f3b2.jsonl" << 'JSONL'
{"type":"user","message":{"role":"user","content":"Add a changelog file for the Acme API documenting v1.2.0 changes"},"timestamp":"2026-02-25T10:00:00Z"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"I'll create a changelog documenting the v1.2.0 release with the new endpoints, rate limiting, and bug fixes."}]},"timestamp":"2026-02-25T10:00:02Z"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"tool_use","id":"tu_01","name":"Write","input":{"file_path":"docs/changelog.md","content":"# Changelog\n\n## v1.2.0\n\n### Added\n- DELETE /widgets/:id endpoint\n- Rate limiting (100 req/min)\n- Webhook retry with exponential backoff\n\n### Fixed\n- Cursor pagination duplicates\n- OAuth token refresh race condition"}}]},"timestamp":"2026-02-25T10:00:05Z"}
{"type":"tool_result","message":{"role":"tool","content":"File created: docs/changelog.md (12 lines)"},"timestamp":"2026-02-25T10:00:06Z"}
{"type":"user","message":{"role":"user","content":"Now update the README to mention rate limiting in the features section"},"timestamp":"2026-02-25T10:00:10Z"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"I'll add rate limiting to the features list in the README."}]},"timestamp":"2026-02-25T10:00:11Z"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"tool_use","id":"tu_02","name":"Edit","input":{"file_path":"README.md","old_string":"- **Webhooks**","new_string":"- **Rate limited** — 100 requests/minute per API key\n- **Webhooks**"}}]},"timestamp":"2026-02-25T10:00:13Z"}
{"type":"tool_result","message":{"role":"tool","content":"Applied edit to README.md"},"timestamp":"2026-02-25T10:00:14Z"}
{"type":"user","message":{"role":"user","content":"The auth docs are outdated — we switched from API keys to OAuth. Can you rewrite docs/auth.md?"},"timestamp":"2026-02-25T10:00:20Z"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"I'll rewrite the authentication docs to cover OAuth 2.0 with PKCE, replacing the API key instructions."}]},"timestamp":"2026-02-25T10:00:22Z"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"tool_use","id":"tu_03","name":"Write","input":{"file_path":"docs/auth.md","content":"# Authentication\n\n## OAuth 2.0 with PKCE\n\nAll API access requires OAuth tokens. We use the Authorization Code flow with PKCE for public clients.\n\n### 1. Register your application\n\nCreate an OAuth app in the dashboard to get your `client_id`.\n\n### 2. Authorize\n\n```\nGET https://auth.acme.dev/authorize\n  ?client_id=YOUR_CLIENT_ID\n  &redirect_uri=http://localhost:3000/callback\n  &response_type=code\n  &code_challenge=CHALLENGE\n  &code_challenge_method=S256\n```\n\n### 3. Exchange code for token\n\n```bash\ncurl -X POST https://auth.acme.dev/token \\\n  -d grant_type=authorization_code \\\n  -d code=AUTH_CODE \\\n  -d client_id=YOUR_CLIENT_ID \\\n  -d code_verifier=VERIFIER\n```\n\n### 4. Use the token\n\n```bash\ncurl -H \"Authorization: Bearer ACCESS_TOKEN\" \\\n  https://api.acme.dev/widgets\n```\n\nTokens expire after 1 hour. Use the refresh token to get a new access token without re-authorization."}}]},"timestamp":"2026-02-25T10:00:28Z"}
{"type":"tool_result","message":{"role":"tool","content":"File written: docs/auth.md (35 lines)"},"timestamp":"2026-02-25T10:00:29Z"}
{"type":"user","message":{"role":"user","content":"Looks good. Also add a rate limiting section to the API docs explaining the 100 req/min limit and the retry-after header."},"timestamp":"2026-02-25T10:00:35Z"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"I'll append a rate limiting section to the API reference doc."}]},"timestamp":"2026-02-25T10:00:36Z"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"tool_use","id":"tu_04","name":"Edit","input":{"file_path":"docs/api.md","old_string":"### `POST /widgets`","new_string":"## Rate Limiting\n\nAll endpoints enforce a rate limit of **100 requests per minute** per API key.\n\n| Header | Description |\n|--------|-------------|\n| `X-RateLimit-Limit` | Request limit per window |\n| `X-RateLimit-Remaining` | Requests remaining |\n| `Retry-After` | Seconds until limit resets (on 429) |\n\nWhen rate limited, the API returns `429 Too Many Requests`. Back off for the duration specified in `Retry-After`.\n\n### `POST /widgets`"}}]},"timestamp":"2026-02-25T10:00:40Z"}
{"type":"tool_result","message":{"role":"tool","content":"Applied edit to docs/api.md"},"timestamp":"2026-02-25T10:00:41Z"}
{"type":"user","message":{"role":"user","content":"Perfect. One more thing — add a CONTRIBUTING.md with basic guidelines."},"timestamp":"2026-02-25T10:00:50Z"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"I'll create a contributing guide with setup instructions, PR process, and code style guidelines."}]},"timestamp":"2026-02-25T10:00:52Z"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"tool_use","id":"tu_05","name":"Write","input":{"file_path":"CONTRIBUTING.md","content":"# Contributing\n\nThanks for your interest in contributing to Acme API!\n\n## Getting Started\n\n1. Fork the repository\n2. Clone your fork: `git clone https://github.com/YOUR_USER/acme-api`\n3. Install dependencies: `npm install`\n4. Run tests: `npm test`\n\n## Pull Requests\n\n- Branch from `main`\n- Keep PRs focused — one feature or fix per PR\n- Add tests for new endpoints\n- Update docs if you change API behavior\n\n## Code Style\n\n- We use Prettier with default settings\n- Run `npm run lint` before committing\n- Prefer descriptive variable names over comments"}}]},"timestamp":"2026-02-25T10:00:58Z"}
{"type":"tool_result","message":{"role":"tool","content":"File created: CONTRIBUTING.md (20 lines)"},"timestamp":"2026-02-25T10:00:59Z"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"Done! Here's a summary of all changes:\n\n1. **docs/changelog.md** — Created v1.2.0 changelog with new endpoints, rate limiting, and bug fixes\n2. **README.md** — Added rate limiting to features list\n3. **docs/auth.md** — Rewrote from API keys to OAuth 2.0 with PKCE\n4. **docs/api.md** — Added rate limiting section with headers and retry behavior\n5. **CONTRIBUTING.md** — Created contributing guidelines\n\nFive files touched across the session."}]},"timestamp":"2026-02-25T10:01:02Z"}
JSONL
# Conversation-only transcript (no hook events — discovered via transcript scanning)
cat > "$TRANSCRIPT_DIR/session-conv01.jsonl" << 'JSONL'
{"type":"user","message":{"role":"user","content":"What's the best approach for adding WebSocket support to our widget API? Should we use Server-Sent Events or full WebSocket connections?"},"timestamp":"2026-02-25T09:30:00Z"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"For your widget API, I'd recommend Server-Sent Events (SSE) over WebSockets for a few reasons:\n\n1. **Unidirectional updates** — Widget state changes flow server→client. You don't need bidirectional communication.\n2. **HTTP/2 multiplexing** — SSE works over regular HTTP, so you get connection multiplexing for free.\n3. **Simpler infrastructure** — No WebSocket upgrade handling, works through most proxies and load balancers.\n4. **Auto-reconnection** — The EventSource API handles reconnection with Last-Event-ID replay.\n\nWebSockets would be better if you needed client→server streaming (e.g., collaborative editing), but for push notifications about widget changes, SSE is the simpler and more robust choice."}]},"timestamp":"2026-02-25T09:30:15Z"}
{"type":"user","message":{"role":"user","content":"Good points. How would we handle authentication for SSE connections? Our API uses OAuth tokens that expire after 1 hour."},"timestamp":"2026-02-25T09:31:00Z"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"SSE has a limitation here — the EventSource API doesn't support custom headers, so you can't pass the Bearer token directly.\n\nThree common approaches:\n\n1. **Query parameter token** — `new EventSource('/events?token=...')`. Simple but the token appears in server logs. Use short-lived tokens.\n2. **Cookie-based auth** — Set an HttpOnly cookie on login. EventSource sends cookies automatically. Best security but requires cookie infrastructure.\n3. **Ticket-based** — POST to get a one-time ticket, then `EventSource('/events?ticket=...')`. The ticket is single-use and short-lived (~30s). Best of both worlds.\n\nI'd go with option 3 (ticket-based) for your setup since you already have OAuth. The flow would be: exchange OAuth token for a one-time SSE ticket via POST, then connect with that ticket."}]},"timestamp":"2026-02-25T09:32:00Z"}
JSONL

log "Fake transcripts at $TRANSCRIPT_DIR"

# Create fake memory files for the Memory Browser demo
# Each project needs: ~/.claude/projects/<encoded>/memory/MEMORY.md
MEMORY_BASE="$HOME/.claude/projects"
DEMO_ENCODED="$(echo "$DEMO_DIR" | tr '/' '-')"
MEMORY_DIRS_TO_CLEAN=()

create_demo_memory() {
    local dir="$1" content="$2"
    mkdir -p "$dir/memory"
    echo "$content" > "$dir/memory/MEMORY.md"
    MEMORY_DIRS_TO_CLEAN+=("$dir/memory")
}

create_demo_memory "$MEMORY_BASE/$DEMO_ENCODED" "# Acme API Memory

## Architecture

- REST API with Express.js, PostgreSQL, Redis cache
- OAuth 2.0 with PKCE for authentication
- Rate limiting: 100 req/min per API key

## Key Gotchas

### Cursor pagination
Use keyset pagination (not offset) — offset skips break on concurrent inserts.

### Webhook retries
Exponential backoff with jitter. Max 5 retries over 24 hours.

## Dependencies

- express v4.18, pg v8.11, ioredis v5.3
- jest for testing, supertest for integration tests"

create_demo_memory "$MEMORY_BASE/-Users-rd-projects-demo-webapp" "# Demo Webapp Memory

## Architecture

- Next.js 14 with App Router, TypeScript, Tailwind CSS
- Vercel deployment with edge functions

## Key Gotchas

### Server components
Default to server components. Only add 'use client' when needed for interactivity."

create_demo_memory "$MEMORY_BASE/-Users-rd-projects-demo-infra" "# Infra Memory

## Architecture

- Terraform + AWS CDK for infrastructure as code
- ECS Fargate for container orchestration

## Deployment

- Blue/green deploys via CodeDeploy
- Rollback triggers on 5xx spike > 5%"

log "Created demo memory files for Memory Browser"

# --------------- Step 2: Start peekm ---------------
log "Starting peekm on port $PORT..."
"$PEEKM_BIN" -port "$PORT" -browser=false -demo "$DEMO_DIR" &
PEEKM_PID=$!
sleep 1

if ! kill -0 "$PEEKM_PID" 2>/dev/null; then
    err "peekm failed to start (port $PORT in use?)"
    exit 1
fi

# --------------- Step 3: Open Chrome in --app mode (no tab/address bar) ---------------
log "Opening Chrome in app mode (${VIEWPORT_W}x${VIEWPORT_H})..."
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
CHROME_DATA_DIR=$(mktemp -d /tmp/peekm-chrome-XXXXXX)

"$CHROME" \
    --app="http://localhost:$PORT" \
    --window-size=${VIEWPORT_W},${VIEWPORT_H} \
    --window-position=100,100 \
    --user-data-dir="$CHROME_DATA_DIR" \
    --remote-debugging-port=${CDP_PORT} \
    --no-first-run \
    --disable-extensions \
    --disable-infobars 2>/dev/null &
CHROME_PID=$!

log "Waiting for page load..."
sleep 3

# Force dark theme regardless of system preference (#7)
log "Forcing dark theme..."
run_js "
    document.documentElement.setAttribute('data-theme', 'dark');
    document.documentElement.setAttribute('data-color-mode', 'dark');
    localStorage.setItem('theme', 'dark');
"
sleep 0.5

# Resize window via CDP (AppleScript can't see --user-data-dir Chrome instances)
run_js "window.moveTo(100, 100); window.resizeTo(${VIEWPORT_W}, ${VIEWPORT_H});"

# --------------- Step 4: Start screen recording ---------------
MOV_FILE=$(mktemp /tmp/peekm-demo-XXXXXX.mov)

# Get main screen logical dimensions via NSScreen (reliable on multi-monitor setups)
SCREEN_LOGICAL_W=$(osascript -l JavaScript -e '
    ObjC.import("AppKit");
    $.NSScreen.mainScreen.frame.size.width
')
SCREEN_LOGICAL_H=$(osascript -l JavaScript -e '
    ObjC.import("AppKit");
    $.NSScreen.mainScreen.frame.size.height
')
log "Screen logical size: ${SCREEN_LOGICAL_W}x${SCREEN_LOGICAL_H}"

# Find the correct screen capture device index (not camera, not iPhone).
# Note: ffmpeg -list_devices always exits non-zero, so suppress pipefail.
SCREEN_INDEX=$(ffmpeg -f avfoundation -list_devices true -i "" 2>&1 \
    | grep "Capture screen" | head -1 | sed 's/.*\[\([0-9]*\)\].*/\1/' || true)

if [ -z "$SCREEN_INDEX" ]; then
    err "No screen capture device found. Available devices:"
    ffmpeg -f avfoundation -list_devices true -i "" 2>&1 || true
    exit 1
fi
log "Using avfoundation screen device index: $SCREEN_INDEX"

# Record at 24fps — evenly divisible by output 12fps for smooth frame cadence (#11)
log "Recording ${RECORD_SECONDS}s of full screen (will crop to browser window)..."
ffmpeg -y -loglevel warning \
    -f avfoundation -framerate 24 -capture_cursor 0 \
    -pixel_format uyvy422 \
    -probesize 50M \
    -i "${SCREEN_INDEX}:none" \
    -t "$RECORD_SECONDS" \
    -r 24 \
    -c:v libx264 -preset ultrafast -crf 18 \
    "$MOV_FILE" &
FFMPEG_PID=$!

sleep 1  # Let recording stabilize

# Bring the --app Chrome to front via its PID
osascript -e "tell application \"System Events\" to set frontmost of (first process whose unix id is $CHROME_PID) to true" || true
sleep 0.5

# --------------- Step 5: Orchestrate demo actions ---------------

# Dwell: let viewer absorb the clean UI before any action (#5)
show_caption "Markdown viewer with live reload"
sleep 2

# Scene 1: Simulate Claude Code creating a new file (triggers toast)
log "Scene 1: Simulating Claude Code creating docs/changelog.md..."
hide_caption
NEW_FILE="$DEMO_DIR/docs/changelog.md"

curl -s -X POST "http://localhost:$PORT/hook/file-modified" \
    -H "Content-Type: application/json" \
    -H "Origin: http://localhost:$PORT" \
    -d '{
        "file_path": "'"$NEW_FILE"'",
        "session_id": "session-a7f3b2",
        "tool_name": "Write",
        "permission_mode": "allowedTools"
    }' > /dev/null

cat > "$NEW_FILE" << 'MD'
# Changelog

## v1.2.0 (2026-02-17)

### Added
- `DELETE /widgets/:id` endpoint for removing widgets
- Rate limiting on all API endpoints (100 req/min)
- Webhook retry with exponential backoff

### Fixed
- Cursor pagination returning duplicate results
- OAuth token refresh race condition
MD

show_caption "Real-time AI notification"
sleep 2

# Scene 2: Second AI edit to build up timeline
log "Scene 2: Simulating Claude Code editing README.md..."
curl -s -X POST "http://localhost:$PORT/hook/file-modified" \
    -H "Content-Type: application/json" \
    -H "Origin: http://localhost:$PORT" \
    -d '{
        "file_path": "'"$DEMO_DIR/README.md"'",
        "session_id": "session-a7f3b2",
        "tool_name": "Edit",
        "permission_mode": "allowedTools"
    }' > /dev/null

echo "" >> "$DEMO_DIR/README.md"
sleep 1.5

# Scene 3: Navigate to new file — shows session badge (SPA navigation, no white flash)
log "Scene 3: Navigating to file with session info..."
hide_caption
run_js "navigate('/view/docs/changelog.md')"
sleep 0.5
show_caption "AI-modified file with session metadata"
sleep 2

# Scene 4: Two-tier sharing — LAN then public
log "Scene 4: Sharing file (LAN + public)..."
hide_caption
run_js "toggleShare()"
sleep 0.5
show_caption "Share on your local network"
sleep 1.3

# Click "Make public" — triggers mock tunnel via --demo flag
run_js "makeSharePublic()"
sleep 0.8
show_caption "...or make it public with one click"
sleep 1.5

# Close share panel before moving on
run_js "setSharePanelOpen(false)"
sleep 0.3

# Scene 5: Open AI timeline + expand session card — shows both edit and conversation sessions
log "Scene 5: Opening AI timeline..."
hide_caption
run_js "navigate('/timeline')"
sleep 1
# Expand the first session card to show event details
run_js "
    var header = document.querySelector('.timeline-session-header');
    if (header) toggleTimelineSession(header);
"
show_caption "All AI sessions — edits and conversations"
sleep 2.5

# Scene 6: Toggle filter to "Edits only" then back to "All"
log "Scene 6: Demonstrating filter toggle..."
hide_caption
run_js "setTimelineFilter('edits')"
sleep 0.3
show_caption "Filter: Edits only"
sleep 1
run_js "setTimelineFilter('all')"
sleep 0.3
show_caption "Filter: All sessions"
sleep 1

# Scene 7: Open transcript viewer
log "Scene 7: Opening transcript viewer..."
hide_caption
run_js "navigate('/transcript?session=session-a7f3b2')"
sleep 0.5
show_caption "Full AI conversation transcript"
sleep 2.5

# Scene 8: Open memory browser dashboard
log "Scene 8: Opening memory browser..."
hide_caption
run_js "navigate('/memory')"
sleep 0.5
show_caption "Cross-project memory browser"
sleep 2.5

hide_caption

# --------------- Step 6: Stop recording and convert ---------------
log "Stopping recording..."
kill "$FFMPEG_PID" 2>/dev/null || true
wait "$FFMPEG_PID" 2>/dev/null || true
FFMPEG_PID=""

if [ ! -f "$MOV_FILE" ] || [ ! -s "$MOV_FILE" ]; then
    err "Recording failed — no video captured."
    err "Tip: grant Screen Recording permission to Terminal in System Settings > Privacy & Security"
    exit 1
fi

# Compute crop region: map logical window bounds to capture pixel coordinates.
# The scale factor is often fractional (e.g. 1920/1440 = 1.333), so use awk.
CAPTURE_W=$(ffprobe -v error -select_streams v:0 -show_entries stream=width -of csv=p=0 "$MOV_FILE")
CAPTURE_H=$(ffprobe -v error -select_streams v:0 -show_entries stream=height -of csv=p=0 "$MOV_FILE")

read -r CROP_W CROP_H CROP_X CROP_Y <<< "$(awk -v cw="$CAPTURE_W" -v ch="$CAPTURE_H" \
    -v sw="$SCREEN_LOGICAL_W" -v sh="$SCREEN_LOGICAL_H" \
    -v ww="$VIEWPORT_W" -v wh="$VIEWPORT_H" \
    'BEGIN {
        sx = cw / sw; sy = ch / sh
        # Use int() to floor — ffmpeg crop needs integer pixel values
        printf "%d %d %d %d", int(ww*sx), int(wh*sy), int(100*sx), int(100*sy)
    }')"

log "Capture: ${CAPTURE_W}x${CAPTURE_H}, logical: ${SCREEN_LOGICAL_W}x${SCREEN_LOGICAL_H}"
log "Crop: ${CROP_W}x${CROP_H}+${CROP_X}+${CROP_Y}"

log "Converting to optimized GIF (${GIF_WIDTH}px wide, ${FPS}fps)..."
ffmpeg -y -loglevel error \
    -i "$MOV_FILE" \
    -vf "crop=${CROP_W}:${CROP_H}:${CROP_X}:${CROP_Y},fps=${FPS},scale=${GIF_WIDTH}:-1:flags=lanczos,split[s0][s1];[s0]palettegen=max_colors=128:stats_mode=diff[p];[s1][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle" \
    -loop 0 \
    "$OUTPUT"

# Further compress with gifsicle if available
if command -v gifsicle &>/dev/null; then
    log "Optimizing with gifsicle..."
    gifsicle -O3 --lossy=60 --colors 128 "$OUTPUT" -o "$OUTPUT"
fi

SIZE_BYTES=$(stat -f%z "$OUTPUT")
SIZE_MB=$(awk "BEGIN { printf \"%.1f\", $SIZE_BYTES / 1048576 }")
log "Hero GIF saved to $OUTPUT (${SIZE_MB}MB)"

if awk "BEGIN { exit ($SIZE_MB > $MAX_GIF_SIZE_MB) ? 0 : 1 }"; then
    warn "GIF is over ${MAX_GIF_SIZE_MB}MB. Consider reducing RECORD_SECONDS, FPS, or GIF_WIDTH."
fi

log ""
log "Add to README.md:"
log '  ![peekm demo](assets/hero-demo.gif)'
