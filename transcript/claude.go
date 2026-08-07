package transcript

import (
	"encoding/json"
	"io"
	"strings"
)

// claudeLine is the envelope of one Claude Code transcript JSONL line.
type claudeLine struct {
	Type      string          `json:"type"`
	IsMeta    bool            `json:"isMeta"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

// claudeMsg is the message body within a transcript line.
type claudeMsg struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

// claudeContentBlock matches the JSON structure of a message content block.
type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
}

// ParseClaude parses a Claude Code session transcript (JSONL).
func ParseClaude(r io.Reader) (*Session, error) {
	scanner := newLineScanner(r)

	var turns []Turn
	for scanner.Scan() {
		turn, ok := parseClaudeLine(scanner.Bytes())
		if !ok {
			continue
		}
		turns = append(turns, turn)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return &Session{Version: Version, Harness: HarnessClaudeCode, Turns: finishTurns(turns)}, nil
}

// parseClaudeLine parses a single JSONL line; ok is false for meta lines,
// non-conversation types, and malformed input.
func parseClaudeLine(line []byte) (Turn, bool) {
	var env claudeLine
	if json.Unmarshal(line, &env) != nil {
		return Turn{}, false
	}
	if env.IsMeta || (env.Type != "user" && env.Type != "assistant") || len(env.Message) == 0 {
		return Turn{}, false
	}
	var msg claudeMsg
	if json.Unmarshal(env.Message, &msg) != nil {
		return Turn{}, false
	}
	blocks := claudeContentBlocks(msg.Content)
	if len(blocks) == 0 {
		return Turn{}, false
	}
	return Turn{Role: msg.Role, Model: msg.Model, Timestamp: parseTime(env.Timestamp), Blocks: blocks}, true
}

// claudeContentBlocks converts a message content field — a plain string or an
// array of typed blocks — into model blocks.
func claudeContentBlocks(raw json.RawMessage) []Block {
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if str == "" {
			return nil
		}
		return []Block{{Kind: KindText, Text: str}}
	}

	var rawBlocks []json.RawMessage
	if json.Unmarshal(raw, &rawBlocks) != nil {
		return nil
	}
	var blocks []Block
	for _, rb := range rawBlocks {
		if b, ok := claudeBlock(rb); ok {
			blocks = append(blocks, b)
		}
	}
	return blocks
}

func claudeBlock(rb json.RawMessage) (Block, bool) {
	var peek claudeContentBlock
	if json.Unmarshal(rb, &peek) != nil {
		return Block{}, false
	}
	switch peek.Type {
	case "text":
		if peek.Text == "" {
			return Block{}, false
		}
		return Block{Kind: KindText, Text: peek.Text}, true
	case "thinking":
		if peek.Thinking == "" {
			return Block{}, false
		}
		return Block{Kind: KindThinking, Text: peek.Thinking}, true
	case "tool_use":
		return Block{Kind: KindToolCall, Tool: &ToolCall{
			ID:       peek.ID,
			Name:     peek.Name,
			Input:    parseInputMap(peek.Input),
			RawInput: peek.Input,
		}}, true
	case "tool_result":
		text, images := claudeResultContent(peek.Content)
		if text == "" && len(images) == 0 {
			return Block{}, false
		}
		return Block{Kind: KindToolResult, Result: &ToolResult{
			CallID:  peek.ToolUseID,
			Text:    text,
			IsError: peek.IsError,
			Images:  images,
		}}, true
	}
	return Block{}, false
}

// claudeResultContent extracts text and images from a tool_result content
// field: a plain string or an array of typed content blocks.
func claudeResultContent(content json.RawMessage) (string, []Image) {
	if len(content) == 0 {
		return "", nil
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return "", nil
	}
	var buf strings.Builder
	var images []Image
	for _, p := range parts {
		switch p.Type {
		case "text":
			buf.WriteString(p.Text)
		case "image":
			if p.Source.Type == "base64" && p.Source.MediaType != "" && p.Source.Data != "" {
				images = append(images, Image{MediaType: p.Source.MediaType, Data: p.Source.Data})
			}
		}
	}
	return buf.String(), images
}
