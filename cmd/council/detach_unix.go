//go:build unix

package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// detach makes the session folder the council's way in and out, so whoever
// starts it can go away and attach again: the chair's lines come through the
// pipe chair, the events go to events.jsonl, and running.json says the
// session runs and in which process. The pipe is open for reading and
// writing, so it never ends when a writer leaves. cleanup removes the pipe,
// which a writer then cannot open, and running.json.
func detach(dir string) (io.Reader, io.Writer, func(), error) {
	pipe := filepath.Join(dir, "chair")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, nil, nil, err
	}
	chair, err := os.OpenFile(pipe, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	events, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		chair.Close()
		return nil, nil, nil, err
	}
	running := filepath.Join(dir, "running.json")
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "started": time.Now().UTC()})
	if err := os.WriteFile(running, b, 0o600); err != nil {
		chair.Close()
		events.Close()
		return nil, nil, nil, err
	}
	return chair, events, func() {
		os.Remove(running)
		os.Remove(pipe)
		chair.Close()
		events.Close()
	}, nil
}
