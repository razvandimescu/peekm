package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	sessionActiveThreshold = 5 * time.Minute
	monitorTickInterval    = 30 * time.Second
	summarizationTimeout   = 5 * time.Minute
	ollamaModel            = "qwen3.5:27b-q8_0"
	// Limit summary input to recent activity so multi-day sessions don't
	// blur distinct work periods together.
	summaryWindow = 24 * time.Hour
)

// Sentinel errors returned by generateSummary for expected skip paths.
// Real failures (read errors, ollama errors) are wrapped via fmt.Errorf.
var (
	errNoRecentActivity  = errors.New("no recent activity in window")
	errSummaryUpToDate   = errors.New("summary already covers latest activity")
	errInsufficientInput = errors.New("too few conversation lines")
)

const dayStartHour = 5 // sessions before 5am group with previous day

type summaryStore struct {
	mu         sync.RWMutex
	summaries  map[string]*sessionSummary
	daily      map[string]*dailySummary // key: "2026-03-31"
	filePath   string
	heartbeats *heartbeatStore
	prevActive map[string]bool
	running    atomic.Bool
}

type sessionSummary struct {
	Summary       string    `json:"summary"`
	Project       string    `json:"project"`
	Outcome       string    `json:"outcome"`                  // "completed" | "partial" | "blocked"
	Domain        string    `json:"domain"`                   // e.g. "bug fix", "feature", "refactor"
	FilesModified []string  `json:"files_modified,omitempty"` // basenames of edited files
	FilesExplored []string  `json:"files_explored,omitempty"` // basenames of read-only files
	ToolsUsed     []string  `json:"tools_used,omitempty"`     // deduplicated tool names
	StartedAt     string    `json:"started_at,omitempty"`     // RFC3339 from first transcript turn
	GeneratedAt   time.Time `json:"generated_at"`
}

type summaryMetadata struct {
	Formatted     string
	ConvLines     int
	FilesModified []string
	FilesExplored []string
	ToolsUsed     []string
}

type dailySummary struct {
	Summary     string    `json:"summary"`
	SessionIDs  []string  `json:"session_ids"`
	GeneratedAt time.Time `json:"generated_at"`
}

// summaryFile is the persisted JSON structure (supports migration from old flat format).
type summaryFile struct {
	Sessions map[string]*sessionSummary `json:"sessions"`
	Daily    map[string]*dailySummary   `json:"daily"`
}

type ollamaResult struct {
	Summary string
	Outcome string
	Domain  string
}

func newSummaryStore(hb *heartbeatStore) *summaryStore {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("Summary: cannot determine home dir: %v", err)
		return &summaryStore{
			summaries:  make(map[string]*sessionSummary),
			daily:      make(map[string]*dailySummary),
			heartbeats: hb,
			prevActive: hb.activeSessionIDs(sessionActiveThreshold),
		}
	}
	fp := filepath.Join(home, ".peekm", "summaries.json")
	ss := &summaryStore{
		summaries:  make(map[string]*sessionSummary),
		daily:      make(map[string]*dailySummary),
		filePath:   fp,
		heartbeats: hb,
		prevActive: make(map[string]bool),
	}
	ss.load()
	ss.cleanContaminatedSummaries()
	ss.prevActive = hb.activeSessionIDs(sessionActiveThreshold)
	log.Printf("Summary: loaded %d summaries from %s (%d active sessions)", len(ss.summaries), ss.filePath, len(ss.prevActive))
	return ss
}

func (ss *summaryStore) load() {
	data, err := os.ReadFile(ss.filePath)
	if err != nil {
		return
	}
	var sf summaryFile
	if json.Unmarshal(data, &sf) == nil && sf.Sessions != nil {
		ss.summaries = sf.Sessions
		if sf.Daily != nil {
			ss.daily = sf.Daily
		}
		return
	}
	// Fall back to old flat format (map[string]*sessionSummary)
	var m map[string]*sessionSummary
	if json.Unmarshal(data, &m) == nil {
		ss.summaries = m
	}
}

