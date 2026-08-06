package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
)

// transcriptTemplateData is used for rendering the transcript viewer
type transcriptTemplateData struct {
	baseTemplateData
	TreeHTML     template.HTML
	Title        string
	Subtitle     string
	BrowsePath   string
	SessionID    string
	Turns        []transcriptTurn
	NotFound     bool
	SessionStats *sessionStats
}

// transcriptTurn represents a single user or assistant turn in the conversation
type transcriptTurn struct {
	Role        string // "user" or "assistant"
	Blocks      []contentBlock
	Model       string
	Timestamp   string
	Collapsible bool // whole-turn clamp when accumulated content renders tall
	LineCount   int  // estimated rendered lines for the toggle label
}

// imageData represents a base64-encoded image from a tool result
type imageData struct {
	MediaType string // e.g. "image/png"
	Data      string // base64-encoded
}

// contentBlock represents a piece of content within a turn
type contentBlock struct {
	Type            string        // "text", "tool_use", "tool_result", "thinking", "context_summary"
	HTML            template.HTML // rendered markdown (for text blocks)
	Text            string        // raw text (for thinking, tool input)
	Collapsible     bool          // long user text blocks get fade-out + expand button
	LineCount       int           // line count hint for collapsible blocks
	ToolName        string        // for tool_use blocks
	ToolInput       string        // pretty-printed, truncated
	ToolID          string        // for pairing tool_use ↔ tool_result
	Result          *contentBlock // paired tool_result (nil if unpaired)
	ToolDisplayName string        // humanized name
	ToolServer      string        // MCP server prefix
	ToolSummary     string        // short preview for collapsed summary line
	ToolInputHTML   template.HTML // structured rendering
	ItemCount       int           // for context_summary
	Images          []imageData   // for tool_result blocks containing images
	textChars       int           // raw text size, for turn-level collapse accounting
	textLines       int
}

// encodeProjectDir maps an absolute directory to the name Claude Code uses for its
// project folder under ~/.claude/projects (slashes become dashes).
func encodeProjectDir(dir string) string {
	return strings.ReplaceAll(dir, "/", "-")
}

// sessionIDRe matches Claude Code session IDs (UUIDs). Anything else is rejected
// before path construction: filepath.Join cleans "..", so an unvalidated session ID
// would escape ~/.claude/projects and read arbitrary .jsonl files.
var sessionIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// resolveTranscriptPath finds a session transcript by ID, checking Claude
// Code's project store first, then pi's session store. The source return is
// "" for Claude Code and "pi" for pi, so callers can dispatch on harness
// without re-inferring it from file content.
func resolveTranscriptPath(sessionID string) (path, source string) {
	if p := resolveClaudeTranscriptPath(sessionID); p != "" {
		return p, ""
	}
	if p := resolvePiTranscriptPath(sessionID); p != "" {
		return p, "pi"
	}
	return "", ""
}

// parseTranscriptFor dispatches to the harness-appropriate transcript parser.
func parseTranscriptFor(path, source string) ([]transcriptTurn, error) {
	if source == "pi" {
		return parsePiTranscript(path)
	}
	return parseTranscript(path)
}

