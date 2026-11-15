package minds

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

var errStalled = errors.New("stalled: no bytes from the API")

// watchdog fails a request that receives no bytes for idle: while it waits
// for its headers, and between reads of its body. Pings count as bytes, so a
// healthy stream that is thinking quietly stays alive as long as the API
// sends them, and the headers count too, so the body's first read gets a full
// window. The timer cancels a context of the request's own, never the
// chair's, because the floor drops the turns the chair cancelled.
type watchdog struct {
	base http.RoundTripper
	idle time.Duration
}

func (w watchdog) RoundTrip(r *http.Request) (*http.Response, error) {
	quiet, _ := r.Context().Value(silenceKey{}).(*silence)
	// Each time is taken before the timer is armed, so a stall always notes a
	// silence of at least idle.
	sent := time.Now()
	ctx, cancel := context.WithCancelCause(r.Context())
	timer := time.AfterFunc(w.idle, func() { cancel(errStalled) })
	resp, err := w.base.RoundTrip(r.WithContext(ctx))
	headers := time.Now()
	if err != nil {
		quiet.fail(headers.Sub(sent))
		timer.Stop()
		if context.Cause(ctx) == errStalled {
			err = errStalled
		}
		cancel(nil)
		return nil, err
	}
	quiet.note(headers.Sub(sent))
	timer.Reset(w.idle)
	resp.Body = &watched{ReadCloser: resp.Body, ctx: ctx, cancel: cancel, timer: timer, idle: w.idle, quiet: quiet, last: headers}
	return resp, nil
}

// watched is a response body that pushes the stall timer back on every read
// that returns data, and notes the gaps between reads: apart, the one a
// failure ended. A read that fails comes back as a wireError.
type watched struct {
	io.ReadCloser
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer
	idle   time.Duration
	quiet  *silence
	last   time.Time
}

func (b *watched) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		now := time.Now()
		b.quiet.note(now.Sub(b.last))
		b.last = now
		b.timer.Reset(b.idle)
	}
	if err != nil {
		// Whatever ended the read (the stall timer, a lost connection, the
		// chair, a stream that ended too early), the gap before it was a
		// silence a failure ended.
		b.quiet.fail(time.Since(b.last))
		if context.Cause(b.ctx) == errStalled {
			err = errStalled
		}
		if err != io.EOF {
			err = wireError{err}
		}
	}
	return n, err
}

// wireError is a read of a response body that failed: a lost connection, a
// stall, a reset stream. It tells a broken stream from an event the SDK
// cannot decode. io.EOF stays bare, since readers compare it with ==.
type wireError struct{ err error }

func (e wireError) Error() string { return e.err.Error() }
func (e wireError) Unwrap() error { return e.err }

func (b *watched) Close() error {
	b.timer.Stop()
	b.cancel(nil)
	return b.ReadCloser.Close()
}

// silence keeps a request's longest silences, the wait for its headers and
// the gaps between reads of its body, by what ended them: data or headers
// (the healthy figure the idle limit is calibrated on), or a failure. It rides
// on the request's context, shared by the SDK's retries, so a turn can report
// both.
type silence struct {
	mu      sync.Mutex
	longest time.Duration
	cut     time.Duration
}

type silenceKey struct{}

func (s *silence) note(d time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.longest = max(s.longest, d)
}

// fail notes a silence that a failure ended.
func (s *silence) fail(d time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cut = max(s.cut, d)
}

func (s *silence) max() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.longest
}

func (s *silence) maxCut() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cut
}
