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
Today is {date}.{tools}

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

const toolRules = " Check facts before you assert them, and cite the files, commits or URLs you rely on." +
	" What you read stays private to you: quote what matters, since the others see only what you write."

// label is what the minds call a seat: its name, or its ID when it has none.
func label(s minds.Spec) string {
	if s.Name != "" {
		return s.Name
	}
	return s.Seat
}

// Charter returns a seat's system prompt. A session stores it once per seat
// and reuses it when resumed, so the prompt never changes under replayed
// turns. repo is the repository the minds can read, if any.
func Charter(seat minds.Spec, roster []minds.Spec, words int, date, repo string) string {
	members := make([]string, len(roster))
	for i, s := range roster {
		members[i] = fmt.Sprintf("%s (%s, %s)", label(s), s.Model, minds.Lab(s.Provider))
	}
	read := "You can read the repository " + repo + " as the chair has it: read_file shows a file git tracks," +
		" and git runs read-only commands (log, show, diff, blame, grep, ls-files…). Untracked and ignored files are out of reach."
	tools := ""
	switch search := seat.Search != "off"; {
	case repo != "" && search:
		tools = "\n\n" + read + " You can also search the web for fresh facts." + toolRules
	case repo != "":
		tools = "\n\n" + read + toolRules
	case search:
		tools = "\n\nYou can search the web for fresh facts." + toolRules
	}
	prompt := strings.NewReplacer(
		"{seat}", label(seat),
		"{model}", seat.Model,
		"{roster}", strings.Join(members, ", "),
		"{date}", date,
		"{tools}", tools,
		"{words}", strconv.Itoa(words),
	).Replace(charter)
	if seat.Card != "" {
		prompt += "\n" + seat.Card + "\n"
	}
	return prompt
}
