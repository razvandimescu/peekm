#!/bin/bash
# record-demo.sh — Records a hero GIF of peekm's AI session tracking workflow.
#
# Prerequisites: ffmpeg, peekm (go build -o peekm)
# Usage: ./scripts/record-demo.sh [output.gif]
#
# The script:
#   1. Creates a demo project with sample markdown
#   2. Starts peekm on a non-conflicting port
#   3. Opens Chrome at a fixed viewport size
#   4. Records the browser window via ffmpeg (screen capture + crop)
#   5. Orchestrates demo actions: file edits, Claude Code hook simulation
#   6. Converts recording to an optimized GIF

set -euo pipefail

# --------------- Configuration ---------------
OUTPUT="${1:-assets/hero-demo.gif}"
PORT=16419
RECORD_SECONDS=12
VIEWPORT_W=1200
VIEWPORT_H=750
FPS=8
GIF_WIDTH=640
MAX_GIF_SIZE_MB=5

# --------------- State ---------------
DEMO_DIR=""
PEEKM_PID=""
FFMPEG_PID=""
MOV_FILE=""
CHROME_OPENED=false

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
    [ -n "$DEMO_DIR" ]   && rm -rf "$DEMO_DIR"
    [ -n "$MOV_FILE" ] && [ -f "$MOV_FILE" ] && rm -f "$MOV_FILE"
    if $CHROME_OPENED; then
        osascript -e 'tell application "Google Chrome"
            repeat with w in windows
                if URL of active tab of w contains "localhost:'"$PORT"'" then close w
            end repeat
        end tell' 2>/dev/null || true
    fi
    log "Done."
}
trap cleanup EXIT

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

mkdir -p "$DEMO_DIR/docs" "$DEMO_DIR/src"

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

# --------------- Step 2: Start peekm ---------------
log "Starting peekm on port $PORT..."
"$PEEKM_BIN" -port "$PORT" -browser=false "$DEMO_DIR" &
PEEKM_PID=$!
sleep 1

if ! kill -0 "$PEEKM_PID" 2>/dev/null; then
    err "peekm failed to start (port $PORT in use?)"
    exit 1
fi

# --------------- Step 3: Open Chrome at fixed size ---------------
log "Opening Chrome window (${VIEWPORT_W}x${VIEWPORT_H})..."
osascript << APPLESCRIPT
tell application "Google Chrome"
    set newWindow to make new window
    set URL of active tab of newWindow to "http://localhost:$PORT"
    set bounds of newWindow to {100, 100, $((100 + VIEWPORT_W)), $((100 + VIEWPORT_H))}
    activate
end tell
APPLESCRIPT
CHROME_OPENED=true

log "Waiting for page load..."
sleep 3

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
SCREEN_INDEX=$(set +o pipefail; ffmpeg -f avfoundation -list_devices true -i "" 2>&1 \
    | grep "Capture screen" | head -1 | sed 's/.*\[\([0-9]*\)\].*/\1/')

if [ -z "$SCREEN_INDEX" ]; then
    err "No screen capture device found. Available devices:"
    ffmpeg -f avfoundation -list_devices true -i "" 2>&1 || true
    exit 1
fi
log "Using avfoundation screen device index: $SCREEN_INDEX"

log "Recording ${RECORD_SECONDS}s of full screen (will crop to browser window)..."
ffmpeg -y -loglevel warning \
    -f avfoundation -framerate 30 -capture_cursor 0 \
    -pixel_format uyvy422 \
    -probesize 50M \
    -i "${SCREEN_INDEX}:none" \
    -t "$RECORD_SECONDS" \
    -r 30 \
    -c:v libx264 -preset ultrafast -crf 18 \
    "$MOV_FILE" &
FFMPEG_PID=$!

sleep 1  # Let recording stabilize

# --------------- Step 5: Orchestrate demo actions ---------------

# Scene 1: Brief glimpse of README with sidebar
log "Scene 1: Showing README..."
sleep 1

# Scene 2: Simulate Claude Code creating a new file (triggers toast + tree update)
log "Scene 2: Simulating Claude Code creating docs/changelog.md..."
NEW_FILE="$DEMO_DIR/docs/changelog.md"

# POST hook metadata first (simulates the PostToolUse hook firing before fsnotify)
curl -s -X POST "http://localhost:$PORT/hook/file-modified" \
    -H "Content-Type: application/json" \
    -H "Origin: http://localhost:$PORT" \
    -d '{
        "file_path": "'"$NEW_FILE"'",
        "session_id": "session-a7f3b2",
        "tool_name": "Write",
        "permission_mode": "allowedTools"
    }' > /dev/null

# Create the file to trigger fsnotify CREATE event
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

log "Toast notification should appear..."
sleep 3

# Scene 3: Click the toast / navigate to the new file to show session info panel
log "Scene 3: Navigating to new file to show session info..."
osascript -e 'tell application "Google Chrome" to set URL of active tab of front window to "http://localhost:'"$PORT"'/view/docs/changelog.md"'

sleep 3

# Scene 4: Hold on the session info panel
log "Scene 4: Session info panel visible..."
sleep 3

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
    -vf "crop=${CROP_W}:${CROP_H}:${CROP_X}:${CROP_Y},fps=${FPS},scale=${GIF_WIDTH}:-1:flags=lanczos,split[s0][s1];[s0]palettegen=max_colors=64:stats_mode=diff[p];[s1][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle" \
    -loop 0 \
    "$OUTPUT"

# Further compress with gifsicle if available
if command -v gifsicle &>/dev/null; then
    log "Optimizing with gifsicle..."
    gifsicle -O3 --lossy=80 --colors 64 "$OUTPUT" -o "$OUTPUT"
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
