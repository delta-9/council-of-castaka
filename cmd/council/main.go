// Command council runs a council of minds from several labs on a technical
// matter. Every message, the chair's or a mind's, is queued into the next turn
// of every other mind; internal/floor explains how.
//
// Usage:
//
//	council [-roster minds|rehearsal] [-resume <session>] [-fake]
//	council -q -m "matter" [-n rounds]
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/delta-9/council-of-castaka/internal/floor"
	"github.com/delta-9/council-of-castaka/internal/minds"
)

type price struct {
	In         float64 `json:"in"`
	Out        float64 `json:"out"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

type config struct {
	EnvFile   string                  `json:"envFile"`
	Keys      map[string]string       `json:"keys"` // provider → variable name
	Sessions  string                  `json:"sessions"`
	Rounds    int                     `json:"rounds"`
	Words     int                     `json:"words"`
	MaxTokens int64                   `json:"maxTokens"`
	Rosters   map[string][]minds.Spec `json:"rosters"`
	Prices    map[string]price        `json:"prices"` // $ per 1M tokens
}

const synth = "Synthesize the debate so far: the consensus, the open disagreements and who holds each, and what to test first."

const help = `Type a line to post it; every mind reads it at its next turn.
"""            start or end a multi-line message
@seat …        only the named seats speak in the next round
/more          one more round
/stop          cancel the round in flight
/wait          hold the next line until the floor is quiet
/rounds N      rounds granted to each message
/seats         list the seats; /seat -id or /seat +id takes one off or back
/attach PATH   post a file
/view SEAT     what that seat receives next
/synth [SEAT]  ask one seat for a synthesis
/cost          tokens and estimated cost per seat
/quit          stop and leave`

func main() {
	cfgPath := flag.String("config", "council.json", "config file, looked up next to the binary too")
	rosterName := flag.String("roster", "minds", "roster from the config")
	resume := flag.String("resume", "", "session directory to reopen")
	quiet := flag.Bool("q", false, "no console: post -m, run the rounds, print the transcript path")
	matter := flag.String("m", "", "the matter, with -q")
	rounds := flag.Int("n", 0, "rounds granted to each chair message (default from the config)")
	fakeAll := flag.Bool("fake", false, "every seat uses the fake mind, for free")
	flag.Parse()
	if err := run(*cfgPath, *rosterName, *resume, *quiet, *matter, *rounds, *fakeAll); err != nil {
		fmt.Fprintln(os.Stderr, "council:", err)
		os.Exit(1)
	}
}

func run(cfgPath, rosterName, resume string, quiet bool, matter string, rounds int, fakeAll bool) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	roster, ok := cfg.Rosters[rosterName]
	if !ok {
		return fmt.Errorf("no roster %q in the config", rosterName)
	}
	if rounds <= 0 {
		rounds = cfg.Rounds
	}
	if quiet && matter == "" {
		return errors.New(`-q needs -m "matter"`)
	}
	dir := resume
	if dir == "" {
		dir = filepath.Join(expand(cfg.Sessions), time.Now().Format("2006-01-02-150405"))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	log, err := floor.OpenLog(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		return err
	}
	defer log.Close()
	systems, err := loadSystems(dir, roster, cfg.Words)
	if err != nil {
		return err
	}
	keys := loadKeys(expand(cfg.EnvFile), cfg.Keys)
	var seats []*floor.Seat
	for _, spec := range roster {
		if fakeAll {
			spec.Provider = "fake"
		}
		key := keys[spec.Provider]
		if spec.Provider != "fake" && key == "" {
			return fmt.Errorf("seat %s: no %s in %s or the environment", spec.Seat, cfg.Keys[spec.Provider], cfg.EnvFile)
		}
		mind, err := minds.New(spec, key, minds.Options{MaxTokens: cfg.MaxTokens, Session: filepath.Base(dir)})
		if err != nil {
			return err
		}
		seats = append(seats, floor.NewSeat(spec.Seat, spec.Model, systems[spec.Seat], mind))
	}
	f := floor.New(log, seats, rounds)
	c := &console{f: f, cfg: cfg, dir: dir, quiet: quiet, tty: term.IsTerminal(int(os.Stdout.Fd()))}
	f.OnRound, f.OnEntry = c.onRound, c.onEntry
	if quiet {
		if err := f.Post(matter); err != nil {
			return err
		}
		f.Wait()
		fmt.Println(c.transcript())
		return nil
	}
	return c.run()
}

func loadConfig(path string) (config, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) && !filepath.IsAbs(path) {
		if exe, e := os.Executable(); e == nil {
			b, err = os.ReadFile(filepath.Join(filepath.Dir(exe), path))
		}
	}
	if err != nil {
		return config{}, err
	}
	cfg := config{Sessions: "~/.council/sessions", Rounds: 2, Words: 300, MaxTokens: 16000}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// loadSystems returns each seat's system prompt. A session writes them to
// systems.json when it starts and reuses them verbatim when it resumes.
func loadSystems(dir string, roster []minds.Spec, words int) (map[string]string, error) {
	path := filepath.Join(dir, "systems.json")
	systems := map[string]string{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &systems); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	added := false
	for _, spec := range roster {
		if _, ok := systems[spec.Seat]; !ok {
			systems[spec.Seat] = floor.Charter(spec, roster, words)
			added = true
		}
	}
	if added {
		b, err := json.MarshalIndent(systems, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return nil, err
		}
	}
	return systems, nil
}

// loadKeys reads the API keys by variable name from the env file; a variable
// set in the environment wins. Values are never printed.
func loadKeys(path string, names map[string]string) map[string]string {
	provider := map[string]string{}
	for p, v := range names {
		provider[v] = p
	}
	keys := map[string]string{}
	if b, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			name, value, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
			if p, want := provider[strings.TrimSpace(name)]; ok && want {
				keys[p] = strings.Trim(strings.TrimSpace(value), `"'`)
			}
		}
	}
	for v, p := range provider {
		if s := os.Getenv(v); s != "" {
			keys[p] = s
		}
	}
	return keys
}

func expand(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}

// console is the chair's seat: it reads lines, prints the floor, and keeps
// transcript.md current.
type console struct {
	f     *floor.Floor
	cfg   config
	dir   string
	quiet bool
	tty   bool
	mu    sync.Mutex // serializes output and transcript writes
}

func (c *console) transcript() string { return filepath.Join(c.dir, "transcript.md") }

func (c *console) color(code, s string) string {
	if !c.tty {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func (c *console) say(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Println(s)
}

func (c *console) onRound(round int, seats []string) {
	if c.quiet {
		return
	}
	c.say(c.color("2", fmt.Sprintf("── round %d · %s ──", round, strings.Join(seats, " "))))
}

func (c *console) onEntry(e floor.Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	md := floor.Transcript("Council · "+filepath.Base(c.dir), c.f.Seats, c.f.Log.Snapshot())
	if err := os.WriteFile(c.transcript(), []byte(md), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "transcript:", err)
	}
	if c.quiet {
		return
	}
	switch {
	case e.From == "chair":
		fmt.Println(c.color("2", fmt.Sprintf("#%d chair: %s", e.Seq, firstLine(e.Text))))
	case e.Kind == "msg":
		fmt.Printf("\n%s\n%s\n\n", c.color("38;2;240;200;100", fmt.Sprintf("── %s · #%d · %s ──", e.From, e.Seq, floor.Meta(e))), e.Text)
	case e.Kind == "pass":
		fmt.Println(c.color("2", fmt.Sprintf("#%d %s passes · %s", e.Seq, e.From, floor.Meta(e))))
	default:
		fmt.Println(c.color("31", fmt.Sprintf("#%d %s: error: %s", e.Seq, e.From, e.Text)))
	}
}

func (c *console) run() error {
	members := make([]string, len(c.f.Seats))
	for i, s := range c.f.Seats {
		members[i] = s.ID + " (" + s.Model + ")"
	}
	c.say(fmt.Sprintf("session %s\nseats: %s\n/help for commands", c.dir, strings.Join(members, ", ")))

	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	go func() {
		for range interrupts {
			if !c.f.Busy() {
				os.Exit(130)
			}
			c.f.Stop()
			c.say("stopped: the round in flight leaves no trace")
		}
	}()

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1<<20), 16<<20)
	var block []string
	inBlock := false
	for in.Scan() {
		line := in.Text()
		if strings.TrimSpace(line) == `"""` {
			if inBlock {
				c.post(strings.Join(block, "\n"))
				block = nil
			}
			inBlock = !inBlock
			continue
		}
		if inBlock {
			block = append(block, line)
			continue
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "/"):
			if !c.command(line) {
				return nil
			}
		default:
			c.post(line)
		}
	}
	c.f.Wait()
	return in.Err()
}

func (c *console) post(text string) {
	if err := c.f.Post(text); err != nil {
		c.say("error: " + err.Error())
	}
}

// command runs a slash command; false means leave.
func (c *console) command(line string) bool {
	cmd, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	switch cmd {
	case "/quit":
		c.f.Stop()
		return false
	case "/more":
		c.f.More()
	case "/stop":
		c.f.Stop()
	case "/wait":
		c.f.Wait()
	case "/rounds":
		n, err := strconv.Atoi(arg)
		if err != nil || n < 1 {
			c.say("/rounds needs a number from 1")
			break
		}
		c.f.SetRounds(n)
	case "/seats":
		for _, s := range c.f.Seats {
			state := "on"
			if !c.f.Active(s) {
				state = "off"
			}
			c.say(fmt.Sprintf("%-10s %-20s %s", s.ID, s.Model, state))
		}
	case "/seat":
		if len(arg) < 2 || (arg[0] != '+' && arg[0] != '-') || !c.f.SetActive(arg[1:], arg[0] == '+') {
			c.say("usage: /seat -id or /seat +id")
		}
	case "/attach":
		c.attach(arg)
	case "/view":
		c.view(arg)
	case "/synth":
		seat := arg
		if seat == "" {
			seat = c.f.Seats[0].ID
		}
		c.post("@" + seat + " " + synth)
	case "/cost":
		c.say(c.cost())
	case "/help":
		c.say(help)
	default:
		c.say("unknown command; /help lists them")
	}
	return true
}

func (c *console) attach(path string) {
	b, err := os.ReadFile(expand(path))
	if err != nil {
		c.say("attach: " + err.Error())
		return
	}
	if len(b) > 256<<10 {
		c.say("attach: over 256 KB; every mind would reread it on every turn")
		return
	}
	c.post(fmt.Sprintf("Attached `%s`:\n\n```\n%s\n```", filepath.Base(path), strings.TrimRight(string(b), "\n")))
}

// view prints exactly what a seat receives next: the queue made visible.
func (c *console) view(id string) {
	if c.f.Seat(id) == nil {
		c.say("no seat " + id)
		return
	}
	entries := c.f.Log.Snapshot()
	turns, queue := floor.View(entries, id, len(entries))
	if len(queue) == 0 {
		c.say(fmt.Sprintf("%s: nothing queued, after %d turns of history", id, len(turns)))
		return
	}
	from := make([]string, len(queue))
	for i, seq := range queue {
		from[i] = fmt.Sprintf("#%d %s", seq, entries[seq-1].From)
	}
	c.say(fmt.Sprintf("%s receives next, after %d turns of history: %s\n\n%s",
		id, len(turns)-1, strings.Join(from, ", "), turns[len(turns)-1].Text))
}

// cost sums each seat's usage and prices it with council.json.
func (c *console) cost() string {
	type sum struct {
		turns                int
		in, read, write, out int64
		usd                  float64
		unpriced             bool
	}
	per := map[string]*sum{}
	var order []string
	for _, e := range c.f.Log.Snapshot() {
		if e.Usage == nil {
			continue
		}
		s := per[e.From]
		if s == nil {
			s = &sum{}
			per[e.From] = s
			order = append(order, e.From)
		}
		u := *e.Usage
		s.turns++
		s.in, s.read, s.write, s.out = s.in+u.Input, s.read+u.CacheRead, s.write+u.CacheWrite, s.out+u.Output
		p, ok := c.cfg.Prices[e.Served]
		if !ok {
			p, ok = c.cfg.Prices[e.Model]
		}
		if !ok {
			s.unpriced = true
			continue
		}
		write := p.CacheWrite
		if write == 0 {
			write = p.In
		}
		s.usd += (float64(u.Input)*p.In + float64(u.CacheRead)*p.CacheRead + float64(u.CacheWrite)*write + float64(u.Output)*p.Out) / 1e6
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-10s %5s %9s %9s %9s %9s %9s\n", "seat", "turns", "input", "cached", "written", "output", "est. $")
	total := 0.0
	for _, id := range order {
		s := per[id]
		usd := fmt.Sprintf("%.4f", s.usd)
		if s.unpriced {
			usd += "+"
		}
		fmt.Fprintf(&b, "%-10s %5d %9d %9d %9d %9d %9s\n", id, s.turns, s.in, s.read, s.write, s.out, usd)
		total += s.usd
	}
	fmt.Fprintf(&b, "total est. $%.4f (prices from council.json; + marks turns with no price)", total)
	return b.String()
}

func firstLine(s string) string {
	line, _, more := strings.Cut(s, "\n")
	if r := []rune(line); len(r) > 100 {
		line, more = string(r[:100]), true
	}
	if more {
		line += " …"
	}
	return line
}
