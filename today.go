package main

import (
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fsnotify/fsnotify"
)

// Today is a per-day plan kept as a GFM task list in ~/.peekm/days/<date>.md.
// It stays plain markdown so agents can read and tick it too; Recap remains
// the record of what shipped.

var dayFileRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.md$`)

// maxTaskLen bounds one added task; anything longer belongs in the file.
const maxTaskLen = 500

// daysRelDir is the plan folder under $HOME.
const daysRelDir = ".peekm/days"

func daysDir() string {
	return homeSubdir(daysRelDir)
}

func dayFilePath(date string) string {
	return filepath.Join(daysDir(), date+".md")
}

// isDayFile reports whether absPath is a day plan. Like memory files, these
// are whitelisted although they live outside the browse dir.
func isDayFile(absPath string) bool {
	return filepath.Dir(absPath) == daysDir() && dayFileRe.MatchString(filepath.Base(absPath))
}

type todayTemplateData struct {
	baseTemplateData
	TreeHTML   template.HTML
	BrowsePath string
	Date       string
	Label      string
	Eyebrow    string
	PrevDate   string
	NextDate   string // empty on tomorrow, the last day that can be planned
	MaxDate    string
	IsToday    bool
	Editable   bool // today or tomorrow: add input and carry-over
	ShowRecap  bool // the day has started, so Recap has something to show
	WebPath    string
	AbsPath    string
	Exists     bool
	Plan       template.HTML
	Open, Done int
	CarryLabel string
	CarryItems []string
}

func serveToday(w http.ResponseWriter, r *http.Request) {
	today := startOfDay(time.Now())
	tomorrow := today.AddDate(0, 0, 1)
	day := targetDay(r.URL.Query().Get("d"), today, tomorrow)
	date := day.Format(time.DateOnly)

	data := todayTemplateData{
		baseTemplateData: newBaseTemplateData(r),
		TreeHTML:         shellTreeHTML(r),
		BrowsePath:       currentBrowseDir(),
		Date:             date,
		Label:            day.Format("Monday, 2 January 2006"),
		Eyebrow:          todayEyebrow(day, today, tomorrow),
		PrevDate:         day.AddDate(0, 0, -1).Format(time.DateOnly),
		MaxDate:          tomorrow.Format(time.DateOnly),
		IsToday:          day.Equal(today),
		Editable:         !day.Before(today),
		ShowRecap:        !day.After(today),
		WebPath:          "~/" + daysRelDir + "/" + date + ".md",
		AbsPath:          dayFilePath(date),
	}
	if day.Before(tomorrow) {
		data.NextDate = day.AddDate(0, 0, 1).Format(time.DateOnly)
	}

	if src, err := os.ReadFile(data.AbsPath); err == nil {
		data.Exists = true
		data.Open, data.Done = countTasks(src)
		if data.Plan, err = renderMarkdownBytes(src); err != nil {
			log.Printf("today: render %s: %v", data.AbsPath, err)
		}
	} else if data.Editable {
		from, items := carryOverCandidates(date)
		data.CarryItems = items
		if t, err := time.ParseInLocation(time.DateOnly, from, today.Location()); err == nil {
			data.CarryLabel = t.Format("Mon 2 Jan")
		}
	}
	renderTemplatePair(w, r, todayTmpl, todayPartialTmpl, data)
}

func todayEyebrow(day, today, tomorrow time.Time) string {
	switch {
	case day.Equal(today):
		return "Today"
	case day.Equal(tomorrow):
		return "Tomorrow"
	default:
		return "Plan"
	}
}

func countTasks(src []byte) (open, done int) {
	for _, box := range parseTaskBoxes(src) {
		if box.IsChecked {
			done++
		} else {
			open++
		}
	}
	return open, done
}

func openTasks(src []byte) []string {
	var items []string
	for _, box := range parseTaskBoxes(src) {
		if box.IsChecked {
			continue
		}
		if item := strings.TrimSpace(string(taskText(src, taskBoxOffset(box)))); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// carryOverCandidates returns the open tasks of the latest plan before date.
// Older plans are not searched: their leftovers were already declined once.
func carryOverCandidates(date string) (from string, items []string) {
	dir := daysDir()
	entries, _ := os.ReadDir(dir)
	for i := len(entries) - 1; i >= 0; i-- {
		name := entries[i].Name()
		if !dayFileRe.MatchString(name) || name >= date+".md" {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", nil
		}
		return strings.TrimSuffix(name, ".md"), openTasks(src)
	}
	return "", nil
}

// handleTodayAdd appends tasks to a day plan, creating it if needed. An empty
// list still creates the file, which is how "Start fresh" settles carry-over.
func handleTodayAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Date  string   `json:"date"`
		Items []string `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if _, err := time.Parse(time.DateOnly, req.Date); err != nil {
		http.Error(w, "invalid date", http.StatusBadRequest)
		return
	}
	var lines strings.Builder
	for _, item := range req.Items {
		item = strings.Join(strings.Fields(item), " ")
		if utf8.RuneCountInString(item) > maxTaskLen {
			http.Error(w, "task too long", http.StatusBadRequest)
			return
		}
		if item != "" {
			lines.WriteString("- [ ] " + item + "\n")
		}
	}
	if err := appendToDayFile(req.Date, lines.String()); err != nil {
		log.Printf("today: append %s: %v", req.Date, err)
		http.Error(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func appendToDayFile(date, lines string) error {
	dir := daysDir()
	if dir == "" {
		return errors.New("no home directory")
	}
	fileEditMu.Lock()
	defer fileEditMu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := dayFilePath(date)
	src, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(src) > 0 && src[len(src)-1] != '\n' {
		src = append(src, '\n')
	}
	return atomicWriteFile(path, string(src)+lines)
}

// watchDaysDir broadcasts day_modified whenever a plan changes, whoever wrote
// it (the page, an agent, an editor), so an open Today page stays current.
func watchDaysDir() {
	dir := daysDir()
	if dir == "" {
		return
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("today: watcher: %v", err)
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil || watcher.Add(dir) != nil {
		log.Printf("today: cannot watch %s", dir)
		watcher.Close()
		return
	}
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if name := filepath.Base(event.Name); dayFileRe.MatchString(name) {
					broadcastJSON(fileEventMessage{Type: "day_modified", Path: strings.TrimSuffix(name, ".md")})
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("today: watcher: %v", err)
			}
		}
	}()
}
