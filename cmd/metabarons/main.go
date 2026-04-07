// Command metabarons invokes the Council of the Castaka as a standalone CLI.
//
// Usage:
//
//	metabarons -m "matter" [-o ~/Documents/] [-q]
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/delta-9/council-of-castaka/internal/council"

	"golang.org/x/term"
)

func main() {
	var (
		matter     string
		outDir     string
		quiet      bool
		trimReport bool
	)

	flag.StringVar(&matter, "m", "", "the matter to deliberate")
	flag.StringVar(&outDir, "o", ".", "output directory for the markdown transcript")
	flag.BoolVar(&quiet, "q", false, "quiet mode — save report and print path, no other output")
	flag.BoolVar(&trimReport, "t", false, "trim report — only show the final word (scrutiny round)")
	flag.Parse()

	// If matter not provided via -m, use remaining args.
	if matter == "" {
		matter = strings.Join(flag.Args(), " ")
	}
	if matter == "" {
		fmt.Fprintln(os.Stderr, "usage: metabarons -m \"matter\" [-o dir] [-q] [-t]")
		os.Exit(1)
	}

	logLevel := slog.LevelInfo
	if quiet {
		logLevel = slog.LevelError
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))

	if !quiet {
		fmt.Fprintf(os.Stderr, "Summoning the Council of the Castaka...\n\n")
	}

	cwd, _ := os.Getwd()
	result, err := council.Invoke(context.Background(), council.CouncilRequest{
		Matter:       matter,
		PeerPressure: true,
		Scrutiny:     true,
	}, cwd, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if quiet {
		if trimReport {
			path, saveErr := result.SaveMarkdownFinal(outDir, matter)
			if saveErr != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", saveErr)
				os.Exit(1)
			}
			fmt.Println(path)
			return
		}
		path, saveErr := result.SaveMarkdown(outDir, matter)
		if saveErr != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", saveErr)
			os.Exit(1)
		}
		fmt.Println(path)
		return
	}

	// Terminal width for word wrapping.
	termW := 80
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		termW = w
	}
	maxW := termW - 4
	if maxW < 40 {
		maxW = 40
	}

	// ANSI colors.
	gold := "\033[38;2;240;200;100m"
	dim := "\033[38;2;185;185;185m"
	body := "\033[38;2;220;220;220m"
	rst := "\033[0m"

	// Build sections for interactive reveal.
	var sections []string

	// Section 0: header + matter + members.
	var names []string
	for _, m := range result.Members {
		names = append(names, m.Name)
	}
	var hdr strings.Builder
	hdr.WriteString(fmt.Sprintf("\n%s═══════════════════════════════════════%s\n", gold, rst))
	hdr.WriteString(fmt.Sprintf("%s  COUNCIL OF THE CASTAKA%s\n", gold, rst))
	hdr.WriteString(fmt.Sprintf("%s═══════════════════════════════════════%s\n\n", gold, rst))
	hdr.WriteString(fmt.Sprintf("%s  The matter:%s\n", dim, rst))
	for _, line := range wrap(matter, maxW-4) {
		hdr.WriteString(fmt.Sprintf("  %s> %s%s\n", dim, line, rst))
	}
	hdr.WriteString(fmt.Sprintf("\n%s  Members summoned: %s%s\n", dim, strings.Join(names, ", "), rst))
	sections = append(sections, hdr.String())

	// Display only the final word (scrutiny round). Fall back to members if not present.
	display := result.Scrutiny
	if len(display) == 0 {
		display = result.Members
	}
	for _, ms := range display {
		var s strings.Builder
		s.WriteString(fmt.Sprintf("\n%s─── %s ──────%s\n\n", gold, ms.Name, rst))
		// Private exchanges.
		// messageReceived := []council.PrivateExchange{}
		// for _, ex := range result.Exchanges {
		// 	if ex.ToKey == ms.Key {
		// 		messageReceived = append(messageReceived, ex)
		// 	}
		// }
		// if len(messageReceived) > 0 {
		// 	s.WriteString("\n")
		// 	s.WriteString("**Private messages received:**\n\n")

		// 	for _, ex := range messageReceived {
		// 		s.WriteString(fmt.Sprintf("---\n\nFrom %s:\n", ex.From))
		// 		for _, line := range wrap(council.AnsiFormat(ex.Message), maxW-4) {
		// 			s.WriteString(fmt.Sprintf("  %s> %s%s\n", dim, line, rst))
		// 		}
		// 		if ex.Reply != "" {
		// 			s.WriteString("Replied:\n")
		// 			for _, line := range wrap(council.AnsiFormat(ex.Reply), maxW-4) {
		// 				s.WriteString(fmt.Sprintf("  %s> %s%s\n", dim, line, rst))
		// 			}
		// 		} else {
		// 			s.WriteString("*[silence]*\n\n")
		// 		}
		// 	}
		// 	s.WriteString("\n---\n\n")
		// }
		for _, paragraph := range strings.Split(ms.Statement, "\n") {
			if paragraph == "" {
				s.WriteString("\n")
				continue
			}
			for _, line := range wrap(council.AnsiFormat(paragraph), maxW) {
				s.WriteString(fmt.Sprintf("  %s%s%s\n", body, line, rst))
			}
		}
		sections = append(sections, s.String())
	}

	// Closing.
	sections = append(sections, fmt.Sprintf("\n%s═══════════════════════════════════════%s\n\n", gold, rst)+
		fmt.Sprintf("  %sThe council does not reconvene on the same matter.%s\n", dim, rst)+
		fmt.Sprintf("  %sWhat is decided here stands.%s\n", dim, rst))

	// Put terminal in raw mode to read keypresses.
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		// Fallback: print everything at once.
		for _, s := range sections {
			fmt.Print(s)
		}
	} else {
		defer term.Restore(int(os.Stdin.Fd()), oldState)

		buf := make([]byte, 1)
		for i, s := range sections {
			// In raw mode, \n doesn't return to column 0 — replace with \r\n.
			fmt.Print(strings.ReplaceAll(s, "\n", "\r\n"))
			if i < len(sections)-1 {
				remaining := len(sections) - i - 1
				fmt.Printf("\r\n\033[38;2;185;185;185m  [Space] continue (%d remaining)  │  [q] quit\033[0m", remaining)
				for {
					os.Stdin.Read(buf)
					if buf[0] == ' ' {
						// Clear the prompt line.
						fmt.Print("\r\033[K")
						break
					}
					if buf[0] == 'q' || buf[0] == 27 { // q or Esc
						fmt.Print("\r\033[K\r\n")
						term.Restore(int(os.Stdin.Fd()), oldState)
						goto done
					}
				}
			}
		}
	}

done:
	// Restore terminal before printing save message.
	if oldState != nil {
		term.Restore(int(os.Stdin.Fd()), oldState)
	}
	fmt.Println()

	// Save markdown.
	path, err := result.SaveMarkdown(outDir, matter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not save transcript: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "Transcript saved to %s\n", path)
	}
}

// wrap splits text into lines no wider than maxWidth, breaking on spaces.
func wrap(text string, maxWidth int) []string {
	if maxWidth <= 0 {
		return []string{text}
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	current := words[0]
	for _, w := range words[1:] {
		if len(current)+1+len(w) > maxWidth {
			lines = append(lines, current)
			current = w
		} else {
			current += " " + w
		}
	}
	lines = append(lines, current)
	return lines
}
