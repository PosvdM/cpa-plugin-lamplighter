package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Record is one line of a history file: a quota sample or an event.
type Record struct {
	Kind      string  `json:"k"`           // "s" for a sample, "e" for an event
	Time      int64   `json:"t"`           // Unix seconds
	Group     string  `json:"g,omitempty"` // quota group key
	Window    string  `json:"w,omitempty"` // window ID, samples only
	Remaining float64 `json:"r,omitempty"` // remaining percent, samples only
	Reset     int64   `json:"x,omitempty"` // reset time in Unix seconds, samples only
	Source    string  `json:"s,omitempty"` // "active" or "passive", samples only
	Event     string  `json:"e,omitempty"` // event type, events only
	Level     string  `json:"v,omitempty"` // "info", "warn" or "error", events only
	Message   string  `json:"m,omitempty"` // event text, events only
	Label     string  `json:"l,omitempty"` // quota group or source label, events only
	Detail    string  `json:"d,omitempty"` // event text without the label, events only
}

// History stores records in one JSON Lines file per UTC day under Dir.
type History struct {
	Dir string
	mu  sync.Mutex
}

func (h *History) fileFor(t time.Time) string {
	return filepath.Join(h.Dir, t.UTC().Format("2006-01-02")+".jsonl")
}

// Append writes records to the file of their day.
func (h *History) Append(records ...Record) error {
	if len(records) == 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := os.MkdirAll(h.Dir, 0o755); err != nil {
		return err
	}
	byFile := map[string][]Record{}
	for _, record := range records {
		path := h.fileFor(time.Unix(record.Time, 0))
		byFile[path] = append(byFile[path], record)
	}
	for path, items := range byFile {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		writer := bufio.NewWriter(file)
		for _, item := range items {
			raw, err := json.Marshal(item)
			if err != nil {
				continue
			}
			writer.Write(raw)
			writer.WriteByte('\n')
		}
		flushErr := writer.Flush()
		closeErr := file.Close()
		if flushErr != nil {
			return flushErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// Query returns the records between from and to, oldest first.
func (h *History) Query(from, to time.Time) ([]Record, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Record
	for day := from.UTC().Truncate(24 * time.Hour); !day.After(to); day = day.Add(24 * time.Hour) {
		file, err := os.Open(h.fileFor(day))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			var record Record
			if json.Unmarshal(scanner.Bytes(), &record) != nil {
				continue
			}
			if record.Time < from.Unix() || record.Time > to.Unix() {
				continue
			}
			out = append(out, record)
		}
		file.Close()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out, nil
}

// Prune deletes day files older than retention.
func (h *History) Prune(now time.Time, retention time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	entries, err := os.ReadDir(h.Dir)
	if err != nil {
		return nil
	}
	cutoff := now.UTC().Add(-retention).Truncate(24 * time.Hour)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		day, err := time.Parse("2006-01-02", strings.TrimSuffix(name, ".jsonl"))
		if err != nil {
			continue
		}
		if day.Before(cutoff) {
			os.Remove(filepath.Join(h.Dir, name))
		}
	}
	return nil
}
