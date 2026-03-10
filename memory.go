package main

import (
	"bufio"
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type memoryTemplateData struct {
	baseTemplateData
	TreeHTML   template.HTML
	Title      string
	Subtitle   string
	BrowsePath string
	Projects   []memoryProjectCard
}

type memoryProjectCard struct {
	Name      string
	Files     []memoryFileEntry
	LastMod   string
	FirstLink string
	Sections  []string // H2 headings across all files
	LineCount int      // total lines across all files
	Snippet   string   // preview text from first meaningful content
}

type memoryFileEntry struct {
	Name string
	Link string
}

type memoryProject struct {
	encoded   string
	decoded   string
	files     []string
	newestMod time.Time
}

func serveMemory(w http.ResponseWriter, r *http.Request) {
	memFiles, projectIndex := collectMemoryFiles("")
	home, _ := os.UserHomeDir()
	projectsDir := filepath.Join(home, ".claude", "projects")

	fileMutex.RLock()
	urlBase := browseDir
	fileMutex.RUnlock()

	projects := groupMemoryByProject(memFiles, projectsDir, projectIndex)
	treeHTML := buildMemoryTreeHTML(projects, urlBase, "")
	var cards []memoryProjectCard
	for _, p := range projects {
		sections, lineCount, snippet := extractMemoryInsights(p.files)
		card := memoryProjectCard{
			Name:      p.decoded,
			LastMod:   formatTimeAgo(p.newestMod),
			Sections:  sections,
			LineCount: lineCount,
			Snippet:   snippet,
		}
		for _, f := range p.files {
			relPath := tildeRelPath(f, urlBase)
			card.Files = append(card.Files, memoryFileEntry{
				Name: filepath.Base(f),
				Link: "/view/" + pathEscapeSegments(relPath),
			})
		}
		if len(card.Files) > 0 {
			card.FirstLink = card.Files[0].Link
		}
		cards = append(cards, card)
	}

	if len(memFiles) == 0 {
		treeHTML = `<div style="padding: 16px; color: var(--fgColor-muted); font-size: 13px;">No Claude Code memory files found.<br><br>Memory files appear in <code>~/.claude/projects/*/memory/</code> as Claude Code learns about your projects.</div>`
	}

	data := memoryTemplateData{
		baseTemplateData: newBaseTemplateData(),
		Title:            "Memory Browser",
		Subtitle:         formatMemorySubtitle(cards),
		TreeHTML:         template.HTML(treeHTML),
		BrowsePath:       projectsDir,
		Projects:         cards,
	}

	renderTemplatePair(w, r, memoryTmpl, memoryPartialTmpl, data)
}

func decodeProjectName(encoded string) string {
	s := strings.TrimPrefix(encoded, "-")
	if s == "" {
		return encoded
	}
	if path, ok := resolveEncodedPath(strings.Split(s, "-"), 0, string(filepath.Separator)); ok {
		return filepath.Base(path)
	}
	return s
}

// resolveEncodedPath recursively tries all ways to split dash-separated parts
// into real directory path segments, returning the deepest resolved filesystem path.
// Claude Code encodes both / and _ as -, so both variants are tried.
// Prefers the longest match to avoid ambiguity (e.g. rinkt_bot vs rinkt_bot_api).
func resolveEncodedPath(parts []string, start int, resolved string) (string, bool) {
	if start >= len(parts) {
		return resolved, true
	}
	var best string
	for n := 1; n <= len(parts)-start; n++ {
		seg := parts[start : start+n]
		candidates := [2]string{strings.Join(seg, "-"), strings.Join(seg, "_")}
		limit := 1
		if n > 1 {
			limit = 2
		}
		for _, segment := range candidates[:limit] {
			candidate := filepath.Join(resolved, segment)
			info, err := os.Stat(candidate)
			if err != nil || !info.IsDir() {
				continue
			}
			if path, ok := resolveEncodedPath(parts, start+n, candidate); ok && len(path) > len(best) {
				best = path
			}
		}
	}
	if best != "" {
		return best, true
	}
	if start > 0 {
		return resolved, true
	}
	return "", false
}

// resolveProjectDir resolves an encoded project directory name to its full filesystem path.
func resolveProjectDir(encoded string) string {
	s := strings.TrimPrefix(encoded, "-")
	if s == "" {
		return ""
	}
	path, ok := resolveEncodedPath(strings.Split(s, "-"), 0, string(filepath.Separator))
	if !ok {
		return ""
	}
	return path
}

// collectMemoryFiles scans ~/.claude/projects/*/memory/*.md and each project's .claude/CLAUDE.md.
// Returns sorted absolute paths and a projectIndex mapping external files (CLAUDE.md) to their
// encoded project directory name (for groupMemoryByProject).
// If filter is non-empty, only projects whose decoded name contains the filter are included.
func collectMemoryFiles(filter string) ([]string, map[string]string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil
	}

	projectsDir := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil, nil
	}

	var files []string
	projectIndex := make(map[string]string)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		if filter != "" {
			decoded := decodeProjectName(entry.Name())
			if !strings.Contains(strings.ToLower(decoded), strings.ToLower(filter)) {
				continue
			}
		}

		// Collect memory/*.md files
		memDir := filepath.Join(projectsDir, entry.Name(), "memory")
		mdFiles, _ := filepath.Glob(filepath.Join(memDir, "*.md"))

		// Also check for project's .claude/CLAUDE.md
		var claudeMD string
		if projDir := resolveProjectDir(entry.Name()); projDir != "" {
			candidate := filepath.Join(projDir, ".claude", "CLAUDE.md")
			if _, err := os.Stat(candidate); err == nil {
				claudeMD = candidate
				projectIndex[candidate] = entry.Name()
			}
		}

		if len(mdFiles) == 0 && claudeMD == "" {
			continue
		}
		if claudeMD != "" {
			files = append(files, claudeMD)
		}
		files = append(files, mdFiles...)
	}

	sort.Strings(files)
	return files, projectIndex
}

