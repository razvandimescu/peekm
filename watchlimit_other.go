//go:build !freebsd && !openbsd && !netbsd && !dragonfly && !darwin

package main

import "math"

// watchEntryLimit is the number of entries the tree watcher may cover.
// inotify and ReadDirectoryChangesW hold no file descriptor per entry.
func watchEntryLimit() int {
	return math.MaxInt
}
