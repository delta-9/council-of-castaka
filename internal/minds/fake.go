package minds

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"
)

var fromRe = regexp.MustCompile(`<msg from="([^"]+)">`)

// fake answers with what it was shown, after a random delay, so the floor can
// be exercised for free. With the git tool it also reads the last commit, so
// the tool plumbing runs too. It passes once it has spoken twice and the
// chair is silent.
type fake struct {
	seat string
	opt  Options
}

func (f fake) Speak(ctx context.Context, system string, turns []Turn) (Reply, error) {
	select {
	case <-time.After(time.Duration(100+rand.IntN(700)) * time.Millisecond):
	case <-ctx.Done():
		return Reply{}, ctx.Err()
	}
	reply := Reply{Served: "fake"}
	var heard []string
	chair := false
	for _, m := range fromRe.FindAllStringSubmatch(turns[len(turns)-1].Text, -1) {
		heard = append(heard, m[1])
		chair = chair || m[1] == "chair"
	}
	if spoken := len(turns) / 2; spoken >= 2 && !chair {
		reply.Text = "PASS"
		return reply, nil
	}
	reply.Text = fmt.Sprintf("%s heard %s (view of %d turns).", f.seat, strings.Join(heard, ", "), len(turns))
	for _, t := range f.opt.Tools.Tools() {
		if t.Name == "git" {
			out, _ := call(ctx, f.opt, &reply, "git", json.RawMessage(`{"args":["log","-n","1","--format=%h %s"]}`))
			reply.Text += " Last commit: " + strings.TrimSpace(out) + "."
		}
	}
	return reply, nil
}
