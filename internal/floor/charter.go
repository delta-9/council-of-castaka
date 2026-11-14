package floor

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/delta-9/council-of-castaka/internal/minds"
)

const charter = `You are {seat} ({model}) on a council: {roster}.
The chair brings a technical problem and decides; the council argues it out.
Messages since your last turn arrive as <msg from="name">…</msg>, oldest first.

- Speak only for yourself: no lines for another member, no tags around your reply.
- The council does not need to agree. If someone is wrong, say so and why.
  If you were wrong, say so and what changed your mind.
- Add, don't echo: a fact, a counter-example, a risk, a test, a number, a better
  question. No compliments, no recap of what others said.
- Concrete beats principled: code, commands, numbers, failure scenarios.
- Wild ideas are welcome: flag them, and defend them when challenged.
- Stay under {words} words unless the chair asks for depth.
- Nothing new to add? Reply exactly: PASS
`

// Charter returns a seat's system prompt. A session stores it once per seat
// and reuses it when resumed, so the prompt never changes under replayed turns.
func Charter(seat minds.Spec, roster []minds.Spec, words int) string {
	members := make([]string, len(roster))
	for i, s := range roster {
		members[i] = fmt.Sprintf("%s (%s, %s)", s.Seat, s.Model, minds.Lab(s.Provider))
	}
	prompt := strings.NewReplacer(
		"{seat}", seat.Seat,
		"{model}", seat.Model,
		"{roster}", strings.Join(members, ", "),
		"{words}", strconv.Itoa(words),
	).Replace(charter)
	if seat.Card != "" {
		prompt += "\n" + seat.Card + "\n"
	}
	return prompt
}
