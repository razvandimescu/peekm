package main

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
)

const taskDoc = "# Plan\n\n```mermaid\ngraph TD; A-->B\n```\n\n" +
	"```\n- [ ] inside code, not a task\n```\n\n" +
	"- [ ] write tests\n- [x] ship it\n  - [ ] nested\n- plain item\n\n" +
	"1. [ ] numbered\n"

var dataTaskRe = regexp.MustCompile(`data-task="([0-9a-f]{8})"`)

func renderTasks(t *testing.T, src string) (string, []string) {
	t.Helper()
	var buf bytes.Buffer
	if err := newMarkdownRenderer().Convert(preprocessMermaid([]byte(src)), &buf); err != nil {
		t.Fatal(err)
	}
	var fps []string
	for _, m := range dataTaskRe.FindAllStringSubmatch(buf.String(), -1) {
		fps = append(fps, m[1])
	}
	return buf.String(), fps
}

func TestTaskListMarkup(t *testing.T) {
	html, fps := renderTasks(t, taskDoc)
	if len(fps) != 4 {
		t.Fatalf("got %d task boxes, want 4:\n%s", len(fps), html)
	}
	for _, want := range []string{
		`<ul class="contains-task-list">`,
		`<li class="task-list-item"><input type="checkbox" class="task-list-item-checkbox"`,
		`disabled checked> ship it`,
		`<li>plain item</li>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in:\n%s", want, html)
		}
	}
}

func TestSetTaskState(t *testing.T) {
	_, fps := renderTasks(t, taskDoc)
	tests := []struct {
		index   int
		checked bool
		line    string
	}{
		{0, true, "- [x] write tests"},
		{1, false, "- [ ] ship it"},
		{2, true, "  - [x] nested"},
		{3, true, "1. [x] numbered"},
	}
	for _, tt := range tests {
		out, err := setTaskState([]byte(taskDoc), tt.index, fps[tt.index], tt.checked)
		if err != nil {
			t.Fatalf("index %d: %v", tt.index, err)
		}
		if !strings.Contains(string(out), "\n"+tt.line+"\n") {
			t.Errorf("index %d: %q not found in:\n%s", tt.index, tt.line, out)
		}
		if len(out) != len(taskDoc) || strings.Count(string(out), "inside code, not a task") != 1 {
			t.Errorf("index %d: unexpected rewrite:\n%s", tt.index, out)
		}
	}
}

func TestSetTaskStateStale(t *testing.T) {
	_, fps := renderTasks(t, taskDoc)
	edited := strings.Replace(taskDoc, "- [ ] write tests\n", "- [ ] new first task\n- [ ] write tests\n", 1)
	cases := map[string]struct {
		src   string
		index int
		fp    string
	}{
		"task inserted above": {edited, 1, fps[1]},
		"index out of range":  {taskDoc, 4, fps[0]},
		"negative index":      {taskDoc, -1, fps[0]},
	}
	for name, c := range cases {
		if _, err := setTaskState([]byte(c.src), c.index, c.fp, true); !errors.Is(err, errTaskStale) {
			t.Errorf("%s: err = %v, want errTaskStale", name, err)
		}
	}
}
