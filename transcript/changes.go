package transcript

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// FileChange is the net change a session made to one file: its content
// before the session's first edit and after its last one.
type FileChange struct {
	Path    string `json:"path"`
	Before  string `json:"before,omitempty"`
	After   string `json:"after,omitempty"`
	Created bool   `json:"created,omitempty"`
	// Partial marks a file whose content could not be rebuilt: no copy from
	// before the session, or it changed outside the recorded edits. Before
	// and After are empty then.
	Partial bool `json:"partial,omitempty"`
}

// changesLine holds the fields of a Claude Code JSONL line that record file
// content: tool results of Edit/Write, and checkpoint snapshots.
type changesLine struct {
	Type          string          `json:"type"`
	Timestamp     string          `json:"timestamp"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	Snapshot      struct {
		TrackedFileBackups map[string]struct {
			BackupFileName *string `json:"backupFileName"`
			BackupTime     string  `json:"backupTime"`
		} `json:"trackedFileBackups"`
	} `json:"snapshot"`
}

// claudeEditResult is the toolUseResult of a successful Edit or Write.
// Claude Code omits OriginalFile for files over about 10 KB.
type claudeEditResult struct {
	FilePath        string          `json:"filePath"`
	Type            string          `json:"type"` // Write: "create" or "update"
	Content         string          `json:"content"`
	OldString       *string         `json:"oldString"`
	NewString       string          `json:"newString"`
	ReplaceAll      bool            `json:"replaceAll"`
	OriginalFile    *string         `json:"originalFile"`
	StructuredPatch json.RawMessage `json:"structuredPatch"`
}

// changeEvent is an edit, or a checkpoint backup that holds the true file
// content at a user prompt.
type changeEvent struct {
	at     time.Time
	edit   *claudeEditResult
	path   string // checkpoint target
	backup string // checkpoint file name
}

// ClaudeChanges rebuilds the net change per file of a Claude Code session by
// replaying its Edit and Write results. historyDir is the session's
// checkpoint folder (~/.claude/file-history/<session>); it supplies the
// content of large files and resyncs the replay at each user prompt, which
// also picks up Bash changes to edited files. Files are sorted by path;
// files without a net change are left out.
func ClaudeChanges(transcriptPath, historyDir string) ([]FileChange, error) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	events, err := readChangeEvents(f)
	if err != nil {
		return nil, err
	}
	return replayChanges(events, historyDir), nil
}

// readChangeEvents collects edits and checkpoints in time order. Checkpoint
// keys may be relative to the session's cwd, so they are matched to edited
// files by the path hash in the backup file name instead.
func readChangeEvents(r io.Reader) ([]changeEvent, error) {
	var events, checkpoints []changeEvent
	byHash := map[string]string{}
	scanner := newLineScanner(r)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.Contains(line, []byte(`"toolUseResult"`)) && !bytes.Contains(line, []byte(`"file-history-snapshot"`)) {
			continue
		}
		var l changesLine
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		if l.Type == "file-history-snapshot" {
			for _, b := range l.Snapshot.TrackedFileBackups {
				if b.BackupFileName != nil {
					checkpoints = append(checkpoints, changeEvent{at: parseTime(b.BackupTime), backup: *b.BackupFileName})
				}
			}
			continue
		}
		var res claudeEditResult
		if json.Unmarshal(l.ToolUseResult, &res) != nil || res.FilePath == "" || res.StructuredPatch == nil {
			continue
		}
		events = append(events, changeEvent{at: parseTime(l.Timestamp), edit: &res})
		byHash[backupHash(res.FilePath)] = res.FilePath
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for _, c := range checkpoints {
		if !backupNameRe.MatchString(c.backup) {
			continue
		}
		hash, _, _ := strings.Cut(c.backup, "@")
		if p, ok := byHash[hash]; ok {
			c.path = p
			events = append(events, c)
		}
	}
	slices.SortStableFunc(events, func(a, b changeEvent) int { return a.at.Compare(b.at) })
	return events, nil
}

// backupNameRe keeps backup names from the transcript inside historyDir.
var backupNameRe = regexp.MustCompile(`^[0-9a-f]{16}@v[0-9]+$`)

// backupHash is the prefix Claude Code names a file's checkpoint backups by.
func backupHash(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])[:16]
}

// replayState tracks one file during replay; known is false while its
// content is uncertain.
type replayState struct {
	FileChange
	known bool
}

func replayChanges(events []changeEvent, historyDir string) []FileChange {
	readBackup := func(name string) (string, bool) {
		if historyDir == "" {
			return "", false
		}
		b, err := os.ReadFile(filepath.Join(historyDir, name))
		return string(b), err == nil
	}

	files := map[string]*replayState{}
	for _, e := range events {
		if e.edit == nil {
			if s := files[e.path]; s != nil {
				if content, ok := readBackup(e.backup); ok {
					s.After, s.known = content, true
				}
			}
			continue
		}
		s := files[e.edit.FilePath]
		if s == nil {
			s = startReplay(e.edit, readBackup)
			files[e.edit.FilePath] = s
		}
		s.apply(e.edit)
	}

	changes := make([]FileChange, 0, len(files))
	for _, s := range files {
		if !s.known || s.Partial {
			changes = append(changes, FileChange{Path: s.Path, Created: s.Created, Partial: true})
		} else if s.Created || s.Before != s.After {
			changes = append(changes, s.FileChange)
		}
	}
	slices.SortFunc(changes, func(a, b FileChange) int { return strings.Compare(a.Path, b.Path) })
	return changes
}

// startReplay sets a file's content from before the session: empty for a
// created file, else the edit's OriginalFile, else the first checkpoint.
func startReplay(e *claudeEditResult, readBackup func(string) (string, bool)) *replayState {
	s := &replayState{FileChange: FileChange{Path: e.FilePath}, known: true}
	switch {
	case e.Type == "create":
		s.Created = true
	case e.OriginalFile != nil:
		s.Before = *e.OriginalFile
	default:
		var ok bool
		if s.Before, ok = readBackup(backupHash(e.FilePath) + "@v1"); !ok {
			s.Partial = true
		}
	}
	s.After = s.Before
	return s
}

// apply replays one edit. A recorded OriginalFile resyncs the content first.
// An Edit whose old text no longer matches exactly once (or at all, with
// replaceAll) means the file changed outside the recorded edits.
func (s *replayState) apply(e *claudeEditResult) {
	if e.OriginalFile != nil && e.Type != "create" {
		s.After, s.known = *e.OriginalFile, true
	}
	switch {
	case e.Type == "create" || e.Type == "update":
		s.After, s.known = e.Content, true
	case e.OldString == nil:
		s.known = false
	case s.known:
		n := strings.Count(s.After, *e.OldString)
		if *e.OldString == "" || n == 0 || (n > 1 && !e.ReplaceAll) {
			s.known = false
		} else if e.ReplaceAll {
			s.After = strings.ReplaceAll(s.After, *e.OldString, e.NewString)
		} else {
			s.After = strings.Replace(s.After, *e.OldString, e.NewString, 1)
		}
	}
}
