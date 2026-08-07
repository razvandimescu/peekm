// Package transcript parses AI coding-agent session logs — Claude Code and
// pi — into one neutral data model. The model is pure data: no markdown
// rendering, truncation, or layout decisions, and it marshals to stable JSON
// (see Version) so it can be serialized for other consumers.
package transcript

import (
	"os"
	"path/filepath"
	"strings"
)

// maxLineBytes caps a single JSONL line (tool results can embed large output).
const maxLineBytes = 10 * 1024 * 1024

// ParseFile parses a session file, detecting the harness by content: pi
// session files carry a {"type":"session"} header line, Claude Code
// transcripts never do.
func ParseFile(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if IsPiSessionFile(path) {
		return ParsePi(f)
	}
	sess, err := ParseClaude(f)
	if err != nil {
		return nil, err
	}
	// Claude stores no session ID inside the transcript; it is the file name.
	sess.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	return sess, nil
}
