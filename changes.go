package main

import (
	"html/template"
	"os"
	"path/filepath"

	"github.com/razvandimescu/peekm/transcript"
)

const (
	changesMaxLines  = 2000 // rendered diff lines per file
	changesOpenLines = 300  // diffs up to this many changed lines start expanded
)

// changesView is the transcript's Changes tab: the net diff per file.
type changesView struct {
	Files          []sessionChange
	Added, Removed int
}

type sessionChange struct {
	Path           string // relative to the browse dir, else ~-prefixed
	FullPath       string
	Created        bool
	Partial        bool
	Open           bool
	Added, Removed int
	DiffHTML       template.HTML
}

// buildChangesView diffs each file a Claude Code session edited, from before
// its first edit to after its last.
func buildChangesView(transcriptPath, sessionID string) (*changesView, error) {
	changes, err := transcript.ClaudeChanges(transcriptPath, homeSubdir(".claude", "file-history", sessionID))
	if err != nil {
		return nil, err
	}
	v := &changesView{}
	for _, c := range changes {
		sc := sessionChange{Path: displayPath(c.Path), FullPath: c.Path, Created: c.Created, Partial: c.Partial, Open: c.Partial}
		if !c.Partial {
			html, added, removed := hunkDiffHTML(diffOps(splitDiffLines(c.Before), splitDiffLines(c.After)), changesMaxLines)
			if added+removed == 0 && !c.Created {
				continue // only trailing newlines changed
			}
			sc.DiffHTML, sc.Added, sc.Removed = template.HTML(html), added, removed
			sc.Open = added+removed <= changesOpenLines
		}
		v.Added += sc.Added
		v.Removed += sc.Removed
		v.Files = append(v.Files, sc)
	}
	return v, nil
}

// displayPath shortens p relative to the browse dir, else to ~.
func displayPath(p string) string {
	if dir := currentBrowseDir(); isSubpath(p, dir) {
		if rel, err := filepath.Rel(dir, p); err == nil {
			return rel
		}
	}
	if home, _ := os.UserHomeDir(); isSubpath(p, home) {
		if rel, err := filepath.Rel(home, p); err == nil {
			return "~/" + rel
		}
	}
	return p
}
