package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTargetDay(t *testing.T) {
	today := time.Date(2026, 10, 28, 0, 0, 0, 0, time.Local)
	tomorrow := today.AddDate(0, 0, 1)
	tests := map[string]time.Time{
		"":           today,
		"garbage":    today,
		"2026-10-20": time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local),
		"2026-10-29": tomorrow,
		"2026-12-01": tomorrow,
	}
	for q, want := range tests {
		if got := targetDay(q, today, tomorrow); !got.Equal(want) {
			t.Errorf("targetDay(%q) = %v, want %v", q, got, want)
		}
	}
}

func TestOpenTasksAndCount(t *testing.T) {
	src := []byte("## peekm\n\n- [ ] write tests\n- [x] ship it\n  - [ ]   nested  \n- [ ]\n\n```\n- [ ] code\n```\n")
	if got, want := openTasks(src), []string{"write tests", "nested"}; !reflect.DeepEqual(got, want) {
		t.Errorf("openTasks = %q, want %q", got, want)
	}
	if open, done := countTasks(src); open != 3 || done != 1 {
		t.Errorf("countTasks = %d open, %d done, want 3, 1", open, done)
	}
}

func setTempHome(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return daysDir()
}

func writeDay(t *testing.T, dir, date, content string) {
	t.Helper()
	mustMkdir(t, dir)
	mustWrite(t, filepath.Join(dir, date+".md"), content)
}

func TestCarryOverCandidates(t *testing.T) {
	dir := setTempHome(t)
	writeDay(t, dir, "2026-10-20", "- [ ] old leftover\n")
	writeDay(t, dir, "2026-10-26", "- [x] done\n- [ ] review PR\n")
	writeDay(t, dir, "2026-10-29", "- [ ] tomorrow\n")

	from, items := carryOverCandidates("2026-10-28")
	if from != "2026-10-26" || !reflect.DeepEqual(items, []string{"review PR"}) {
		t.Errorf("got %q %q, want 2026-10-26 [review PR]", from, items)
	}

	writeDay(t, dir, "2026-10-27", "- [x] all done\n")
	if from, items := carryOverCandidates("2026-10-28"); from != "2026-10-27" || items != nil {
		t.Errorf("latest plan fully done: got %q %q, want no items", from, items)
	}
}

func postTodayAdd(body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handleTodayAdd(rec, httptest.NewRequest(http.MethodPost, "/today/add", strings.NewReader(body)))
	return rec
}

func TestHandleTodayAdd(t *testing.T) {
	dir := setTempHome(t)
	path := filepath.Join(dir, "2026-10-28.md")

	if rec := postTodayAdd(`{"date":"2026-10-28","items":[]}`); rec.Code != http.StatusNoContent {
		t.Fatalf("start fresh: status %d", rec.Code)
	}
	if got, err := os.ReadFile(path); err != nil || len(got) != 0 {
		t.Fatalf("start fresh should create an empty plan, got %q %v", got, err)
	}

	writeDay(t, dir, "2026-10-28", "notes without newline")
	if rec := postTodayAdd(`{"date":"2026-10-28","items":["  review\nPR  #56 ", "   "]}`); rec.Code != http.StatusNoContent {
		t.Fatalf("add: status %d", rec.Code)
	}
	if got, _ := os.ReadFile(path); string(got) != "notes without newline\n- [ ] review PR #56\n" {
		t.Errorf("plan = %q", got)
	}

	for name, body := range map[string]string{
		"bad json":   `{`,
		"bad date":   `{"date":"../../etc/x","items":["a"]}`,
		"too long":   `{"date":"2026-10-28","items":["` + strings.Repeat("a", maxTaskLen+1) + `"]}`,
		"empty date": `{"items":["a"]}`,
	} {
		if rec := postTodayAdd(body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
}

func TestIsDayFile(t *testing.T) {
	dir := setTempHome(t)
	tests := map[string]bool{
		filepath.Join(dir, "2026-10-28.md"):               true,
		filepath.Join(dir, "notes.md"):                    false,
		filepath.Join(dir, "sub", "2026-10-28.md"):        false,
		filepath.Join(dir, ".peekm-tmp-1"):                false,
		filepath.Join(filepath.Dir(dir), "2026-10-28.md"): false,
	}
	for path, want := range tests {
		if got := isDayFile(path); got != want {
			t.Errorf("isDayFile(%q) = %v, want %v", path, got, want)
		}
	}
}
