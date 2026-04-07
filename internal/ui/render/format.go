package render

import "strings"

// AnsiFormat converts markdown-style inline formatting to ANSI escape codes.
// Handles **bold** and *italic* (including _italic_).
func AnsiFormat(text string) string {
	// Process **bold** first (before *italic* to avoid conflict).
	text = replaceDelimited(text, "**", "\033[1m", "\033[22m")
	// Then *italic*.
	text = replaceDelimited(text, "*", "\033[3m", "\033[23m")
	text = replaceDelimited(text, "_", "\033[3m", "\033[23m")
	return text
}

// replaceDelimited finds pairs of delimiter and wraps their content with ANSI codes.
func replaceDelimited(text, delim, open, close string) string {
	var b strings.Builder
	rest := text
	for {
		start := strings.Index(rest, delim)
		if start == -1 {
			b.WriteString(rest)
			break
		}
		end := strings.Index(rest[start+len(delim):], delim)
		if end == -1 {
			b.WriteString(rest)
			break
		}
		end += start + len(delim)
		// Don't format if the content is empty or just whitespace.
		inner := rest[start+len(delim) : end]
		if strings.TrimSpace(inner) == "" {
			b.WriteString(rest[:end+len(delim)])
			rest = rest[end+len(delim):]
			continue
		}
		b.WriteString(rest[:start])
		b.WriteString(open)
		b.WriteString(inner)
		b.WriteString(close)
		rest = rest[end+len(delim):]
	}
	return b.String()
}