// resolveClaudeTranscriptPath finds a Claude Code transcript by scanning project
// directories. Tries the current browseDir first, then all project dirs.
func resolveClaudeTranscriptPath(sessionID string) string {
	if !sessionIDRe.MatchString(sessionID) {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	projectsDir := filepath.Join(home, ".claude", "projects")
	fileName := sessionID + ".jsonl"

	// Try current browseDir first (fast path)
	fileMutex.RLock()
	dir := browseDir
	fileMutex.RUnlock()
	candidate := filepath.Join(projectsDir, encodeProjectDir(dir), fileName)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}

	// Scan all project directories
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate = filepath.Join(projectsDir, entry.Name(), fileName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func isSystemNoise(text string) bool {
	return strings.Contains(text, "<local-command-caveat>") ||
		strings.Contains(text, "<command-name>") ||
		strings.Contains(text, "<local-command-stdout>") ||
		strings.HasPrefix(strings.TrimSpace(text), "[Request interrupted")
}

// extractSessionSummary reads the first real user message from a transcript JSONL.
// Skips system caveats, slash commands, and empty messages. Returns truncated text.
func extractSessionSummary(transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	for i := 0; i < 50; i++ {
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			break
		}
		if s := extractSummaryFromRaw(raw); s != "" {
			return s
		}
	}
	return ""
}

// extractSummaryFromRaw extracts a user summary from a single transcript JSON
// line. Accepts Claude lines (type "user") and pi entries (type "message" with
// message.role "user").
func extractSummaryFromRaw(raw json.RawMessage) string {
	var entry struct {
		Type    string `json:"type"`
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &entry) != nil {
		return ""
	}
	if entry.Type != "user" && !(entry.Type == "message" && entry.Message.Role == "user") {
		return ""
	}
	text := extractUserText(entry.Message.Content)
	if text == "" || isSystemNoise(text) {
		return ""
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "Implement the following plan") {
		if title := extractPlanTitle(text); title != "" {
			return truncateString(title, 120)
		}
	}
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = text[:idx]
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return truncateString(text, 120)
}

func extractPlanTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			title := strings.TrimPrefix(line, "# ")
			// Strip "Plan: " prefix if present
			title = strings.TrimPrefix(title, "Plan: ")
			return title
		}
	}
	return ""
}

