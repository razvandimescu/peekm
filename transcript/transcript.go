// Package transcript parses AI coding-agent session logs — Claude Code and
// pi — into one neutral data model. The model is pure data: no markdown
// rendering, truncation, or layout decisions, and it marshals to stable JSON
// (see Version) so it can be serialized for other consumers.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxLineBytes caps a single JSONL line (tool results can embed large output).
const maxLineBytes = 10 * 1024 * 1024

// newLineScanner returns a JSONL scanner sized for large tool-result lines.
func newLineScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	return s
}

// ParseFile parses a session file, detecting the harness by content: pi
// session files carry a {"type":"session"} header line, Claude Code
// transcripts never do.
func ParseFile(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 64*1024)
	if sniffPiHeader(br) {
		return ParsePi(br)
	}
	sess, err := ParseClaude(br)
	if err != nil {
		return nil, err
	}
	// Claude stores no session ID inside the transcript; it is the file name.
	sess.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	return sess, nil
}

// sniffPiHeader peeks the first line without consuming it. A pi header always
// fits the peek window; an over-long first line is a Claude transcript.
func sniffPiHeader(r *bufio.Reader) bool {
	peeked, _ := r.Peek(4096)
	if i := bytes.IndexByte(peeked, '\n'); i >= 0 {
		peeked = peeked[:i]
	}
	var h piHeader
	return json.Unmarshal(peeked, &h) == nil && h.Type == "session"
}