// cleanContaminatedSummaries strips thinking blocks from stored summaries
// and removes daily entries generated from contaminated input.
// Must be called before startMonitor (no concurrent access).
func (ss *summaryStore) cleanContaminatedSummaries() {
	dirty := false
	for sid, s := range ss.summaries {
		cleaned := stripThinkingBlock(s.Summary)
		if isContaminated(cleaned) {
			// Can't recover — mark for re-summarization by clearing
			log.Printf("Summary: clearing contaminated session %s", truncateSessionID(sid))
			s.Summary = ""
			dirty = true
		} else if cleaned != s.Summary {
			log.Printf("Summary: cleaned thinking block from session %s", truncateSessionID(sid))
			s.Summary = cleaned
			dirty = true
		}
	}
	for dateKey, d := range ss.daily {
		cleaned := stripThinkingBlock(d.Summary)
		if isContaminated(cleaned) || cleaned != d.Summary {
			log.Printf("Summary: removed contaminated daily digest for %s", dateKey)
			delete(ss.daily, dateKey)
			dirty = true
		}
	}
	if dirty {
		ss.save()
	}
}

func isContaminated(s string) bool {
	return s == "" || strings.HasPrefix(s, "Thinking")
}

func (ss *summaryStore) save() {
	ss.mu.RLock()
	sf := summaryFile{Sessions: ss.summaries, Daily: ss.daily}
	data, err := json.MarshalIndent(sf, "", "  ")
	ss.mu.RUnlock()
	if err != nil {
		log.Printf("Summary: cannot marshal summaries: %v", err)
		return
	}
	if err := atomicWriteFile(ss.filePath, string(data)); err != nil {
		log.Printf("Summary: cannot save %s: %v", ss.filePath, err)
	}
}

func (ss *summaryStore) get(sessionID string) (*sessionSummary, bool) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	s, ok := ss.summaries[sessionID]
	return s, ok
}

// getAll returns a snapshot of all summaries (single lock acquisition).
func (ss *summaryStore) getAll() map[string]*sessionSummary {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	m := make(map[string]*sessionSummary, len(ss.summaries))
	for k, v := range ss.summaries {
		m[k] = v
	}
	return m
}

func (ss *summaryStore) set(sessionID string, summary *sessionSummary) {
	ss.mu.Lock()
	ss.summaries[sessionID] = summary
	ss.mu.Unlock()
	ss.save()
}

func (ss *summaryStore) getAllDaily() map[string]*dailySummary {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	m := make(map[string]*dailySummary, len(ss.daily))
	for k, v := range ss.daily {
		m[k] = v
	}
	return m
}

func (ss *summaryStore) setDaily(dateKey string, d *dailySummary) {
	ss.mu.Lock()
	ss.daily[dateKey] = d
	ss.mu.Unlock()
	ss.save()
}

// effectiveDate shifts timestamps before dayStartHour to the previous day.
func effectiveDate(t time.Time) time.Time {
	t = t.Add(-time.Duration(dayStartHour) * time.Hour)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func effectiveDateKey(t time.Time) string {
	return effectiveDate(t).Format("2006-01-02")
}

// maybeGenerateDailySummary checks if the day has 2+ session summaries
// and generates a synthesized daily journal entry if needed.
func (ss *summaryStore) maybeGenerateDailySummary(sessionTime time.Time) {
	dateKey := effectiveDateKey(sessionTime)

	// Collect session summaries for this date
	ss.mu.RLock()
	var sessionIDs []string
	var sessionTexts []string
	for sid, s := range ss.summaries {
		if s.GeneratedAt.IsZero() || s.Summary == "" {
			continue
		}
		if effectiveDateKey(s.GeneratedAt) == dateKey {
			if isContaminated(s.Summary) {
				continue
			}
			sessionIDs = append(sessionIDs, sid)
			sessionTexts = append(sessionTexts, s.Summary)
		}
	}
	existing := ss.daily[dateKey]
	ss.mu.RUnlock()

	if len(sessionIDs) < 2 {
		return
	}

	sort.Strings(sessionIDs)
	if existing != nil && slices.Equal(existing.SessionIDs, sessionIDs) {
		return
	}

	var buf strings.Builder
	for i, text := range sessionTexts {
		buf.WriteString(fmt.Sprintf("Session %d: %s\n\n", i+1, text))
	}

	prompt := fmt.Sprintf(dailyPromptTemplate, len(sessionTexts), buf.String())

	ctx, cancel := context.WithTimeout(context.Background(), summarizationTimeout)
	defer cancel()

	log.Printf("Summary: generating daily digest for %s (%d sessions)", dateKey, len(sessionIDs))

	result, err := runOllamaSummarize(ctx, prompt)
	if err != nil {
		log.Printf("Summary: daily digest failed for %s: %v", dateKey, err)
		return
	}

	ss.setDaily(dateKey, &dailySummary{
		Summary:     result,
		SessionIDs:  sessionIDs,
		GeneratedAt: time.Now(),
	})
	log.Printf("Summary: daily digest generated for %s (%d chars)", dateKey, len(result))
}

func (hs *heartbeatStore) activeSessionIDs(threshold time.Duration) map[string]bool {
	now := time.Now()
	hs.mu.RLock()
	defer hs.mu.RUnlock()
	active := make(map[string]bool, len(hs.beats))
	for id, hb := range hs.beats {
		if now.Sub(hb.Timestamp) < threshold {
			active[id] = true
		}
	}
	return active
}

func (ss *summaryStore) startMonitor() {
	go func() {
		for range time.Tick(monitorTickInterval) {
			ss.tick()
		}
	}()
}

func (ss *summaryStore) tick() {
	currentActive := ss.heartbeats.activeSessionIDs(sessionActiveThreshold)

	ss.mu.Lock()
	var newlyInactive []string
	for sid := range ss.prevActive {
		if !currentActive[sid] {
			newlyInactive = append(newlyInactive, sid)
		}
	}
	ss.prevActive = currentActive
	ss.mu.Unlock()

	if len(newlyInactive) == 0 {
		return
	}

	if !ss.running.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer ss.running.Store(false)
		for _, sid := range newlyInactive {
			ss.summarizeSession(sid)
		}
	}()
}

