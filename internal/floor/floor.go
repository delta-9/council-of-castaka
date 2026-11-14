package floor

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/delta-9/council-of-castaka/internal/minds"
)

// Seat is one mind on the floor.
type Seat struct {
	ID     string
	Model  string
	System string
	Mind   minds.Mind
	active bool
}

// NewSeat returns a seat that speaks from the next round.
func NewSeat(id, model, system string, mind minds.Mind) *Seat {
	return &Seat{ID: id, Model: model, System: system, Mind: mind, active: true}
}

// Floor runs the rounds. In a round, every active seat with a non-empty queue
// speaks once, all in parallel, on the same snapshot of the log. Replies land
// as they arrive, and whatever lands during a round waits for the next one.
type Floor struct {
	Log   *Log
	Seats []*Seat

	// OnRound and OnEntry report progress, possibly from several goroutines.
	OnRound func(round int, seats []string)
	OnEntry func(Entry)

	mu      sync.Mutex
	rounds  int // rounds granted to a chair message
	budget  int
	only    map[string]bool
	round   int
	running bool
	cancel  context.CancelFunc
	idle    chan struct{}
}

var mention = regexp.MustCompile(`@([A-Za-z0-9_-]+)`)

// New returns a floor whose round numbers continue from the log.
func New(log *Log, seats []*Seat, rounds int) *Floor {
	f := &Floor{Log: log, Seats: seats, rounds: rounds}
	for _, e := range log.Snapshot() {
		f.round = max(f.round, e.Round)
	}
	return f
}

// Post appends a chair message and grants the council its rounds. A message
// that names seats with @ runs one round in which only they speak.
func (f *Floor) Post(text string) error {
	only := map[string]bool{}
	for _, m := range mention.FindAllStringSubmatch(text, -1) {
		if f.Seat(m[1]) != nil {
			only[m[1]] = true
		}
	}
	// Append and grant under the floor lock: a round's snapshot then sees the
	// message together with its rounds, or neither.
	f.mu.Lock()
	e, err := f.Log.Append(Entry{From: "chair", Kind: "msg", Text: text})
	if err == nil && len(only) > 0 {
		f.budget, f.only = 1, only
	} else if err == nil {
		f.budget, f.only = f.rounds, nil
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	f.report(e)
	f.kick()
	return nil
}

// More grants one more round.
func (f *Floor) More() {
	f.mu.Lock()
	f.budget++
	f.mu.Unlock()
	f.kick()
}

// SetRounds sets the rounds granted to each chair message.
func (f *Floor) SetRounds(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rounds = n
}

// Stop cancels the round in flight, whose turns leave no trace, and the rounds left.
func (f *Floor) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.budget, f.only = 0, nil
	if f.cancel != nil {
		f.cancel()
	}
}

// Busy reports whether rounds are running.
func (f *Floor) Busy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

// Wait blocks until the floor is quiet.
func (f *Floor) Wait() {
	f.mu.Lock()
	idle := f.idle
	f.mu.Unlock()
	if idle != nil {
		<-idle
	}
}

// Seat returns the seat called id, or nil.
func (f *Floor) Seat(id string) *Seat {
	for _, s := range f.Seats {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// SetActive takes a seat off the floor or puts it back; false if there is no such seat.
func (f *Floor) SetActive(id string, on bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.Seat(id)
	if s != nil {
		s.active = on
	}
	return s != nil
}

// Active reports whether a seat speaks in the next rounds.
func (f *Floor) Active(s *Seat) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return s.active
}

func (f *Floor) kick() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		return
	}
	f.running, f.idle = true, make(chan struct{})
	go f.loop()
}

func (f *Floor) loop() {
	for {
		f.mu.Lock()
		entries := f.Log.Snapshot()
		head := len(entries)
		var speakers []*Seat
		for _, s := range f.Seats {
			if !s.active || (f.only != nil && !f.only[s.ID]) {
				continue
			}
			if _, queue := View(entries, s.ID, head); len(queue) > 0 {
				speakers = append(speakers, s)
			}
		}
		if f.budget <= 0 || len(speakers) == 0 {
			f.budget, f.only, f.running, f.cancel = 0, nil, false, nil
			close(f.idle)
			f.mu.Unlock()
			return
		}
		f.budget--
		f.only = nil
		f.round++
		round := f.round
		ctx, cancel := context.WithCancel(context.Background())
		f.cancel = cancel
		f.mu.Unlock()

		ids := make([]string, len(speakers))
		for i, s := range speakers {
			ids[i] = s.ID
		}
		if f.OnRound != nil {
			f.OnRound(round, ids)
		}
		var wg sync.WaitGroup
		for _, s := range speakers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				f.speak(ctx, s, entries, head, round)
			}()
		}
		wg.Wait()
		cancel()
	}
}

// speak runs one seat's turn on the round's snapshot and appends its reply.
func (f *Floor) speak(ctx context.Context, s *Seat, entries []Entry, head, round int) {
	turns, _ := View(entries, s.ID, head)
	start := time.Now()
	reply, err := s.Mind.Speak(ctx, s.System, turns)
	if ctx.Err() != nil {
		return // stopped by the chair: the turn leaves no trace
	}
	e := Entry{From: s.ID, Seen: head, Round: round, Model: s.Model, Served: reply.Served, Ms: time.Since(start).Milliseconds()}
	switch {
	case err != nil:
		e.Kind, e.Text = "error", err.Error()
	case strings.TrimSpace(reply.Text) == "":
		e.Kind, e.Text = "error", "empty reply ("+reply.Stop+")"
	case IsPass(reply.Text):
		e.Kind, e.Text, e.Raw = "pass", reply.Text, reply.Raw
	default:
		e.Kind, e.Text, e.Raw = "msg", reply.Text, reply.Raw
	}
	if err == nil {
		e.Usage = &reply.Usage
	}
	stored, err := f.Log.Append(e)
	if err != nil {
		stored = Entry{From: s.ID, Kind: "error", Round: round, Text: "log write failed: " + err.Error()}
	}
	f.report(stored)
}

func (f *Floor) report(e Entry) {
	if f.OnEntry != nil {
		f.OnEntry(e)
	}
}
