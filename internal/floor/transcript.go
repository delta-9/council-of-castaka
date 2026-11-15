package floor

import (
	"fmt"
	"strconv"
	"strings"
)

// Transcript renders the session as Markdown.
func Transcript(title string, seats []*Seat, entries []Entry) string {
	var b strings.Builder
	members := make([]string, len(seats))
	names := map[string]string{}
	for i, s := range seats {
		members[i] = s.Label() + " (" + s.Model + ")"
		names[s.ID] = s.Label()
	}
	fmt.Fprintf(&b, "# %s\n\n*%s*\n\n---\n", title, strings.Join(members, " · "))
	for _, e := range entries {
		from := e.From
		if name := names[e.From]; name != "" {
			from = name
		}
		switch {
		case e.Kind == "msg" && e.From == "chair":
			fmt.Fprintf(&b, "\n**chair** · #%d\n\n> %s\n", e.Seq, strings.ReplaceAll(e.Text, "\n", "\n> "))
		case e.Kind == "msg":
			fmt.Fprintf(&b, "\n### %s · round %d · #%d\n\n*%s*\n", from, e.Round, e.Seq, Meta(e))
			if n := len(e.Calls); n > 0 {
				noun := "calls"
				if n == 1 {
					noun = "call"
				}
				fmt.Fprintf(&b, "\n*%d %s: %s*\n", n, noun, strings.Join(e.Calls, " · "))
			}
			fmt.Fprintf(&b, "\n%s\n", e.Text)
		case e.Kind == "pass":
			fmt.Fprintf(&b, "\n*%s passes · round %d · #%d*\n", from, e.Round, e.Seq)
		case e.Kind == "error":
			fmt.Fprintf(&b, "\n> **%s** · round %d · #%d · error: %s\n", from, e.Round, e.Seq, strings.ReplaceAll(e.Text, "\n", " "))
		}
	}
	return b.String()
}

// Meta describes a seat's turn: model, time and tokens.
func Meta(e Entry) string {
	parts := []string{e.Model}
	if e.Served != "" && !strings.HasPrefix(e.Served, e.Model) {
		parts = append(parts, "served by "+e.Served)
	}
	parts = append(parts, fmt.Sprintf("%.1fs", float64(e.Ms)/1000))
	if u := e.Usage; u != nil {
		parts = append(parts, "in "+tokens(u.Input+u.CacheRead+u.CacheWrite))
		if u.CacheRead > 0 {
			parts = append(parts, "cached "+tokens(u.CacheRead))
		}
		parts = append(parts, "out "+tokens(u.Output))
	}
	return strings.Join(parts, " · ")
}

func tokens(n int64) string {
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}
