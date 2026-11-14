package minds

import (
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"
)

var fromRe = regexp.MustCompile(`<msg from="([^"]+)">`)

// fake answers with what it was shown, after a random delay, so the floor can
// be exercised for free. It passes once it has spoken twice and the chair is
// silent.
type fake struct{ seat string }

func (f fake) Speak(ctx context.Context, system string, turns []Turn) (Reply, error) {
	select {
	case <-time.After(time.Duration(100+rand.IntN(700)) * time.Millisecond):
	case <-ctx.Done():
		return Reply{}, ctx.Err()
	}
	var heard []string
	chair := false
	for _, m := range fromRe.FindAllStringSubmatch(turns[len(turns)-1].Text, -1) {
		heard = append(heard, m[1])
		chair = chair || m[1] == "chair"
	}
	if spoken := len(turns) / 2; spoken >= 2 && !chair {
		return Reply{Text: "PASS", Served: "fake"}, nil
	}
	return Reply{Text: fmt.Sprintf("%s heard %s (view of %d turns).", f.seat, strings.Join(heard, ", "), len(turns)), Served: "fake"}, nil
}
