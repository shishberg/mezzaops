// task/logfile.go
package task

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// TailLogFile reads the last n bytes from the file at path.
// Returns "" if the file doesn't exist or is empty.
func TailLogFile(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return ""
	}

	size := fi.Size()
	offset := int64(0)
	readLen := size
	if size > int64(n) {
		offset = size - int64(n)
		readLen = int64(n)
	}

	buf := make([]byte, readLen)
	_, err = f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return ""
	}
	return string(buf)
}

// LogPath returns the log file path for a task with a given PID.
func LogPath(dir, taskName string, pid int) string {
	return filepath.Join(dir, fmt.Sprintf("%s.%d.log", taskName, pid))
}

// CleanupOldLogs removes old log files for a task, keeping the most recent `keep` files.
// Log files are matched by the pattern <taskName>.*.log.
func CleanupOldLogs(dir, taskName string, keep int) {
	pattern := filepath.Join(dir, taskName+".*.log")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) <= keep {
		return
	}
	// Filter out files we can't stat, then sort by modification time (oldest first)
	type fileEntry struct {
		path  string
		mtime int64
	}
	var entries []fileEntry
	for _, path := range matches {
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		entries = append(entries, fileEntry{path, fi.ModTime().UnixNano()})
	}
	if len(entries) <= keep {
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].mtime < entries[j].mtime
	})
	for _, e := range entries[:len(entries)-keep] {
		os.Remove(e.path)
	}
}
