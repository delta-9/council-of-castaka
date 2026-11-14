// Package floor holds the council's state and turn-taking: one append-only
// log, a view per seat rebuilt from it on every turn, and rounds in which the
// seats with something new to read speak in parallel.
package floor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/delta-9/council-of-castaka/internal/minds"
)

// Entry is one line of log.jsonl.
type Entry struct {
	Seq    int             `json:"seq"`
	TS     time.Time       `json:"ts"`
	From   string          `json:"from"` // "chair" or a seat
	Kind   string          `json:"kind"` // "msg", "pass" or "error"
	Text   string          `json:"text,omitempty"`
	Seen   int             `json:"seen,omitempty"` // a seat's turn: the last seq it had read
	Round  int             `json:"round,omitempty"`
	Model  string          `json:"model,omitempty"`  // the seat's configured model
	Served string          `json:"served,omitempty"` // the model that answered, as reported
	Ms     int64           `json:"ms,omitempty"`
	Usage  *minds.Usage    `json:"usage,omitempty"`
	Raw    json.RawMessage `json:"raw,omitempty"` // the native reply, for replay
}

// Log is the session's append-only record. An entry is written to disk before
// anyone can read it, so quitting or crashing loses nothing.
type Log struct {
	mu      sync.Mutex
	f       *os.File
	entries []Entry
}

// OpenLog loads path, if it exists, and opens it for appending. A last line
// cut short by a crash is dropped.
func OpenLog(path string) (*Log, error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	l := &Log{}
	good := 0
	for good < len(data) {
		end := bytes.IndexByte(data[good:], '\n')
		if end < 0 {
			break
		}
		var e Entry
		if err := json.Unmarshal(data[good:good+end], &e); err != nil {
			return nil, fmt.Errorf("%s: line %d: %w", path, len(l.entries)+1, err)
		}
		l.entries = append(l.entries, e)
		good += end + 1
	}
	if good < len(data) {
		if err := os.Truncate(path, int64(good)); err != nil {
			return nil, err
		}
	}
	if l.f, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err != nil {
		return nil, err
	}
	return l, nil
}

// Append numbers e, writes it and returns it as stored.
func (l *Log) Append(e Entry) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Seq = len(l.entries) + 1
	e.TS = time.Now().UTC()
	line, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		return e, err
	}
	l.entries = append(l.entries, e)
	return e, nil
}

// Snapshot returns a copy of every entry so far; the last one's Seq is the head.
func (l *Log) Snapshot() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Entry(nil), l.entries...)
}

// Close closes the file.
func (l *Log) Close() error {
	return l.f.Close()
}
