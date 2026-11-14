package floor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/delta-9/council-of-castaka/internal/minds"
)

// failing is a mind whose turn fails after it has paid for some requests.
type failing struct{ usage minds.Usage }

func (f failing) Speak(ctx context.Context, system string, turns []minds.Turn) (minds.Reply, error) {
	return minds.Reply{Usage: f.usage}, errors.New("stalled mid-turn")
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
