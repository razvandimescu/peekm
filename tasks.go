package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"sync"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// taskListHTML renders GFM task lists with GitHub's markup (classes styled by
// github-markdown.css) and tags each checkbox with a fingerprint of its source
// line, so a click can be mapped back to the exact "[ ]" in the file.
type taskListHTML struct{}

// Priority below the GFM task-list renderer (500) so this one wins.
const taskListPriority = 100

func (t taskListHTML) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithASTTransformers(util.Prioritized(t, taskListPriority)))
	m.Renderer().AddOptions(renderer.WithNodeRenderers(util.Prioritized(t, taskListPriority)))
}

func (taskListHTML) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	walkTaskBoxes(doc, func(box *extast.TaskCheckBox) {
		item := box.Parent().Parent()
		item.SetAttributeString("class", []byte("task-list-item"))
		item.Parent().SetAttributeString("class", []byte("contains-task-list"))
	})
}

func (taskListHTML) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(extast.KindTaskCheckBox, renderTaskBox)
}

func renderTaskBox(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	checked := ""
	if n.(*extast.TaskCheckBox).IsChecked {
		checked = " checked"
	}
	fmt.Fprintf(w, `<input type="checkbox" class="task-list-item-checkbox" data-task="%s" disabled%s> `,
		taskFingerprint(source, taskBoxOffset(n)), checked)
	return ast.WalkContinue, nil
}

func walkTaskBoxes(doc ast.Node, fn func(*extast.TaskCheckBox)) {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if box, ok := n.(*extast.TaskCheckBox); ok && entering {
			fn(box)
		}
		return ast.WalkContinue, nil
	})
}

// taskBoxOffset is the byte offset of the box's "[": the GFM parser only
// accepts a checkbox at the very start of a list item's first line.
func taskBoxOffset(box ast.Node) int {
	return box.Parent().Lines().At(0).Start
}

// taskFingerprint hashes the task's line after its "[ ]"/"[x]", so it is the
// same in both states and changes when the task text does.
func taskFingerprint(src []byte, off int) string {
	line := src[off+len("[ ]"):]
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	h := fnv.New32a()
	h.Write(line)
	return fmt.Sprintf("%08x", h.Sum32())
}

var errTaskStale = errors.New("file changed since it was displayed")

// setTaskState sets the index-th task box in src, after checking that the box
// still carries the fingerprint the viewer rendered.
func setTaskState(src []byte, index int, fingerprint string, checked bool) ([]byte, error) {
	var offsets []int
	walkTaskBoxes(newMarkdownRenderer().Parser().Parse(text.NewReader(src)), func(box *extast.TaskCheckBox) {
		offsets = append(offsets, taskBoxOffset(box))
	})
	if index < 0 || index >= len(offsets) || taskFingerprint(src, offsets[index]) != fingerprint {
		return nil, errTaskStale
	}
	mark := byte(' ')
	if checked {
		mark = 'x'
	}
	out := bytes.Clone(src)
	out[offsets[index]+1] = mark
	return out, nil
}

// taskToggleMu serializes read-modify-write so rapid clicks don't drop updates.
var taskToggleMu sync.Mutex

func handleTaskToggle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		File    string `json:"file"`
		Index   int    `json:"index"`
		Task    string `json:"task"`
		Checked bool   `json:"checked"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	path, err := validateAndResolvePath(resolveFilePath(req.File))
	if err != nil || !isWhitelistedFile(path) {
		http.Error(w, "File not found or access denied", http.StatusForbidden)
		return
	}

	taskToggleMu.Lock()
	defer taskToggleMu.Unlock()
	src, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "Failed to read file", http.StatusInternalServerError)
		return
	}
	out, err := setTaskState(src, req.Index, req.Task, req.Checked)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err := atomicWriteFile(path, string(out)); err != nil {
		http.Error(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
