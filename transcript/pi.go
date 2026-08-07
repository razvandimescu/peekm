package transcript

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// piHeader is the first line of a pi session file: {"type":"session",...}.
type piHeader struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	CWD  string `json:"cwd"`
}

// piEntry is one line of a pi session file (excluding the header).
type piEntry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  string          `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

// piMessage is the AgentMessage payload of a "message" entry. Fields cover the
// roles the model represents: user, assistant, toolResult, bashExecution.
type piMessage struct {
	Role       string          `json:"role"`
	Model      string          `json:"model"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"toolCallId"`
	IsError    bool            `json:"isError"`
	Command    string          `json:"command"`
	Output     string          `json:"output"`
	ExitCode   *int            `json:"exitCode"`
}

// IsPiSessionFile sniffs the header line pi writes as the first record:
// {"type":"session","version":3,...}. Claude transcripts never start this way.
func IsPiSessionFile(path string) bool {
	header, err := readPiHeader(path)
	return err == nil && header.Type == "session"
}

// PiSessionCwd reads the working directory from a pi session header line.
func PiSessionCwd(path string) string {
	header, err := readPiHeader(path)
	if err != nil || header.Type != "session" {
		return ""
	}
	return header.CWD
}

func readPiHeader(path string) (piHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return piHeader{}, err
	}
	defer f.Close()
	var header piHeader
	err = json.NewDecoder(bufio.NewReaderSize(f, 4096)).Decode(&header)
	return header, err
}

// ParsePi parses a pi v3 session file (JSONL with a header line and a
// parentId-linked entry tree; only the active branch is kept).
func ParsePi(r io.Reader) (*Session, error) {
	header, entries, err := readPiEntries(r)
	if err != nil {
		return nil, err
	}
	entries = piActiveBranch(entries)

	var turns []Turn
	for _, e := range entries {
		if e.Type != "message" {
			continue
		}
		turn, ok := piTurn(e)
		if !ok {
			continue
		}
		turns = append(turns, turn)
	}
	return &Session{
		Version: Version,
		ID:      header.ID,
		Harness: HarnessPi,
		CWD:     header.CWD,
		Turns:   finishTurns(turns),
	}, nil
}

func readPiEntries(r io.Reader) (piHeader, []piEntry, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	var header piHeader
	var entries []piEntry
	for scanner.Scan() {
		if header.Type == "" {
			var h piHeader
			if json.Unmarshal(scanner.Bytes(), &h) == nil && h.Type == "session" {
				header = h
				continue
			}
		}
		var e piEntry
		if json.Unmarshal(scanner.Bytes(), &e) != nil {
			continue
		}
		if e.Type == "session" || e.ID == "" {
			continue // duplicate header or malformed line
		}
		entries = append(entries, e)
	}
	return header, entries, scanner.Err()
}

// piActiveBranch filters entries to the live conversation path. Entries form a
// tree via parentId; every append moves the leaf, so the last entry in the file
// is the current leaf. Walking its parent chain to the root selects the active
// branch and drops abandoned ones.
func piActiveBranch(entries []piEntry) []piEntry {
	if len(entries) == 0 {
		return entries
	}
	byID := make(map[string]*piEntry, len(entries))
	for i := range entries {
		byID[entries[i].ID] = &entries[i]
	}

	onBranch := make(map[string]bool)
	for id := entries[len(entries)-1].ID; id != ""; {
		e, ok := byID[id]
		if !ok || onBranch[id] {
			break // broken or cyclic parent link
		}
		onBranch[id] = true
		id = e.ParentID
	}

	filtered := entries[:0]
	for _, e := range entries {
		if onBranch[e.ID] {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

func piTurn(entry piEntry) (Turn, bool) {
	var msg piMessage
	if json.Unmarshal(entry.Message, &msg) != nil {
		return Turn{}, false
	}

	var role string
	var blocks []Block
	switch msg.Role {
	case "user":
		role = "user"
		blocks = piTextBlocks(msg.Content)
	case "assistant":
		role = "assistant"
		blocks = piAssistantBlocks(msg.Content)
	case "toolResult":
		role = "user"
		blocks = piToolResultBlocks(msg)
	case "bashExecution":
		role = "user"
		blocks = []Block{piBashBlock(msg)}
	default:
		return Turn{}, false
	}
	if len(blocks) == 0 {
		return Turn{}, false
	}
	return Turn{Role: role, Model: msg.Model, Timestamp: parseTime(entry.Timestamp), Blocks: blocks}, true
}

// extractPiContent handles pi's content field: a plain string or an array of
// {type:"text"|"image"} blocks (images carry data/mimeType, unlike Claude's
// nested source object).
func extractPiContent(raw json.RawMessage) (string, []Image) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", nil
	}
	var buf strings.Builder
	var images []Image
	for _, p := range parts {
		switch p.Type {
		case "text":
			if buf.Len() > 0 {
				buf.WriteByte('\n')
			}
			buf.WriteString(p.Text)
		case "image":
			if p.Data != "" && p.MimeType != "" {
				images = append(images, Image{MediaType: p.MimeType, Data: p.Data})
			}
		}
	}
	return buf.String(), images
}

func piTextBlocks(raw json.RawMessage) []Block {
	text, _ := extractPiContent(raw)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []Block{{Kind: KindText, Text: text}}
}

func piAssistantBlocks(raw json.RawMessage) []Block {
	var parts []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var blocks []Block
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				blocks = append(blocks, Block{Kind: KindText, Text: p.Text})
			}
		case "thinking":
			if p.Thinking != "" {
				blocks = append(blocks, Block{Kind: KindThinking, Text: p.Thinking})
			}
		case "toolCall":
			blocks = append(blocks, piToolCallBlock(p.ID, p.Name, p.Arguments))
		}
	}
	return blocks
}

func piToolCallBlock(id, name string, args json.RawMessage) Block {
	canonical, m := normalizePiToolInput(name, parseInputMap(args))
	tc := &ToolCall{ID: id, Name: canonical, Input: m, RawInput: args}
	if canonical != name {
		tc.RawName = name
	}
	return Block{Kind: KindToolCall, Tool: tc}
}

func piToolResultBlocks(msg piMessage) []Block {
	text, images := extractPiContent(msg.Content)
	if text == "" && len(images) == 0 {
		return nil
	}
	return []Block{{Kind: KindToolResult, Result: &ToolResult{
		CallID:  msg.ToolCallID,
		Text:    text,
		IsError: msg.IsError,
		Images:  images,
	}}}
}

// piBashBlock models a user-invoked shell command (pi's "!" prefix) as a Bash
// tool call with its captured output attached as the result.
func piBashBlock(msg piMessage) Block {
	tc := &ToolCall{
		Name:    "Bash",
		RawName: "bashExecution",
		Input:   map[string]any{"command": msg.Command},
	}
	output := msg.Output
	failed := msg.ExitCode != nil && *msg.ExitCode != 0
	if failed {
		output = fmt.Sprintf("%s\n(exit code %d)", output, *msg.ExitCode)
	}
	if strings.TrimSpace(output) != "" {
		tc.Result = &ToolResult{Text: output, IsError: failed}
	}
	return Block{Kind: KindToolCall, Tool: tc}
}

// piToolNames maps pi's built-in tools to their Claude Code equivalents, the
// model's canonical tool vocabulary.
var piToolNames = map[string]string{
	"bash":  "Bash",
	"read":  "Read",
	"edit":  "Edit",
	"write": "Write",
	"grep":  "Grep",
	"find":  "Glob",
	"ls":    "List",
}

// normalizePiToolInput rewrites a pi tool call into Claude Code naming: the
// tool name and argument keys (path → file_path, edits[].oldText → old_string)
// so consumers treat both harnesses identically.
func normalizePiToolInput(name string, args map[string]any) (string, map[string]any) {
	display, known := piToolNames[name]
	if !known {
		display = capitalizeFirst(name)
	}
	if args == nil {
		return display, nil
	}
	m := make(map[string]any, len(args))
	for k, v := range args {
		m[k] = v
	}
	switch name {
	case "read", "edit", "write", "ls":
		if p, ok := m["path"].(string); ok {
			m["file_path"] = p
			delete(m, "path")
		}
	}
	if name == "edit" {
		display = normalizePiEdits(m)
	}
	return display, m
}

// normalizePiEdits converts pi's edits[]{oldText,newText} to Claude key names.
// A single edit renders as Edit, multiple as MultiEdit.
func normalizePiEdits(m map[string]any) string {
	edits, ok := m["edits"].([]any)
	if !ok || len(edits) == 0 {
		return "Edit"
	}
	converted := make([]any, 0, len(edits))
	for _, e := range edits {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		converted = append(converted, map[string]any{
			"old_string": em["oldText"],
			"new_string": em["newText"],
		})
	}
	if len(converted) == 1 {
		single := converted[0].(map[string]any)
		m["old_string"] = single["old_string"]
		m["new_string"] = single["new_string"]
		delete(m, "edits")
		return "Edit"
	}
	m["edits"] = converted
	return "MultiEdit"
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}
