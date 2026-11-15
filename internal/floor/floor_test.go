package floor

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/delta-9/council-of-castaka/internal/minds"
)

// failing is a mind whose turn fails after it has paid for some requests.
type failing struct{ usage minds.Usage }

func (f failing) Speak(ctx context.Context, system string, turns []minds.Turn) (minds.Reply, error) {
	return minds.Reply{Usage: f.usage}, errors.New("stalled mid-turn")
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
