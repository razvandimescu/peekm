package transcript

import (
	"encoding/json"
	"time"
)

// Version identifies the JSON shape of Session. Serialized sessions carry it
// so consumers can detect breaking changes to the model.
const Version = 1

// Session.Harness values.
const (
	HarnessClaudeCode = "claude-code"
	HarnessPi         = "pi"
)

// Block kinds.
const (
	KindText       = "text"
	KindThinking   = "thinking"
	KindToolCall   = "tool_call"
	KindToolResult = "tool_result" // result whose call is not in the session
)

// Session is one parsed agent session: pure data, no rendering concerns.
// It round-trips through JSON unchanged.
type Session struct {
	Version int    `json:"version"`
	ID      string `json:"id,omitempty"`
	Harness string `json:"harness"`
	CWD     string `json:"cwd,omitempty"`
	Turns   []Turn `json:"turns"`
}

// Turn is a user or assistant turn. Consecutive same-role messages are merged.
type Turn struct {
	Role      string    `json:"role"` // "user" or "assistant"
	Model     string    `json:"model,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	Blocks    []Block   `json:"blocks"`
}

// Block is one piece of turn content, discriminated by Kind.
type Block struct {
	Kind   string      `json:"kind"`
	Text   string      `json:"text,omitempty"`   // KindText, KindThinking
	Tool   *ToolCall   `json:"tool,omitempty"`   // KindToolCall
	Result *ToolResult `json:"result,omitempty"` // KindToolResult
}

// ToolCall is a tool invocation with its result attached when the session
// contains one. Name and Input use Claude Code's vocabulary regardless of
// harness (pi's edit/path arrive as Edit/file_path); RawName and RawInput
// preserve the harness-native call when it differs.
type ToolCall struct {
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name"`
	RawName  string          `json:"raw_name,omitempty"`
	Input    map[string]any  `json:"input,omitempty"`
	RawInput json.RawMessage `json:"raw_input,omitempty"`
	Result   *ToolResult     `json:"result,omitempty"`
}

// ToolResult is the outcome of a tool call.
type ToolResult struct {
	CallID       string  `json:"call_id,omitempty"` // id of the call this answers
	Text         string  `json:"text,omitempty"`
	Preformatted bool    `json:"preformatted,omitempty"` // Text is verbatim output, not markdown
	IsError      bool    `json:"is_error,omitempty"`
	Images       []Image `json:"images,omitempty"`
}

// Image is base64-encoded image content from a tool result.
type Image struct {
	MediaType string `json:"media_type"` // e.g. "image/png"
	Data      string `json:"data"`
}

// parseInputMap unmarshals tool input JSON into a generic map, nil on failure.
func parseInputMap(input json.RawMessage) map[string]any {
	if len(input) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return nil
	}
	return m
}

// parseTime parses an RFC3339 timestamp, zero time on failure.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
