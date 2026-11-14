package floor

import (
	"fmt"
	"strings"

	"github.com/delta-9/council-of-castaka/internal/minds"
)

// View is what seat receives on its next turn when the log reads up to head:
// each of its own past turns as an assistant turn, preceded by the others'
// messages it had not read yet when it spoke, then the queue: the others'
// messages it has still not read. A turn's Seen marks what it had read, so a
// reply that landed before the seat's own in the same round still reaches it
// next turn. Passes and errors are never delivered; an empty queue means the
// seat has nothing to answer.
func View(entries []Entry, seat string, head int) (turns []minds.Turn, queue []int) {
	read := 0
	for _, e := range entries {
		if e.From != seat || (e.Kind != "msg" && e.Kind != "pass") {
			continue
		}
		text, _ := batch(entries, seat, read, e.Seen)
		turns = append(turns,
			minds.Turn{Role: "user", Text: text},
			minds.Turn{Role: "assistant", Text: e.Text, Raw: e.Raw, Model: e.Model})
		read = e.Seen
	}
	text, queue := batch(entries, seat, read, head)
	if len(queue) > 0 {
		turns = append(turns, minds.Turn{Role: "user", Text: text})
	}
	return turns, queue
}

// batch renders the others' messages with lo < seq <= hi, oldest first.
func batch(entries []Entry, seat string, lo, hi int) (string, []int) {
	var b strings.Builder
	var seqs []int
	for _, e := range entries {
		if e.Seq <= lo || e.Seq > hi || e.From == seat || e.Kind != "msg" {
			continue
		}
		if len(seqs) > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "<msg from=\"%s\">\n%s\n</msg>", e.From, e.Text)
		seqs = append(seqs, e.Seq)
	}
	if len(seqs) == 0 {
		// Unreachable for a past turn: a seat speaks only with a queue.
		return "(nothing new)", nil
	}
	return b.String(), seqs
}

// IsPass reports whether a reply is the charter's PASS, give or take markup.
func IsPass(text string) bool {
	return strings.EqualFold(strings.Trim(text, " \t\r\n*_`.!\"'"), "PASS")
}