func cwdForSession(sessionID string) string {
	if globalEventLog == nil {
		return ""
	}
	globalEventLog.mu.RLock()
	defer globalEventLog.mu.RUnlock()
	for _, evt := range globalEventLog.events {
		if evt.SessionID == sessionID && evt.CWD != "" {
			return evt.CWD
		}
	}
	return ""
}

func sessionInScopeWith(cwd string) bool {
	if cwd == "" {
		return true // no CWD info — allow (might be conversation-only)
	}
	fileMutex.RLock()
	dir := browseDir
	fileMutex.RUnlock()
	return cwd == dir || strings.HasPrefix(cwd, dir+string(filepath.Separator))
}

func (ss *summaryStore) summarizeSession(sessionID string) {
	cwd := cwdForSession(sessionID)
	if !sessionInScopeWith(cwd) {
		return
	}
	path := resolveTranscriptPath(sessionID)
	if path == "" {
		return
	}

	project := ""
	if e, ok := ss.get(sessionID); ok {
		project = e.Project
	}
	if project == "" && cwd != "" {
		project = filepath.Base(cwd)
	}

	switch err := ss.generateSummary(sessionID, project, path); {
	case err == nil:
		log.Printf("Summary: generated for %s [%s]", truncateSessionID(sessionID), project)
		ss.maybeGenerateDailySummary(time.Now())
	case errors.Is(err, errNoRecentActivity),
		errors.Is(err, errSummaryUpToDate),
		errors.Is(err, errInsufficientInput):
		// expected skip — silent in monitor mode
	default:
		log.Printf("Summary: failed for %s: %v", truncateSessionID(sessionID), err)
	}
}

// generateSummary runs the windowed summarization pipeline for one
// session and stores the result. Returns nil on success, sentinel
// errors for expected skips, or wrapped errors for real failures.
func (ss *summaryStore) generateSummary(sessionID, project, path string) error {
	existing, _ := ss.get(sessionID)

	// mtime gate: skip the entire pipeline if the transcript hasn't
	// been touched since we last summarized it.
	if existing != nil {
		if info, err := os.Stat(path); err == nil && existing.GeneratedAt.After(info.ModTime()) {
			return errSummaryUpToDate
		}
	}

	lines, _, err := readTranscriptLines(path, 0)
	if err != nil {
		return fmt.Errorf("read transcript: %w", err)
	}
	windowed := filterByTimeWindow(lines, time.Now(), summaryWindow)
	if len(windowed) == 0 {
		return errNoRecentActivity
	}

	meta := formatTranscriptForSummary(windowed)
	if meta.ConvLines < 5 || len(meta.Formatted) < 100 {
		return errInsufficientInput
	}
	prompt := buildSummaryPrompt(truncateBytes(meta.Formatted, 50000), "")

	ctx, cancel := context.WithTimeout(context.Background(), summarizationTimeout)
	defer cancel()

	raw, err := runOllamaSummarize(ctx, prompt)
	if err != nil {
		return fmt.Errorf("ollama: %w", err)
	}

	parsed := parseOllamaResult(raw)
	ss.set(sessionID, &sessionSummary{
		Summary:       parsed.Summary,
		Project:       project,
		Outcome:       parsed.Outcome,
		Domain:        parsed.Domain,
		FilesModified: meta.FilesModified,
		FilesExplored: meta.FilesExplored,
		ToolsUsed:     meta.ToolsUsed,
		StartedAt:     extractSessionStartTime(lines),
		GeneratedAt:   time.Now(),
	})
	return nil
}

