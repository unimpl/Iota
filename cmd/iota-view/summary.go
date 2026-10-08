package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Summary paths are derived from a selected session name and checkpoint sequence.
// The API never accepts a filesystem path from the browser or from a log payload.
func summaryHandler(folder string) http.HandlerFunc {
	dirs := []string{folder}
	if home, err := os.UserHomeDir(); err == nil {
		dir := filepath.Join(home, ".iota", "sessions")
		if dir != folder {
			dirs = append(dirs, dir)
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		sequence, err := strconv.ParseUint(r.URL.Query().Get("event"), 10, 64)
		if !sessionFilename.MatchString(name) || err != nil || sequence == 0 {
			http.Error(w, "invalid session name or checkpoint event", http.StatusBadRequest)
			return
		}
		info, err := os.Lstat(filepath.Join(folder, name))
		if err != nil || !info.Mode().IsRegular() {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		filename := strings.TrimSuffix(name, ".jsonl") + fmt.Sprintf(".summary.%d.md", sequence)
		for _, dir := range dirs {
			path := filepath.Join(dir, filename)
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil || !info.Mode().IsRegular() {
				http.Error(w, "summary is not a regular file", http.StatusBadRequest)
				return
			}
			file, err := os.Open(path)
			if err != nil {
				http.Error(w, "summary cannot be read", http.StatusInternalServerError)
				return
			}
			const maxSummaryBytes = 4 << 20
			data, readErr := io.ReadAll(io.LimitReader(file, maxSummaryBytes+1))
			file.Close()
			if readErr != nil {
				http.Error(w, "summary cannot be read", http.StatusInternalServerError)
				return
			}
			if len(data) > maxSummaryBytes {
				http.Error(w, "summary exceeds preview size limit", http.StatusRequestEntityTooLarge)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", filename))
			w.Write(data)
			return
		}
		http.Error(w, "summary file not found; the JSONL checkpoint still contains the summary", http.StatusNotFound)
	}
}
