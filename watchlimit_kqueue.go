//go:build freebsd || openbsd || netbsd || dragonfly || darwin

package main

import "syscall"

// watchEntryLimit is the number of entries the tree watcher may cover. kqueue
// holds a file descriptor for each one; a quarter of the descriptor limit
// stays free for serving files and connections.
func watchEntryLimit() int {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0
	}
	return int(lim.Cur / 4 * 3)
}
