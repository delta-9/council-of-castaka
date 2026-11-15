package floor

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delta-9/council-of-castaka/internal/minds"
)

// failing is a mind whose turn fails after it has paid for some requests.
type failing struct{ usage minds.Usage }

func (f failing) Speak(ctx context.Context, system string, turns []minds.Turn) (minds.Reply, error) {
	return minds.Reply{Usage: f.usage}, errors.New("stalled mid-turn")
}

// counting fails every turn and counts its turns.
type counting struct{ turns *atomic.Int32 }

func (c counting) Speak(ctx context.Context, system string, turns []minds.Turn) (minds.Reply, error) {
	c.turns.Add(1)
	return minds.Reply{}, errors.New("down")
}

// TestFailedTurnRunsAgain checks that a failed turn leaves its seat's queue
// intact, so the next round runs the whole turn again.
func TestFailedTurnRunsAgain(t *testing.T) {
	log, err := OpenLog(filepath.Join(t.TempDir(), "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var n atomic.Int32
	f := New(log, []*Seat{NewSeat("opus", "m", "", counting{turns: &n})}, 2)
	if _, err := f.Post("hello"); err != nil {
		t.Fatal(err)
	}
	f.Wait()
	if n.Load() != 2 {
		t.Fatalf("the failing seat spoke %d times in two rounds, want 2", n.Load())
	}
}

// listening answers every turn and counts them.
type listening struct{ turns *atomic.Int32 }

func (l listening) Speak(ctx context.Context, system string, turns []minds.Turn) (minds.Reply, error) {
	l.turns.Add(1)
	return minds.Reply{Text: "heard"}, nil
}

// TestMentionByName checks that the chair can mention a seat by its name,
// case and accents aside, and that only that seat speaks.
func TestMentionByName(t *testing.T) {
	log, err := OpenLog(filepath.Join(t.TempDir(), "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var hurl, noren atomic.Int32
	carbon := NewSeat("carbon", "m", "", listening{turns: &hurl})
	carbon.Name = "Hûrl"
	fable := NewSeat("fable", "m", "", listening{turns: &noren})
	fable.Name = "Noren"
	f := New(log, []*Seat{carbon, fable}, 1)
	if _, err := f.Post("@hurl what breaks first?"); err != nil {
		t.Fatal(err)
	}
	f.Wait()
	if hurl.Load() != 1 || noren.Load() != 0 {
		t.Fatalf("Hûrl spoke %d times and Noren %d after @hurl, want 1 and 0", hurl.Load(), noren.Load())
	}
}

// TestQuietAfterRounds checks that the floor reports when its rounds end, once
// per stretch of rounds, after it is free for new ones: a review session folds
// on that signal, and its fold must be able to start a round.
func TestQuietAfterRounds(t *testing.T) {
	log, err := OpenLog(filepath.Join(t.TempDir(), "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var turns atomic.Int32
	f := New(log, []*Seat{NewSeat("opus", "m", "", listening{turns: &turns})}, 2)
	quiet := make(chan bool, 4)
	f.OnQuiet = func() { quiet <- f.Busy() }
	if _, err := f.Post("hello"); err != nil {
		t.Fatal(err)
	}
	if busy := <-quiet; busy {
		t.Fatal("the floor reported quiet while still busy")
	}
	if len(quiet) != 0 || turns.Load() != 1 {
		t.Fatalf("%d more quiet reports and %d turns, want none and one", len(quiet), turns.Load())
	}
}

// retried answers after a retry, with silences of both kinds.
type retried struct{}

func (retried) Speak(ctx context.Context, system string, turns []minds.Turn) (minds.Reply, error) {
	return minds.Reply{Text: "answered", Retries: []string{"a broken stream: http2: client connection lost"},
		Quiet: 2 * time.Second, Cut: 60 * time.Second}, nil
}

// TestRetriesReachTheLog checks that a turn that recovered on a retry says so
// in the log, and keeps the silence the failure ended apart from the healthy
// one the idle limit is calibrated on.
func TestRetriesReachTheLog(t *testing.T) {
	log, err := OpenLog(filepath.Join(t.TempDir(), "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	f := New(log, []*Seat{NewSeat("opus", "m", "", retried{})}, 1)
	if _, err := f.Post("hello"); err != nil {
		t.Fatal(err)
	}
	f.Wait()
	entries := log.Snapshot()
	last := entries[len(entries)-1]
	if last.Kind != "msg" || len(last.Retries) != 1 || last.QuietMs != 2000 || last.CutMs != 60000 {
		t.Fatalf("entry %+v, want the answer with its retry, 2000 ms of healthy silence and 60000 cut", last)
	}
}

// blocking waits until the chair stops the round.
type blocking struct{ started chan struct{} }

func (b blocking) Speak(ctx context.Context, system string, turns []minds.Turn) (minds.Reply, error) {
	close(b.started)
	<-ctx.Done()
	return minds.Reply{Usage: minds.Usage{Input: 3}}, ctx.Err()
}

// TestStopLeavesNoTrace checks that a turn the chair stops leaves no entry.
func TestStopLeavesNoTrace(t *testing.T) {
	log, err := OpenLog(filepath.Join(t.TempDir(), "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	b := blocking{started: make(chan struct{})}
	f := New(log, []*Seat{NewSeat("opus", "m", "", b)}, 1)
	if _, err := f.Post("hello"); err != nil {
		t.Fatal(err)
	}
	<-b.started
	f.Stop()
	f.Wait()
	if entries := log.Snapshot(); len(entries) != 1 {
		t.Fatalf("entries after a stop: %+v, want only the chair's message", entries)
	}
}

// TestFailedTurnKeepsUsage checks that a turn failing after some steps still
// records what those steps cost, so /cost does not under-report.
func TestFailedTurnKeepsUsage(t *testing.T) {
	log, err := OpenLog(filepath.Join(t.TempDir(), "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	paid := minds.Usage{Input: 7, Output: 5}
	f := New(log, []*Seat{NewSeat("opus", "m", "", failing{usage: paid})}, 1)
	if _, err := f.Post("hello"); err != nil {
		t.Fatal(err)
	}
	f.Wait()
	entries := log.Snapshot()
	last := entries[len(entries)-1]
	if last.Kind != "error" || last.Usage == nil || *last.Usage != paid {
		t.Fatalf("failed turn = %+v, want an error entry carrying usage %+v", last, paid)
	}
}
