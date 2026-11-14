// Command council runs a council of minds from several labs on a technical
// matter. Every message, the chair's or a mind's, is queued into the next turn
// of every other mind; internal/floor explains how. Started inside a GitHub
// repository, the minds can read what git tracks there, and they can search
// the web.
//
// Usage:
//
//	council [-roster minds|rehearsal] [-resume <session>] [-fake]
//	council -q -m "matter" [-n rounds] [-fold seat]
//	council -search "query"
package main

import (
	"bufio"
	"context"
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
	"github.com/delta-9/council-of-castaka/internal/tools"
)

type price struct {
	In         float64 `json:"in"`
	Out        float64 `json:"out"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Search     float64 `json:"search"` // $ per web search the provider runs
}

// searchServer is the MCP server that seats with "search": "mcp" search through.
type searchServer struct {
	Command []string          `json:"command"`
	Env     map[string]string `json:"env"`
	Key     string            `json:"key"` // the entry of keys whose variable the server reads
	Tool    string            `json:"tool"`
}

type config struct {
	EnvFile      string                  `json:"envFile"`
	Keys         map[string]string       `json:"keys"` // provider → variable name
	Sessions     string                  `json:"sessions"`
	Rounds       int                     `json:"rounds"`
	Words        int                     `json:"words"`
	MaxTokens    int64                   `json:"maxTokens"` // a seat's maxTokens overrides it
	MaxCalls     int                     `json:"maxCalls"`  // local tool calls per turn; 0 means the default
	SearchServer searchServer            `json:"searchServer"`
	Rosters      map[string][]minds.Spec `json:"rosters"`
	Prices       map[string]price        `json:"prices"` // $ per 1M tokens
}

// session is fixed when a session starts: its roster, its repository and its
// system prompts. Changing any of them under replayed turns would break those
// turns, starting with Fable's history check, which compares the tools too.
type session struct {
	Created string            `json:"created"`
	Root    string            `json:"root,omitempty"`
	Repo    string            `json:"repo,omitempty"`
	Roster  []minds.Spec      `json:"roster"`
	Systems map[string]string `json:"systems"`
}

const foldPrompt = "Fold the council's debate into the final report for the chair, as a standalone Markdown document. " +
	"Lead with the conclusion. Then give the facts, evidence and arguments that led to it: who brought them, " +
	"and the files, commits and sources they came from. Say what is still disputed, and who holds which position. " +
	"When relevant, end with the next steps. Check with your tools anything you are unsure of before you write. " +
	"The word limit does not apply."

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
/fold [SEAT]   that seat folds the debate into report.md
/cost          tokens, searches and estimated cost per seat
/quit          stop and leave`

type flags struct {
	config, roster, resume, matter, fold, search string
	quiet, fake                                  bool
	rounds                                       int
}

func main() {
	var fl flags
	flag.StringVar(&fl.config, "config", "council.json", "config file, looked up next to the binary too")
	flag.StringVar(&fl.roster, "roster", "minds", "roster from the config, for a new session")
	flag.StringVar(&fl.resume, "resume", "", "session directory to reopen")
	flag.BoolVar(&fl.quiet, "q", false, "no console: post -m, run the rounds, print the transcript path")
	flag.StringVar(&fl.matter, "m", "", "the matter, with -q")
	flag.StringVar(&fl.fold, "fold", "", "with -q: the seat that folds the debate; prints the report path")
	flag.IntVar(&fl.rounds, "n", 0, "rounds granted to each chair message (default from the config)")
	flag.BoolVar(&fl.fake, "fake", false, "every seat uses the fake mind, for free")
	flag.StringVar(&fl.search, "search", "", "run one search through the MCP server and print the results")
	flag.Parse()
	if err := run(fl); err != nil {
		fmt.Fprintln(os.Stderr, "council:", err)
		os.Exit(1)
	}
}

func run(fl flags) error {
	cfg, err := loadConfig(fl.config)
	if err != nil {
		return err
	}
	keys := loadKeys(expand(cfg.EnvFile), cfg.Keys)
	searcher := cfg.searcher(keys)
	if searcher != nil {
		defer searcher.Close()
	}
	if fl.search != "" {
		if searcher == nil {
			return errors.New("no searchServer in the config")
		}
		start := time.Now()
		out, err := searcher.Search(context.Background(), fl.search)
		if err != nil {
			return err
		}
		fmt.Printf("%s, %d bytes (about %d tokens)\n\n%s\n", time.Since(start).Round(time.Millisecond), len(out), len(out)/4, out)
		return nil
	}
	rounds := fl.rounds
	if rounds <= 0 {
		rounds = cfg.Rounds
	}
	if fl.quiet && fl.matter == "" {
		return errors.New(`-q needs -m "matter"`)
	}
	dir := fl.resume
	if dir == "" {
		dir = filepath.Join(expand(cfg.Sessions), time.Now().Format("2006-01-02-150405"))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sess, err := openSession(dir, cfg, fl.roster)
	if err != nil {
		return err
	}
	log, err := floor.OpenLog(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		return err
	}
	defer log.Close()

	var repo *tools.Repo
	if sess.Root != "" {
		repo = &tools.Repo{Root: sess.Root, Name: sess.Repo}
	}
	c := &console{cfg: cfg, sess: sess, dir: dir, quiet: fl.quiet, tty: term.IsTerminal(int(os.Stdout.Fd()))}
	var seats []*floor.Seat
	for _, spec := range sess.Roster {
		kit := tools.Kit{Repo: repo}
		if spec.Search == "mcp" {
			if searcher == nil {
				return fmt.Errorf("seat %s searches through MCP, but the config has no searchServer", spec.Seat)
			}
			kit.Search = searcher
		}
		if fl.fake {
			spec.Provider = "fake"
		}
		key := keys[spec.Provider]
		if spec.Provider != "fake" && key == "" {
			return fmt.Errorf("seat %s: no %s in %s or the environment", spec.Seat, cfg.Keys[spec.Provider], cfg.EnvFile)
		}
		maxTokens := cfg.MaxTokens
		if spec.MaxTokens > 0 {
			maxTokens = spec.MaxTokens
		}
		mind, err := minds.New(spec, key, minds.Options{
			MaxTokens: maxTokens, MaxCalls: cfg.MaxCalls, Session: filepath.Base(dir), Tools: kit, Notify: c.notifier(spec.Seat),
		})
		if err != nil {
			return err
		}
		seats = append(seats, floor.NewSeat(spec.Seat, spec.Model, sess.Systems[spec.Seat], mind))
	}
	c.f = floor.New(log, seats, rounds)
	c.f.OnRound, c.f.OnEntry = c.onRound, c.onEntry
	if !fl.quiet {
		return c.run()
	}
	if _, err := c.f.Post(fl.matter); err != nil {
		return err
	}
	c.f.Wait()
	if fl.fold == "" {
		fmt.Println(c.transcript())
		return nil
	}
	if err := c.fold(fl.fold); err != nil {
		return err
	}
	c.f.Wait()
	report := c.report()
	if report == "" {
		return fmt.Errorf("%s wrote no report; see %s", fl.fold, c.transcript())
	}
	fmt.Println(report)
	return nil
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

// openSession reopens the session in dir with its own roster, or starts one
// there from the named roster, reading the repository the council was
// started in.
func openSession(dir string, cfg config, name string) (session, error) {
	path := filepath.Join(dir, "session.json")
	var s session
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &s); err != nil {
			return s, fmt.Errorf("%s: %w", path, err)
		}
		return s, nil
	} else if !os.IsNotExist(err) {
		return s, err
	}
	if _, err := os.Stat(filepath.Join(dir, "systems.json")); err == nil {
		return s, fmt.Errorf("%s was started before the minds had tools; start a new session", dir)
	}
	roster, ok := cfg.Rosters[name]
	if !ok {
		return s, fmt.Errorf("no roster %q in the config", name)
	}
	s = session{Created: time.Now().Format("2006-01-02"), Roster: roster, Systems: map[string]string{}}
	if cwd, err := os.Getwd(); err == nil {
		if repo := tools.Find(cwd); repo != nil {
			s.Root, s.Repo = repo.Root, repo.Name
		}
	}
	for _, spec := range roster {
		s.Systems[spec.Seat] = floor.Charter(spec, roster, cfg.Words, s.Created, s.Repo)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return s, err
	}
	return s, os.WriteFile(path, b, 0o600)
}

// searcher returns the MCP web search the config names, or nil.
func (cfg config) searcher(keys map[string]string) *tools.MCP {
	s := cfg.SearchServer
	if len(s.Command) == 0 {
		return nil
	}
	var env []string
	for k, v := range s.Env {
		env = append(env, k+"="+v)
	}
	if name, key := cfg.Keys[s.Key], keys[s.Key]; name != "" && key != "" {
		env = append(env, name+"="+key)
	}
	return &tools.MCP{Command: s.Command, Env: env, Tool: s.Tool}
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
// transcript.md and report.md current.
type console struct {
	f     *floor.Floor
	cfg   config
	sess  session
	dir   string
	quiet bool
	tty   bool

	mu       sync.Mutex // serializes output, transcript writes and the fold
	folding  string     // the seat writing the report, if any
	foldFrom int        // the seq of the fold request
	reported string     // the path of the last report
}

func (c *console) transcript() string { return filepath.Join(c.dir, "transcript.md") }

func (c *console) report() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reported
}

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

// notifier prints a seat's tool calls as they happen.
func (c *console) notifier(seat string) func(string) {
	return func(call string) {
		if !c.quiet {
			c.say(c.color("2", "  "+seat+" · "+call))
		}
	}
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
	folded, failed := "", false
	if c.folding != "" && e.From == c.folding && e.Seen >= c.foldFrom {
		c.folding = ""
		failed = e.Kind != "msg"
		if !failed {
			path := filepath.Join(c.dir, "report.md")
			if err := os.WriteFile(path, []byte(e.Text+"\n"), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, "report:", err)
			} else {
				c.reported, folded = path, path
			}
		}
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
	if folded != "" {
		fmt.Println(c.color("1", "report: "+folded))
	} else if failed {
		fmt.Println(c.color("31", "the fold failed; /fold again or pick another seat"))
	}
}

// fold asks a seat to fold the debate into report.md.
func (c *console) fold(seat string) error {
	if c.f.Seat(seat) == nil {
		return fmt.Errorf("no seat %s", seat)
	}
	e, err := c.f.Post("@" + seat + " " + foldPrompt)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.folding, c.foldFrom = seat, e.Seq
	c.mu.Unlock()
	return nil
}

func (c *console) run() error {
	members := make([]string, len(c.f.Seats))
	for i, s := range c.f.Seats {
		members[i] = s.ID + " (" + s.Model + ")"
	}
	reading := "no GitHub repository here: the minds read no files"
	if c.sess.Repo != "" {
		reading = "reading " + c.sess.Repo + ": what git tracks at " + c.sess.Root
	}
	c.say(fmt.Sprintf("session %s\nseats: %s\n%s\n/help for commands", c.dir, strings.Join(members, ", "), reading))

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
	if _, err := c.f.Post(text); err != nil {
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
	case "/fold":
		seat := arg
		if seat == "" {
			seat = c.f.Seats[0].ID
		}
		if err := c.fold(seat); err != nil {
			c.say("fold: " + err.Error())
		}
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
		turns                          int
		in, read, write, out, searches int64
		usd                            float64
		unpriced                       bool
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
		s.searches += u.Searches
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
		s.usd += (float64(u.Input)*p.In+float64(u.CacheRead)*p.CacheRead+float64(u.CacheWrite)*write+float64(u.Output)*p.Out)/1e6 +
			float64(u.Searches)*p.Search
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-10s %5s %9s %9s %9s %9s %8s %9s\n", "seat", "turns", "input", "cached", "written", "output", "searches", "est. $")
	total := 0.0
	for _, id := range order {
		s := per[id]
		usd := fmt.Sprintf("%.4f", s.usd)
		if s.unpriced {
			usd += "+"
		}
		fmt.Fprintf(&b, "%-10s %5d %9d %9d %9d %9d %8d %9s\n", id, s.turns, s.in, s.read, s.write, s.out, s.searches, usd)
		total += s.usd
	}
	fmt.Fprintf(&b, "total est. $%.4f (prices from council.json; + marks turns with no price; MCP searches are in your MiniMax plan)", total)
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
