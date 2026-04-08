package service

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// LogPath returns the log file path for a service with a given PID.
func LogPath(dir, name string, pid int) string {
	return filepath.Join(dir, fmt.Sprintf("%s.%d.log", name, pid))
}

// TailLogFile reads the last maxBytes bytes from the file at path.
// Returns "" if the file doesn't exist or is empty.
func TailLogFile(path string, maxBytes int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck // read-only file

	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return ""
	}

	size := fi.Size()
	offset := int64(0)
	readLen := size
	if size > int64(maxBytes) {
		offset = size - int64(maxBytes)
		readLen = int64(maxBytes)
	}

	buf := make([]byte, readLen)
	_, err = f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return ""
	}
	return string(buf)
}

// CleanupOldLogs removes old log files for a service, keeping the most recent
// `keep` files. Log files are matched by the pattern <name>.*.log.
func CleanupOldLogs(dir, name string, keep int) {
	pattern := filepath.Join(dir, name+".*.log")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) <= keep {
		return
	}

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
		_ = os.Remove(e.path)
	}
}