// groupMemoryByProject groups memory files by their parent project directory,
// memoryFilePriority returns sort order: CLAUDE.md=0, MEMORY.md=1, others by name.
func memoryFilePriority(path string) string {
	switch filepath.Base(path) {
	case "CLAUDE.md":
		return "\x00"
	case "MEMORY.md":
		return "\x01"
	default:
		return filepath.Base(path)
	}
}

func formatMemorySubtitle(cards []memoryProjectCard) string {
	totalFiles := 0
	totalLines := 0
	for _, c := range cards {
		totalFiles += len(c.Files)
		totalLines += c.LineCount
	}
	return fmt.Sprintf("%d projects · %d files · %s lines of memory",
		len(cards), totalFiles, formatNumber(totalLines))
}

func formatNumber(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%d,%03d", n/1000, n%1000)
}

// extractMemoryInsights scans markdown files for a project and returns
// H2 section headers, total line count, and a content snippet.
func extractMemoryInsights(files []string) (sections []string, lineCount int, snippet string) {
	seen := make(map[string]bool)
	for _, f := range files {
		file, err := os.Open(f)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			lineCount++
			line := scanner.Text()
			if strings.HasPrefix(line, "## ") {
				heading := strings.TrimPrefix(line, "## ")
				if !seen[heading] {
					seen[heading] = true
					sections = append(sections, heading)
				}
			}
			if snippet == "" && line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "---") && !strings.HasPrefix(line, "```") {
				snippet = truncateString(line, 150)
			}
		}
		file.Close()
	}
	return
}

// decodes project names, and returns them sorted by most recent modification.
// Files under baseDir (memory files) are grouped by their first path component.
// Files outside baseDir (e.g. project CLAUDE.md) are matched via projectIndex.
func groupMemoryByProject(files []string, baseDir string, projectIndex map[string]string) []*memoryProject {
	groups := make(map[string]*memoryProject)

	getOrCreate := func(encoded string) *memoryProject {
		g, ok := groups[encoded]
		if !ok {
			g = &memoryProject{encoded: encoded, decoded: decodeProjectName(encoded)}
			groups[encoded] = g
		}
		return g
	}

	for _, f := range files {
		var g *memoryProject
		rel, err := filepath.Rel(baseDir, f)
		if err == nil && !strings.HasPrefix(rel, "..") {
			parts := strings.SplitN(rel, string(filepath.Separator), 3)
			if len(parts) < 3 {
				continue
			}
			g = getOrCreate(parts[0])
		} else if encoded, ok := projectIndex[f]; ok {
			g = getOrCreate(encoded)
		} else {
			continue
		}

		g.files = append(g.files, f)
		if info, err := os.Stat(f); err == nil && info.ModTime().After(g.newestMod) {
			g.newestMod = info.ModTime()
		}
	}

	sorted := make([]*memoryProject, 0, len(groups))
	for _, g := range groups {
		sort.Slice(g.files, func(i, j int) bool {
			return memoryFilePriority(g.files[i]) < memoryFilePriority(g.files[j])
		})
		sorted = append(sorted, g)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].newestMod.After(sorted[j].newestMod)
	})
	return sorted
}

// buildMemoryTreeHTML builds a two-level tree: decoded project names → memory files.
// Accepts pre-grouped projects to avoid redundant groupMemoryByProject calls.
func buildMemoryTreeHTML(projects []*memoryProject, urlBaseDir, activeFile string) string {
	if len(projects) == 0 {
		return ""
	}

	var buf bytes.Buffer
	for _, g := range projects {
		projNode := &fileNode{name: g.decoded, path: g.encoded, isDir: true}
		hasActive := false
		for _, f := range g.files {
			relPath := tildeRelPath(f, urlBaseDir)
			projNode.children = append(projNode.children, &fileNode{name: filepath.Base(f), path: relPath})
			if f == activeFile {
				hasActive = true
			}
		}
		depth := 1
		if hasActive {
			depth = 0
		}
		generateTreeHTMLRecursive(projNode, "", false, false, depth, false, &buf)
	}

	return buf.String()
}
