package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/razvandimescu/peekm/transcript"
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
	CanReply     bool // reply composer resumes via claude; Claude sessions only
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

// contentBlock represents a piece of content within a turn
type contentBlock struct {
	Type            string             // "text", "tool_use", "tool_result", "thinking", "context_summary"
	HTML            template.HTML      // rendered markdown (for text blocks)
	Text            string             // raw text (for thinking, tool input)
	Collapsible     bool               // long user text blocks get fade-out + expand button
	LineCount       int                // line count hint for collapsible blocks
	ToolName        string             // for tool_use blocks
	ToolInput       string             // pretty-printed, truncated
	ToolID          string             // anchor id for transcript deep links
	Result          *contentBlock      // paired tool_result (nil if unpaired)
	ToolDisplayName string             // humanized name
	ToolServer      string             // MCP server prefix
	ToolSummary     string             // short preview for collapsed summary line
	ToolInputHTML   template.HTML      // structured rendering
	ItemCount       int                // for context_summary
	Images          []transcript.Image // for tool_result blocks containing images
	textChars       int                // raw text size, for turn-level collapse accounting
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
// "" for Claude Code and transcript.HarnessPi for pi, so callers can dispatch
// on harness without re-inferring it from file content.
func resolveTranscriptPath(sessionID string) (path, source string) {
	if p := resolveClaudeTranscriptPath(sessionID); p != "" {
		return p, ""
	}
	if p := resolvePiTranscriptPath(sessionID); p != "" {
		return p, transcript.HarnessPi
	}
	return "", ""
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
	probe := func(dir string) string {
		candidate := filepath.Join(dir, fileName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		return ""
	}
	return findInSessionStore(projectsDir, encodeProjectDir(currentBrowseDir()), probe)
}

// currentBrowseDir reads the browse dir under the file mutex.
func currentBrowseDir() string {
	fileMutex.RLock()
	defer fileMutex.RUnlock()
	return browseDir
}

// findInSessionStore probes the browse dir's store folder first (fast path),
// then every folder under root, returning probe's first hit.
func findInSessionStore(root, fastDirName string, probe func(dir string) string) string {
	if p := probe(filepath.Join(root, fastDirName)); p != "" {
		return p
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if p := probe(filepath.Join(root, entry.Name())); p != "" {
			return p
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

// parseTranscript reads a session transcript (Claude Code or pi, detected by
// content) and renders it into view-model turns.
func parseTranscript(path string) ([]transcriptTurn, error) {
	sess, err := transcript.ParseFile(path)
	if err != nil {
		return nil, err
	}
	return renderSessionTurns(sess), nil
}

// renderSessionTurns maps the neutral transcript model onto the view model:
// markdown to HTML, tool formatters, and collapse heuristics.
func renderSessionTurns(sess *transcript.Session) []transcriptTurn {
	md := newSafeMarkdownRenderer()
	turns := make([]transcriptTurn, 0, len(sess.Turns))
	for i, t := range sess.Turns {
		turns = append(turns, renderTurn(t, md, i == 0))
	}
	turns = markTurnCollapsible(turns)
	return expandFinalTurn(turns)
}

// renderTurn converts one model turn. A first user turn opening with a long
// run of unpaired tool results is a session resumed from compacted context;
// those results render as a single context_summary marker.
func renderTurn(t transcript.Turn, md goldmark.Markdown, first bool) transcriptTurn {
	vt := transcriptTurn{Role: t.Role, Model: t.Model}
	if !t.Timestamp.IsZero() {
		vt.Timestamp = t.Timestamp.Format(time.RFC3339)
	}
	collapsed := 0
	if first && t.Role == "user" {
		if n := countOrphanResults(t.Blocks); n > 10 {
			collapsed = n
			vt.Blocks = append(vt.Blocks, contentBlock{Type: "context_summary", ItemCount: n})
		}
	}
	for _, b := range t.Blocks {
		if collapsed > 0 && b.Kind == transcript.KindToolResult {
			continue
		}
		if cb, ok := renderBlock(b, md, t.Role == "assistant"); ok {
			vt.Blocks = append(vt.Blocks, cb)
		}
	}
	return vt
}

// countOrphanResults counts tool results whose call is not in the session.
func countOrphanResults(blocks []transcript.Block) int {
	n := 0
	for _, b := range blocks {
		if b.Kind == transcript.KindToolResult {
			n++
		}
	}
	return n
}

func renderBlock(b transcript.Block, md goldmark.Markdown, assistant bool) (contentBlock, bool) {
	switch b.Kind {
	case transcript.KindText:
		return renderTextBlock(b.Text, md, assistant), true
	case transcript.KindThinking:
		return contentBlock{Type: "thinking", Text: truncateString(b.Text, 20000)}, true
	case transcript.KindToolCall:
		return renderToolCall(b.Tool, md), true
	case transcript.KindToolResult:
		return renderToolResult(b.Result, md), true
	}
	return contentBlock{}, false
}

func renderTextBlock(text string, md goldmark.Markdown, assistant bool) contentBlock {
	block := contentBlock{Type: "text", HTML: renderMarkdownToHTML(md, text)}
	recordTextSize(&block, text)
	markCollapsible(&block, text, assistant)
	return block
}

func renderToolCall(tc *transcript.ToolCall, md goldmark.Markdown) contentBlock {
	displayName, server := humanizeToolName(tc.Name)
	block := contentBlock{
		Type:            "tool_use",
		ToolName:        tc.Name,
		ToolID:          tc.ID,
		ToolDisplayName: displayName,
		ToolServer:      server,
		ToolInput:       formatToolInputFromMap(tc.Input, tc.RawInput),
		ToolSummary:     toolSummaryFromMap(tc.Name, tc.Input),
		ToolInputHTML:   formatStructuredFromMap(tc.Name, tc.Input),
	}
	if tc.Result != nil {
		result := renderToolResult(tc.Result, md)
		block.Result = &result
	}
	return block
}

// renderToolResult renders a tool result. Preformatted output (e.g. captured
// shell output) is fenced so it is not interpreted as markdown.
func renderToolResult(r *transcript.ToolResult, md goldmark.Markdown) contentBlock {
	block := contentBlock{Type: "tool_result", ToolID: r.CallID, Images: r.Images}
	if r.Text != "" {
		text := truncateString(r.Text, 8000)
		if r.Preformatted {
			fence := codeFence(text)
			text = fence + "\n" + text + "\n" + fence
		}
		block.HTML = renderMarkdownToHTML(md, text)
	}
	return block
}

// codeFence returns a backtick fence longer than any backtick run in s, so
// embedded ``` sequences cannot terminate it early (CommonMark honors the
// longer fence).
func codeFence(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

// renderMarkdownToHTML converts markdown text to HTML using goldmark
func renderMarkdownToHTML(md goldmark.Markdown, text string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(text), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(text))
	}
	return template.HTML(buf.String())
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

	path, source := resolveTranscriptPath(sessionID)

	data := transcriptTemplateData{
		baseTemplateData: newBaseTemplateData(),
		TreeHTML:         template.HTML(sidebarTreeHTML(r)),
		Title:            "Transcript",
		Subtitle:         "Session " + truncateSessionID(sessionID),
		BrowsePath:       currentBrowseDir(),
		SessionID:        sessionID,
	}

	if path == "" {
		data.NotFound = true
	} else if turns, err := parseTranscript(path); err != nil {
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
		for _, evt := range globalEventLog.eventsForDir(currentBrowseDir()) {
			if evt.SessionID == sessionID {
				sessionEvents = append(sessionEvents, evt)
			}
		}
		data.SessionStats = computeSessionStats(sessionEvents)
	}

	// The /reply endpoint resumes sessions via `claude` — pi sessions have no
	// resume path, so they get no composer.
	data.CanReply = !data.NotFound && source != transcript.HarnessPi

	renderTemplatePair(w, r, transcriptTmpl, transcriptPartialTmpl, data)
}