func filterByTimeWindow(lines [][]byte, now time.Time, window time.Duration) [][]byte {
	cutoff := now.Add(-window)
	var filtered [][]byte
	for _, line := range lines {
		var env transcriptLineEnvelope
		if json.Unmarshal(line, &env) != nil || env.Timestamp == "" {
			continue
		}
		ts, ok := parseTimestamp(env.Timestamp)
		if !ok {
			continue
		}
		if ts.After(cutoff) {
			filtered = append(filtered, line)
		}
	}
	return filtered
}

func readTranscriptLines(path string, offset int) ([][]byte, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var lines [][]byte
	lineNum := 0
	for scanner.Scan() {
		if lineNum >= offset {
			line := make([]byte, len(scanner.Bytes()))
			copy(line, scanner.Bytes())
			lines = append(lines, line)
		}
		lineNum++
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, err
	}
	return lines, lineNum, nil
}

func extractSessionStartTime(jsonlLines [][]byte) string {
	for _, line := range jsonlLines {
		var env transcriptLineEnvelope
		if json.Unmarshal(line, &env) == nil && env.Timestamp != "" {
			return env.Timestamp
		}
	}
	return ""
}

// formatTranscriptForSummary preprocesses JSONL into a compact session outline
// and returns structured metadata (files, tools) alongside the formatted text.
func formatTranscriptForSummary(jsonlLines [][]byte) summaryMetadata {
	filesModified := make(map[string]bool)
	filesRead := make(map[string]bool)
	toolsUsed := make(map[string]bool)
	turns, convLines := extractSummaryTurns(jsonlLines, filesModified, filesRead, toolsUsed)

	modified := sortedKeys(filesModified)
	toolNames := sortedKeys(toolsUsed)

	// Build explored list excluding modified files (sorted, single pass)
	var explored []string
	for f := range filesRead {
		if !filesModified[f] {
			explored = append(explored, f)
		}
	}
	sort.Strings(explored)

	var buf strings.Builder
	if len(modified) > 0 {
		buf.WriteString("## Files modified\n")
		for _, f := range modified {
			buf.WriteString("- " + f + "\n")
		}
		buf.WriteString("\n")
	}
	if len(explored) > 0 {
		buf.WriteString("## Files explored\n")
		for _, f := range explored {
			buf.WriteString("- " + f + "\n")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("## Session\n\n")
	for _, t := range turns {
		buf.WriteString(t + "\n\n")
	}

	return summaryMetadata{
		Formatted:     buf.String(),
		ConvLines:     convLines,
		FilesModified: modified,
		FilesExplored: explored,
		ToolsUsed:     toolNames,
	}
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func extractSummaryTurns(jsonlLines [][]byte, filesModified, filesRead, toolsUsed map[string]bool) ([]string, int) {
	var turns []string
	convLines := 0

	for _, line := range jsonlLines {
		var env transcriptLineEnvelope
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		if env.IsMeta || (env.Type != "user" && env.Type != "assistant") {
			continue
		}
		if len(env.Message) == 0 {
			continue
		}
		var msg transcriptMsg
		if json.Unmarshal(env.Message, &msg) != nil {
			continue
		}
		convLines++

		switch msg.Role {
		case "user":
			text := extractUserText(msg.Content)
			if text != "" && !isSystemNoise(text) {
				turns = append(turns, "User: "+truncateString(text, 500))
			}
		case "assistant":
			if t := formatAssistantTurn(msg.Content, filesModified, filesRead, toolsUsed); t != "" {
				turns = append(turns, t)
			}
		}
	}
	return turns, convLines
}

func formatAssistantTurn(content json.RawMessage, filesModified, filesRead, toolsUsed map[string]bool) string {
	toolLine, conclusion := summarizeAssistantTurn(content, filesModified, filesRead, toolsUsed)
	if toolLine == "" && conclusion == "" {
		return ""
	}
	if toolLine != "" && conclusion != "" {
		return toolLine + "\n" + conclusion
	}
	if toolLine != "" {
		return toolLine
	}
	return conclusion
}

// summarizeAssistantTurn collapses an assistant turn into a tool summary line
// and the conclusion text. Populates filesModified/filesRead/toolsUsed sets.
func summarizeAssistantTurn(content json.RawMessage, filesModified, filesRead, toolsUsed map[string]bool) (toolLine, conclusion string) {
	var rawBlocks []json.RawMessage
	if json.Unmarshal(content, &rawBlocks) != nil {
		return "", ""
	}

	var tools []toolCall
	var lastText string
	lastTextAfterTool := false

	for _, rb := range rawBlocks {
		var block rawContentBlock
		if json.Unmarshal(rb, &block) != nil {
			continue
		}
		switch block.Type {
		case "text":
			if block.Text != "" {
				lastText = block.Text
				lastTextAfterTool = len(tools) > 0
			}
		case "tool_use":
			m := parseToolInput(block.Input)
			detail := toolSummaryFromMap(block.Name, m)
			tools = append(tools, toolCall{name: block.Name, detail: detail})
			toolsUsed[block.Name] = true
			fp := toolInputStr(m, "file_path")
			if fp != "" {
				short := filepath.Base(fp)
				switch block.Name {
				case "Edit", "Write", "NotebookEdit":
					filesModified[short] = true
				case "Read":
					filesRead[short] = true
				}
			}
		}
	}

	if len(tools) > 0 {
		toolLine = collapseToolCalls(tools)
	}

	if lastText != "" {
		if len(tools) == 0 || lastTextAfterTool {
			conclusion = truncateString(lastText, 500)
		}
	}
	return toolLine, conclusion
}

type toolCall struct {
	name   string
	detail string
}

func collapseToolCalls(tools []toolCall) string {
	type entry struct {
		key   string
		count int
	}
	var groups []entry
	seen := map[string]int{}

	for _, t := range tools {
		key := t.name
		if t.detail != "" {
			key += " " + t.detail
		}
		if idx, ok := seen[key]; ok {
			groups[idx].count++
		} else {
			seen[key] = len(groups)
			groups = append(groups, entry{key: key, count: 1})
		}
	}

	var parts []string
	for _, g := range groups {
		if g.count > 1 {
			parts = append(parts, fmt.Sprintf("%s ×%d", g.key, g.count))
		} else {
			parts = append(parts, g.key)
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// truncateBytes caps at maxBytes on a valid rune boundary.
func truncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes] + "\n[... truncated ...]"
}

const dailyPromptTemplate = `Synthesize these %d coding session summaries into a single daily journal entry.
Write 2-4 sentences covering the day's main accomplishments and status.
Be specific about files and features. No bullet points.
Do not mention "the user" or "the assistant".

%s`

const summaryPromptTemplate = `Summarize the following Claude Code session transcript concisely.

Focus on:
- What the user was trying to accomplish (the goal)
- What was actually done (files created/modified, key decisions)
- Current status (completed, in-progress, blocked)

Write 2-4 sentences. Be specific about file names and features. Do not use bullet points.
Do not mention "the user" or "the assistant" — describe what was done passively or imperatively.

After your summary, on a NEW line write exactly:
OUTCOME: completed OR partial OR blocked
DOMAIN: <short label, e.g. "bug fix", "feature", "refactor", "review", "config", "performance">

%s---TRANSCRIPT---
%s`

func buildSummaryPrompt(transcript, previousSummary string) string {
	contextSection := ""
	if previousSummary != "" {
		contextSection = fmt.Sprintf(
			"This is a CONTINUATION of a session previously summarized as:\n> %s\n\nSummarize the NEW activity below, incorporating the above context into a unified summary.\n\n",
			previousSummary,
		)
	}
	return fmt.Sprintf(summaryPromptTemplate, contextSection, transcript)
}

func runOllamaSummarize(ctx context.Context, prompt string) (string, error) {
	reqBody := struct {
		Model    string              `json:"model"`
		Stream   bool                `json:"stream"`
		Think    bool                `json:"think"`
		Messages []map[string]string `json:"messages"`
	}{
		Model:  ollamaModel,
		Stream: false,
		Think:  false,
		Messages: []map[string]string{
			{"role": "user", "content": prompt},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost:11434/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("timed out after %v", summarizationTimeout)
		}
		return "", fmt.Errorf("ollama API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama API %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var respData struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	result := strings.TrimSpace(respData.Message.Content)
	if result == "" {
		return "", fmt.Errorf("ollama returned empty response")
	}
	// Defensive: strip any residual thinking in case model ignores think:false
	result = stripThinkingBlock(result)
	return result, nil
}

// stripThinkingBlock removes Qwen's thinking output that leaks despite /no_think.
// Handles multiple marker formats: "...done thinking.", "</think>", and "Thinking..."
func stripThinkingBlock(s string) string {
	// Try end markers first (most reliable — everything after is the answer)
	for _, marker := range []string{"...done thinking.", "</think>"} {
		if idx := strings.Index(s, marker); idx >= 0 {
			return strings.TrimSpace(s[idx+len(marker):])
		}
	}
	// Try start marker — if "Thinking..." appears at the start, look for
	// the actual summary by finding the last paragraph (thinking tends to
	// be one giant block, the answer follows after a blank line)
	if strings.HasPrefix(s, "Thinking") {
		if idx := strings.LastIndex(s, "\n\n"); idx >= 0 {
			candidate := strings.TrimSpace(s[idx+2:])
			// Reject if it still looks like reasoning (numbered steps, bullet lists)
			if len(candidate) > 20 && !strings.HasPrefix(candidate, "*") && !strings.HasPrefix(candidate, "1.") {
				return candidate
			}
		}
	}
	return s
}

func parseOllamaResult(raw string) ollamaResult {
	result := ollamaResult{Outcome: "partial"} // default
	lines := strings.Split(raw, "\n")

	var summaryLines []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		upper := strings.ToUpper(trimmed)
		if strings.HasPrefix(upper, "OUTCOME:") {
			val := strings.TrimSpace(trimmed[len("OUTCOME:"):])
			val = strings.ToLower(val)
			switch val {
			case "completed", "partial", "blocked":
				result.Outcome = val
			}
			continue
		}
		if strings.HasPrefix(upper, "DOMAIN:") {
			result.Domain = strings.TrimSpace(trimmed[len("DOMAIN:"):])
			continue
		}
		summaryLines = append(summaryLines, line)
	}

	result.Summary = strings.TrimSpace(strings.Join(summaryLines, "\n"))
	return result
}

// runSummarize handles `peekm summarize <sessionID> [<sessionID>...]`
func runSummarize(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: peekm summarize <sessionID> [<sessionID>...]")
		os.Exit(1)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot determine home dir: %v\n", err)
		os.Exit(1)
	}

	fp := filepath.Join(home, ".peekm", "summaries.json")
	ss := &summaryStore{
		summaries: make(map[string]*sessionSummary),
		daily:     make(map[string]*dailySummary),
		filePath:  fp,
	}
	ss.load()

	for _, sid := range args {
		fmt.Fprintf(os.Stderr, "=== Session %s ===\n", truncateSessionID(sid))

		path := resolveTranscriptPath(sid)
		if path == "" {
			fmt.Fprintln(os.Stderr, "No transcript found")
			continue
		}

		project := ""
		if existing, ok := ss.get(sid); ok {
			project = existing.Project
		}
		if project == "" {
			// Derive from transcript path: ~/.claude/projects/{encoded-dir}/{sid}.jsonl
			dir := filepath.Base(filepath.Dir(path))
			parts := strings.Split(dir, "-")
			if len(parts) > 0 {
				project = parts[len(parts)-1]
			}
		}

		switch err := ss.generateSummary(sid, project, path); {
		case err == nil:
			s, _ := ss.get(sid)
			fmt.Fprintf(os.Stderr, "Done: outcome=%s domain=%q (%d chars)\n", s.Outcome, s.Domain, len(s.Summary))
			fmt.Println(s.Summary)
		case errors.Is(err, errNoRecentActivity):
			fmt.Fprintf(os.Stderr, "No activity in last %s\n", summaryWindow)
		case errors.Is(err, errSummaryUpToDate):
			fmt.Fprintln(os.Stderr, "Summary is already up to date")
		case errors.Is(err, errInsufficientInput):
			fmt.Fprintln(os.Stderr, "Too few conversation lines in window")
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
	}
}