func extractUserText(content json.RawMessage) string {
	// Try string content first
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	// Try array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}

// pairToolResults attaches tool_result blocks to their matching tool_use blocks.
// Paired results are set on the tool_use's Result field and removed from the block list.
func pairToolResults(turns []transcriptTurn) []transcriptTurn {
	// Pass 1: index tool_use blocks by ToolID
	useIndex := make(map[string]*contentBlock)
	for i := range turns {
		for j := range turns[i].Blocks {
			if turns[i].Blocks[j].Type == "tool_use" && turns[i].Blocks[j].ToolID != "" {
				useIndex[turns[i].Blocks[j].ToolID] = &turns[i].Blocks[j]
			}
		}
	}

	// Pass 2: pair tool_results and remove from block lists
	for i := range turns {
		filtered := turns[i].Blocks[:0]
		for j := range turns[i].Blocks {
			b := &turns[i].Blocks[j]
			if b.Type == "tool_result" && b.ToolID != "" {
				if use, ok := useIndex[b.ToolID]; ok {
					use.Result = b
					continue // remove from block list
				}
			}
			filtered = append(filtered, *b)
		}
		turns[i].Blocks = filtered
	}
	return turns
}

// mergeConsecutiveTurns combines adjacent turns with the same role into one turn.
func mergeConsecutiveTurns(turns []transcriptTurn) []transcriptTurn {
	if len(turns) == 0 {
		return turns
	}
	merged := []transcriptTurn{turns[0]}
	for i := 1; i < len(turns); i++ {
		last := &merged[len(merged)-1]
		if turns[i].Role == last.Role {
			last.Blocks = append(last.Blocks, turns[i].Blocks...)
		} else {
			merged = append(merged, turns[i])
		}
	}
	return merged
}

// removeEmptyTurns filters out turns with no content blocks.
func removeEmptyTurns(turns []transcriptTurn) []transcriptTurn {
	filtered := turns[:0]
	for _, t := range turns {
		if len(t.Blocks) > 0 {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

// parseTranscript reads a Claude Code transcript JSONL file and returns conversation turns
func parseTranscript(path string) ([]transcriptTurn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	md := newSafeMarkdownRenderer()
	scanner := newJSONLScanner(f)

	var turns []transcriptTurn
	isFirstUser := true

	for scanner.Scan() {
		collapseCtx := isFirstUser
		turn, skip := parseTranscriptLine(scanner.Bytes(), md, collapseCtx)
		if skip {
			continue
		}
		if collapseCtx {
			isFirstUser = false
		}
		turns = append(turns, turn)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	turns = pairToolResults(turns)
	turns = removeEmptyTurns(turns)
	turns = mergeConsecutiveTurns(turns)
	turns = markTurnCollapsible(turns)
	turns = expandFinalTurn(turns)
	return turns, nil
}

// transcriptLineEnvelope is the minimal structure for a transcript JSONL line
type transcriptLineEnvelope struct {
	Type      string          `json:"type"`
	IsMeta    bool            `json:"isMeta"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

// transcriptMsg is the message body within a transcript line
type transcriptMsg struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

// parseTranscriptLine parses a single JSONL line into a transcriptTurn.
// Returns (turn, skip). If skip is true, the line should be ignored.
func parseTranscriptLine(line []byte, md goldmark.Markdown, collapseToolResults bool) (transcriptTurn, bool) {
	var env transcriptLineEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return transcriptTurn{}, true
	}

	// Only keep user/assistant, skip meta
	if env.IsMeta || (env.Type != "user" && env.Type != "assistant") {
		return transcriptTurn{}, true
	}
	if len(env.Message) == 0 {
		return transcriptTurn{}, true
	}

	var msg transcriptMsg
	if err := json.Unmarshal(env.Message, &msg); err != nil {
		return transcriptTurn{}, true
	}

	blocks := parseContentBlocks(msg.Content, md, collapseToolResults && msg.Role == "user", msg.Role == "assistant")
	if len(blocks) == 0 {
		return transcriptTurn{}, true
	}

	return transcriptTurn{
		Role:      msg.Role,
		Blocks:    blocks,
		Model:     msg.Model,
		Timestamp: env.Timestamp,
	}, false
}

// parseContentBlocks extracts content blocks from a message's content field
func parseContentBlocks(raw json.RawMessage, md goldmark.Markdown, collapseToolResults, assistant bool) []contentBlock {
	// Content can be a string (user prompt) or array of blocks
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if str == "" {
			return nil
		}
		return []contentBlock{newTextBlock(md, str, assistant)}
	}

	var rawBlocks []json.RawMessage
	if err := json.Unmarshal(raw, &rawBlocks); err != nil {
		return nil
	}

	// Count tool_results for context collapse
	if collapseToolResults {
		if n := countToolResults(rawBlocks); n > 10 {
			return []contentBlock{{Type: "context_summary", ItemCount: n}}
		}
	}

	return convertRawBlocks(rawBlocks, md, assistant)
}

// countToolResults counts tool_result blocks in a raw block list
func countToolResults(rawBlocks []json.RawMessage) int {
	count := 0
	for _, rb := range rawBlocks {
		var peek struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(rb, &peek) == nil && peek.Type == "tool_result" {
			count++
		}
	}
	return count
}

// rawContentBlock matches the JSON structure of a Claude message content block
type rawContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
}

// convertRawBlocks parses raw JSON blocks into contentBlock values
func convertRawBlocks(rawBlocks []json.RawMessage, md goldmark.Markdown, assistant bool) []contentBlock {
	var blocks []contentBlock
	for _, rb := range rawBlocks {
		var peek rawContentBlock
		if json.Unmarshal(rb, &peek) != nil {
			continue
		}
		switch peek.Type {
		case "text":
			if peek.Text != "" {
				blocks = append(blocks, newTextBlock(md, peek.Text, assistant))
			}
		case "thinking":
			if peek.Thinking != "" {
				blocks = append(blocks, newThinkingBlock(peek.Thinking))
			}
		case "tool_use":
			displayName, server := humanizeToolName(peek.Name)
			inputMap := parseToolInput(peek.Input)
			summary := toolSummaryFromMap(peek.Name, inputMap)
			structuredHTML := formatStructuredFromMap(peek.Name, inputMap)
			blocks = append(blocks, contentBlock{
				Type:            "tool_use",
				ToolName:        peek.Name,
				ToolInput:       formatToolInputFromMap(inputMap, peek.Input),
				ToolID:          peek.ID,
				ToolDisplayName: displayName,
				ToolServer:      server,
				ToolSummary:     summary,
				ToolInputHTML:   structuredHTML,
			})
		case "tool_result":
			text, images := extractToolResultContent(peek.Content)
			if text != "" || len(images) > 0 {
				blocks = append(blocks, newToolResultBlock(md, peek.ToolUseID, text, images))
			}
		}
	}
	return blocks
}

// newJSONLScanner returns a line scanner sized for transcript records.
func newJSONLScanner(f *os.File) *bufio.Scanner {
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // 10MB max line
	return scanner
}

// newTextBlock builds a rendered text block with size accounting and the
// role-appropriate collapse threshold.
func newTextBlock(md goldmark.Markdown, text string, assistant bool) contentBlock {
	block := contentBlock{Type: "text", HTML: renderMarkdownToHTML(md, text)}
	recordTextSize(&block, text)
	markCollapsible(&block, text, assistant)
	return block
}

func newThinkingBlock(text string) contentBlock {
	return contentBlock{Type: "thinking", Text: truncateString(text, 20000)}
}

// toolResultMaxChars caps rendered tool output; results collapse behind the
// tool call row, so this only bounds pathological outputs.
const toolResultMaxChars = 8000

func newToolResultBlock(md goldmark.Markdown, toolID, text string, images []imageData) contentBlock {
	block := contentBlock{Type: "tool_result", ToolID: toolID, Images: images}
	if text != "" {
		block.HTML = renderMarkdownToHTML(md, truncateString(text, toolResultMaxChars))
	}
	return block
}

// renderMarkdownToHTML converts markdown text to HTML using goldmark
func renderMarkdownToHTML(md goldmark.Markdown, text string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(text), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(text))
	}
	return template.HTML(buf.String())
}

// parseToolInput unmarshals tool input JSON once for reuse across summary/structured/raw formatters.
func parseToolInput(input json.RawMessage) map[string]interface{} {
	if len(input) == 0 {
		return nil
	}
	var m map[string]interface{}
	if json.Unmarshal(input, &m) != nil {
		return nil
	}
	return m
}

// formatToolInputFromMap pretty-prints tool input JSON, truncated to a reasonable size.
func formatToolInputFromMap(m map[string]interface{}, raw json.RawMessage) string {
	if m == nil {
		return truncateString(string(raw), 2000)
	}
	pretty, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return truncateString(string(raw), 2000)
	}
	return truncateString(string(pretty), 2000)
}

// humanizeToolName splits MCP tool names (mcp__server__action) into display name and server.
func humanizeToolName(name string) (string, string) {
	if strings.HasPrefix(name, "mcp__") {
		parts := strings.SplitN(name[5:], "__", 2)
		if len(parts) == 2 {
			return parts[1], parts[0]
		}
	}
	return name, ""
}

// toolIcon returns a Unicode icon for common tool names.
func toolIcon(name string) string {
	switch name {
	case "Bash":
		return "\u25B6" // ▶
	case "Read":
		return "\u2630" // ☰
	case "Edit", "MultiEdit":
		return "\u270E" // ✎
	case "Write":
		return "\u2714" // ✔
	case "Glob":
		return "\u2026" // …
	case "Grep":
		return "\u2315" // ⌕
	case "WebFetch", "WebSearch":
		return "\u2197" // ↗
	case "TaskCreate":
		return "\u002B" // +
	case "TaskUpdate":
		return "\u2611" // ☑
	case "TaskList", "TaskGet":
		return "\u2610" // ☐
	case "NotebookEdit":
		return "\u2338" // ⌸
	case "Agent":
		return "\u21BB" // ↻
	default:
		return "\u2699" // ⚙
	}
}

// toolInputStr extracts a string value from a parsed tool input map.
func toolInputStr(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// esc is a package-level alias for template.HTMLEscapeString, reducing repetition
// and the risk of forgetting to escape in HTML-building helpers.
var esc = template.HTMLEscapeString

// formatStructuredFromMap returns a structured HTML rendering for known tools.
// Returns empty HTML for unknown tools (template falls back to raw JSON).
func formatStructuredFromMap(toolName string, m map[string]interface{}) template.HTML {
	if m == nil {
		return ""
	}
	switch toolName {
	case "Bash":
		return formatBashInput(m)
	case "Glob":
		if pat := toolInputStr(m, "pattern"); pat != "" {
			return template.HTML(`<code class="transcript-structured-input">` + esc(pat) + `</code>`)
		}
	case "Read":
		return formatReadInput(m)
	case "Edit":
		return formatEditInput(m)
	case "MultiEdit":
		return formatMultiEditInput(m)
	case "Write":
		return formatWriteInput(m)
	case "Grep":
		return formatGrepInput(m)
	case "TaskCreate", "TaskUpdate", "WebSearch", "WebFetch", "NotebookEdit":
		return formatMiscToolInput(toolName, m)
	}
	return ""
}

// toolSummaryFromMap returns a short plain-text preview for the tool call summary line.
func toolSummaryFromMap(toolName string, m map[string]interface{}) string {
	if m == nil {
		return ""
	}
	switch toolName {
	case "Bash":
		return bashSummary(m)
	case "Read", "Edit", "MultiEdit", "Write":
		return toolInputStr(m, "file_path")
	case "Grep":
		return grepSummary(m)
	case "Glob":
		return toolInputStr(m, "pattern")
	case "Agent":
		return toolInputStr(m, "description")
	case "WebSearch":
		return toolInputStr(m, "query")
	case "WebFetch":
		return truncateString(toolInputStr(m, "url"), 80)
	case "TaskCreate":
		return toolInputStr(m, "subject")
	case "NotebookEdit":
		return toolInputStr(m, "notebook_path")
	}
	return ""
}

func bashSummary(m map[string]interface{}) string {
	cmd := truncateString(toolInputStr(m, "command"), 80)
	if desc := toolInputStr(m, "description"); desc != "" {
		return desc + "\n$ " + cmd
	}
	return "$ " + cmd
}

func grepSummary(m map[string]interface{}) string {
	s := "/" + toolInputStr(m, "pattern") + "/"
	if p := toolInputStr(m, "path"); p != "" {
		s += " in " + filepath.Base(p)
	}
	return s
}

func formatBashInput(m map[string]interface{}) template.HTML {
	cmd := toolInputStr(m, "command")
	if cmd == "" {
		return ""
	}
	var b strings.Builder
	if desc := toolInputStr(m, "description"); desc != "" {
		b.WriteString(`<div class="transcript-bash-description">`)
		b.WriteString(esc(desc))
		b.WriteString(`</div>`)
	}
	b.WriteString(`<pre class="transcript-structured-input transcript-bash-input"><code>$ `)
	b.WriteString(esc(cmd))
	b.WriteString(`</code></pre>`)
	return template.HTML(b.String())
}

func formatMiscToolInput(toolName string, m map[string]interface{}) template.HTML {
	switch toolName {
	case "TaskCreate":
		if subject := toolInputStr(m, "subject"); subject != "" {
			return template.HTML(`<span class="transcript-structured-input">` + esc(subject) + `</span>`)
		}
	case "TaskUpdate":
		id := toolInputStr(m, "taskId")
		status := toolInputStr(m, "status")
		if id != "" && status != "" {
			return template.HTML(`<span class="transcript-structured-input">#` + esc(id) + ` &#x2192; ` + esc(status) + `</span>`)
		}
	case "WebSearch":
		if query := toolInputStr(m, "query"); query != "" {
			return template.HTML(`<code class="transcript-structured-input">` + esc(query) + `</code>`)
		}
	case "WebFetch":
		if u := toolInputStr(m, "url"); u != "" {
			return template.HTML(`<code class="transcript-structured-input">` + esc(truncateString(u, 100)) + `</code>`)
		}
	case "NotebookEdit":
		if path := toolInputStr(m, "notebook_path"); path != "" {
			return template.HTML(`<span class="transcript-structured-input">` + esc(filepath.Base(path)) + `</span>`)
		}
	}
	return ""
}

func formatReadInput(m map[string]interface{}) template.HTML {
	fp := toolInputStr(m, "file_path")
	if fp == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<span class="transcript-structured-input" title="`)
	b.WriteString(esc(fp))
	b.WriteString(`">`)
	b.WriteString(esc(filepath.Base(fp)))
	if offset := toolInputStr(m, "offset"); offset != "" {
		b.WriteString(`<span class="transcript-structured-range">:`)
		b.WriteString(esc(offset))
		if limit := toolInputStr(m, "limit"); limit != "" {
			b.WriteByte('-')
			b.WriteString(esc(limit))
		}
		b.WriteString(`</span>`)
	}
	b.WriteString(`</span>`)
	return template.HTML(b.String())
}

func formatEditInput(m map[string]interface{}) template.HTML {
	fp := toolInputStr(m, "file_path")
	if fp == "" {
		return ""
	}
	var b strings.Builder
	writeFileLabel(&b, fp, "")
	b.WriteString(lineDiffHTML(toolInputStr(m, "old_string"), toolInputStr(m, "new_string"), diffMaxLines))
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

func formatMultiEditInput(m map[string]interface{}) template.HTML {
	fp := toolInputStr(m, "file_path")
	if fp == "" {
		return ""
	}
	edits, ok := m["edits"].([]interface{})
	if !ok || len(edits) == 0 {
		return formatEditInput(m)
	}
	var b strings.Builder
	writeFileLabel(&b, fp, fmt.Sprintf("%d edits", len(edits)))
	for _, e := range edits {
		em, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		b.WriteString(lineDiffHTML(toolInputStr(em, "old_string"), toolInputStr(em, "new_string"), diffMaxLines))
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

func formatWriteInput(m map[string]interface{}) template.HTML {
	fp := toolInputStr(m, "file_path")
	if fp == "" {
		return ""
	}
	content := toolInputStr(m, "content")
	lines := strings.Count(content, "\n") + 1
	label := fmt.Sprintf("%d line", lines)
	if lines != 1 {
		label += "s"
	}
	var b strings.Builder
	writeFileLabel(&b, fp, label)
	b.WriteString(lineDiffHTML("", content, diffMaxLines))
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

// writeFileLabel opens a structured-input container with a file-name badge and
// an optional muted meta note (e.g. edit count or line count). Caller closes </div>.
func writeFileLabel(b *strings.Builder, fp, meta string) {
	b.WriteString(`<div class="transcript-structured-input" title="`)
	b.WriteString(esc(fp))
	b.WriteString(`"><span>`)
	b.WriteString(esc(filepath.Base(fp)))
	b.WriteString(`</span>`)
	if meta != "" {
		b.WriteString(` <span class="transcript-structured-range">(` + esc(meta) + `)</span>`)
	}
}

// diffMaxLines caps rendered diff lines per hunk; the whole tool call already
// sits behind a collapsed <details>, so this only guards against pathological
// full-file writes, not routine edits.
const diffMaxLines = 60

// lineDiffHTML renders a line-level red/green diff preserving shared context,
// so the shape of a change is visible at a glance. Empty when both sides blank.
func lineDiffHTML(oldStr, newStr string, maxLines int) string {
	if oldStr == "" && newStr == "" {
		return ""
	}
	old := splitDiffLines(oldStr)
	neu := splitDiffLines(newStr)
	ops := diffOps(old, neu)

	var b strings.Builder
	b.WriteString(`<pre class="transcript-mini-diff">`)
	for i, op := range ops {
		if i >= maxLines {
			b.WriteString(fmt.Sprintf(`<span class="diff-meta">… %d more %s</span>`,
				len(ops)-i, plural(len(ops)-i, "line")))
			break
		}
		switch op.kind {
		case '-':
			b.WriteString(`<span class="diff-remove">- ` + esc(op.text) + `</span>`)
		case '+':
			b.WriteString(`<span class="diff-add">+ ` + esc(op.text) + `</span>`)
		default:
			b.WriteString(`<span class="diff-context">  ` + esc(op.text) + `</span>`)
		}
	}
	b.WriteString(`</pre>`)
	return b.String()
}

func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

type diffOp struct {
	kind byte // ' ' context, '-' removed, '+' added
	text string
}

// diffOps computes a line-level diff via LCS, keeping unchanged lines as
// context. For pathologically large inputs it degrades to remove-then-add
// rather than allocating an O(n*m) table.
func diffOps(a, b []string) []diffOp {
	if len(a) == 0 || len(b) == 0 || len(a)*len(b) > 250_000 {
		return flatDiff(a, b)
	}
	return lcsDiff(a, b)
}

// flatDiff renders every line of a as removed then every line of b as added.
func flatDiff(a, b []string) []diffOp {
	ops := make([]diffOp, 0, len(a)+len(b))
	for _, l := range a {
		ops = append(ops, diffOp{'-', l})
	}
	for _, l := range b {
		ops = append(ops, diffOp{'+', l})
	}
	return ops
}

// lcsDiff builds an LCS table and backtracks it into context/-/+ ops.
func lcsDiff(a, b []string) []diffOp {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	ops = append(ops, flatDiff(a[i:], b[j:])...)
	return ops
}

func formatGrepInput(m map[string]interface{}) template.HTML {
	pat := toolInputStr(m, "pattern")
	if pat == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<span class="transcript-structured-input"><code>/`)
	b.WriteString(esc(pat))
	b.WriteString(`/</code>`)
	if p := toolInputStr(m, "path"); p != "" {
		b.WriteString(` in <span title="`)
		b.WriteString(esc(p))
		b.WriteString(`">`)
		b.WriteString(esc(filepath.Base(p)))
		b.WriteString(`</span>`)
	}
	b.WriteString(`</span>`)
	return template.HTML(b.String())
}

// extractToolResultContent extracts text and images from a tool_result content field.
// Content can be a plain string or an array of typed content blocks.
func extractToolResultContent(content json.RawMessage) (string, []imageData) {
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
	if json.Unmarshal(content, &parts) == nil {
		var buf strings.Builder
		var images []imageData
		for _, p := range parts {
			switch p.Type {
			case "text":
				buf.WriteString(p.Text)
			case "image":
				if p.Source.Type == "base64" && p.Source.MediaType != "" && p.Source.Data != "" {
					images = append(images, imageData{MediaType: p.Source.MediaType, Data: p.Source.Data})
				}
			}
		}
		return buf.String(), images
	}
	return "", nil
}

// markCollapsible flags text blocks that render clamped with an expand toggle.
// Assistant prose is the payload readers skim for, so it gets a higher trigger
// than pasted user logs; the line-count check catches tall structured markdown
// (tables, lists) that a char threshold misses.
func markCollapsible(block *contentBlock, rawText string, assistant bool) {
	long := len(rawText) > 1500
	if assistant {
		long = len(rawText) > 2500 || strings.Count(rawText, "\n") > 30
	}
	if long {
		block.Collapsible = true
		block.LineCount = strings.Count(rawText, "\n") + 1
	}
}

func recordTextSize(block *contentBlock, rawText string) {
	block.textChars = len(rawText)
	block.textLines = strings.Count(rawText, "\n") + 1
}

// markTurnCollapsible clamps turns whose accumulated content renders tall.
// Merged turns stitch many short text blocks and tool rows together, so
// per-block thresholds never fire even when the turn dominates the page;
// collapsed tool and thinking rows count ~2 rendered lines each.
func markTurnCollapsible(turns []transcriptTurn) []transcriptTurn {
	for i := range turns {
		// A turn that is a single already-collapsible text block is handled by that
		// block's own toggle; a turn-level clamp on top would render a second toggle
		// that can't release the inner clamp (they bind to different containers).
		if len(turns[i].Blocks) == 1 && turns[i].Blocks[0].Collapsible {
			continue
		}
		chars, lines := 0, 0
		for _, b := range turns[i].Blocks {
			switch b.Type {
			case "text":
				chars += b.textChars
				lines += b.textLines
			case "tool_use", "thinking":
				lines += 2
			}
		}
		long := chars > 1500 || lines > 30
		if turns[i].Role == "assistant" {
			long = chars > 2500 || lines > 30
		}
		if long {
			turns[i].Collapsible = true
			turns[i].LineCount = lines
		}
	}
	return turns
}

// expandFinalTurn keeps the conversation's live tail fully visible: the last
// turn, and — when the session ends on a user message awaiting a reply — the
// assistant turn before it. That tail is the hinge the reply composer answers;
// collapsing it would hide what the reader is responding to.
func expandFinalTurn(turns []transcriptTurn) []transcriptTurn {
	n := len(turns)
	if n == 0 {
		return turns
	}
	expand := func(t *transcriptTurn) {
		t.Collapsible = false
		for i := range t.Blocks {
			t.Blocks[i].Collapsible = false
		}
	}
	expand(&turns[n-1])
	if turns[n-1].Role == "user" && n > 1 {
		expand(&turns[n-2])
	}
	return turns
}

// truncateString truncates a string to maxLen runes, adding "..." if truncated.
func truncateString(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	i := 0
	for n := 0; n < maxLen; n++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i] + "..."
}

// serveTranscript handles GET /transcript?session=<id>
func serveTranscript(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		http.Redirect(w, r, "/timeline", http.StatusFound)
		return
	}

	fileMutex.RLock()
	currentBrowseDir := browseDir
	fileMutex.RUnlock()

	path, source := resolveTranscriptPath(sessionID)

	data := transcriptTemplateData{
		baseTemplateData: newBaseTemplateData(),
		TreeHTML:         template.HTML(sidebarTreeHTML(r)),
		Title:            "Transcript",
		Subtitle:         "Session " + truncateSessionID(sessionID),
		BrowsePath:       currentBrowseDir,
		SessionID:        sessionID,
	}

	if path == "" {
		data.NotFound = true
	} else if turns, err := parseTranscriptFor(path, source); err != nil {
		data.NotFound = true
	} else {
		data.Turns = turns
		model := ""
		for _, t := range turns {
			if t.Role == "assistant" && t.Model != "" {
				model = t.Model
				break
			}
		}
		if model != "" {
			data.Subtitle = fmt.Sprintf("Session %s · %s · %d turns", truncateSessionID(sessionID), model, len(turns))
		} else {
			data.Subtitle = fmt.Sprintf("Session %s · %d turns", truncateSessionID(sessionID), len(turns))
		}
	}

	// Compute edit stats from event log (skip for not-found transcripts)
	if !data.NotFound && globalEventLog != nil {
		var sessionEvents []SessionEvent
		for _, evt := range globalEventLog.eventsForDir(currentBrowseDir) {
			if evt.SessionID == sessionID {
				sessionEvents = append(sessionEvents, evt)
			}
		}
		data.SessionStats = computeSessionStats(sessionEvents)
	}

	renderTemplatePair(w, r, transcriptTmpl, transcriptPartialTmpl, data)
}
