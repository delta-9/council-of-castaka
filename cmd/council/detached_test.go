package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDetachedSession checks the contract the GUI attaches by, on the real
// binary with the fake mind: the chair's lines go into the session's chair
// pipe, the events come out in events.jsonl, running.json marks the session
// while it runs, and /quit ends it with an end event.
func TestDetachedSession(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	bin := filepath.Join(t.TempDir(), "council")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	home := t.TempDir()
	dir := filepath.Join(home, "session")
	cmd := exec.Command(bin, "--detached", "-fake", "-resume", dir)
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Dir = home // not a repository: the minds get no tools
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	events := filepath.Join(dir, "events.jsonl")
	waitFor(t, events, `"type":"session"`)
	if _, err := os.Stat(filepath.Join(dir, "running.json")); err != nil {
		t.Errorf("no running.json while the session runs: %v", err)
	}
	chair(t, dir, "@morkai hello")
	waitFor(t, events, `"from":"opus"`)
	chair(t, dir, "/quit")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the council exited with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("/quit did not end the session")
	}

	var types []string
	f, err := os.Open(events)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for in := bufio.NewScanner(f); in.Scan(); {
		var e struct{ Type string }
		if err := json.Unmarshal(in.Bytes(), &e); err != nil {
			t.Fatalf("not a JSON event: %s", in.Text())
		}
		types = append(types, e.Type)
	}
	got := strings.Join(types, " ")
	for _, want := range []string{"session", "entry", "round", "cost"} {
		if !strings.Contains(got, want) {
			t.Errorf("events %q lack %s", got, want)
		}
	}
	if len(types) == 0 || types[len(types)-1] != "end" {
		t.Errorf("events %q do not end with end", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "running.json")); !os.IsNotExist(err) {
		t.Errorf("running.json outlived the session: %v", err)
	}
}

// chair writes one line into the session's chair pipe, as the GUI does.
func chair(t *testing.T, dir, line string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "chair"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// waitFor waits up to 15 s for text to appear in the file at path.
func waitFor(t *testing.T, path, text string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), text) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("%s never showed %s:\n%s", path, text, b)
}
