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
//	council [-q] --pr number [--dry-run] [-m focus] [-n rounds] [-fold seat]
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
	"github.com/delta-9/council-of-castaka/internal/pr"
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
	PR      *pr.Info          `json:"pr,omitempty"`     // the pull request a review session reviews
	DryRun  bool              `json:"dryRun,omitempty"` // a review that writes its posts to github/ instead
}

const foldPrompt = "Fold the council's debate into the final report for the chair, as a standalone Markdown document. " +
	"Lead with the conclusion. Then give the facts, evidence and arguments that led to it: who brought them, " +
	"and the files, commits and sources they came from. Say what is still disputed, and who holds which position. " +
	"When relevant, end with the next steps. Check with your tools anything you are unsure of before you write. " +
	"The word limit does not apply."

const help = `Type a line to post it; every mind reads it at its next turn.
"""            start or end a multi-line message
@seat …        only the named seats speak in the next round
               (a seat is its name or its id, case and accents aside: @hurl is Hûrl)
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
	quiet, fake, dry                             bool
	rounds, pr                                   int
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
	flag.IntVar(&fl.pr, "pr", 0, "review this pull request of the repository the council runs in, posting each message and the review on it")
	flag.BoolVar(&fl.dry, "dry-run", false, "with -pr: write the comments and the review into the session folder instead of posting them")
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
	dir := fl.resume
	if dir == "" {
		dir = filepath.Join(expand(cfg.Sessions), time.Now().Format("2006-01-02-150405"))
	}
	_, err = os.Stat(filepath.Join(dir, "session.json"))
	fresh := os.IsNotExist(err)
	var info *pr.Info
	var diff string
	if fl.pr > 0 && fresh {
		i, d, err := prepareReview(fl.pr)
		if err != nil {
			return err
		}
		info, diff = &i, d
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sess, err := openSession(dir, cfg, fl.roster, info, fl.dry)
	if err != nil {
		return err
	}
	if info != nil {
		if err := os.WriteFile(filepath.Join(dir, "pr.diff"), []byte(diff), 0o600); err != nil {
			return err
		}
	}
	switch {
	case fl.pr > 0 && (sess.PR == nil || sess.PR.Number != fl.pr):
		return fmt.Errorf("%s is not a review of pull request %d", dir, fl.pr)
	case fl.dry && (sess.PR == nil || !sess.DryRun):
		return errors.New("--dry-run goes with --pr when a review session starts; a session keeps the mode it started with")
	case fl.quiet && fl.matter == "" && sess.PR == nil:
		return errors.New(`-q needs -m "matter"`)
	}
	rounds := fl.rounds
	if rounds <= 0 {
		rounds = cfg.Rounds
		if sess.PR != nil {
			rounds = reviewRounds
		}
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
		kit := tools.Kit{Repo: repo, Rev: sess.PR != nil}
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
		label := spec.Seat
		if spec.Name != "" {
			label = spec.Name
		}
		mind, err := minds.New(spec, key, minds.Options{
			MaxTokens: maxTokens, MaxCalls: cfg.MaxCalls, Session: filepath.Base(dir), Tools: kit, Notify: c.notifier(label),
		})
		if err != nil {
			return err
		}
		seat := floor.NewSeat(spec.Seat, spec.Model, sess.Systems[spec.Seat], mind)
		seat.Name = spec.Name
		seats = append(seats, seat)
	}
	c.f = floor.New(log, seats, rounds)
	c.f.OnRound, c.f.OnEntry = c.onRound, c.onEntry
	if sess.PR != nil {
		focus := ""
		if fresh {
			focus = fl.matter
		}
		if err := c.startReview(fl.fold, focus, fresh); err != nil {
			return err
		}
	}
	if !fl.quiet {
		return c.run()
	}
	if c.rv != nil {
		switch {
		case fresh:
			c.openReview()
		case fl.matter != "":
			c.post(fl.matter)
		default:
			c.onQuiet()
		}
		<-c.rv.done
		if c.rv.err != nil {
			return c.rv.err
		}
		fmt.Println(c.rv.url)
		return nil
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
// started in; with info, the session reviews that pull request.
func openSession(dir string, cfg config, name string, info *pr.Info, dry bool) (session, error) {
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
	s = session{Created: time.Now().Format("2006-01-02"), Roster: roster, Systems: map[string]string{}, PR: info, DryRun: dry && info != nil}
	if cwd, err := os.Getwd(); err == nil {
		if repo := tools.Find(cwd); repo != nil {
			s.Root, s.Repo = repo.Root, repo.Name
		}
	}
	review := ""
	if info != nil {
		review = reviewCharter(*info)
	}
	for _, spec := range roster {
		s.Systems[spec.Seat] = floor.Charter(spec, roster, cfg.Words, s.Created, s.Repo, review)
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
	rv       *review    // the pull request a review session posts on, or nil
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

// label is how the console names a seat: its name, or its ID.
func (c *console) label(id string) string {
	if s := c.f.Seat(id); s != nil {
		return s.Label()
	}
	return id
}

func (c *console) onRound(round int, seats []string) {
	if c.quiet {
		return
	}
	labels := make([]string, len(seats))
	for i, id := range seats {
		labels[i] = c.label(id)
	}
	c.say(c.color("2", fmt.Sprintf("── round %d · %s ──", round, strings.Join(labels, " "))))
}

func (c *console) onEntry(e floor.Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	md := floor.Transcript("Council · "+filepath.Base(c.dir), c.f.Seats, c.f.Log.Snapshot())
	if err := os.WriteFile(c.transcript(), []byte(md), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "transcript:", err)
	}
	folded, failed := "", false
	fold := c.folding != "" && e.From == c.folding && e.Seen >= c.foldFrom
	if fold {
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
	if c.rv != nil {
		c.reviewEntry(e, fold, failed)
	}
	if c.quiet {
		return
	}
	from := c.label(e.From)
	switch {
	case e.From == "chair":
		fmt.Println(c.color("2", fmt.Sprintf("#%d chair: %s", e.Seq, firstLine(e.Text))))
	case e.Kind == "msg":
		fmt.Printf("\n%s\n%s\n\n", c.color("38;2;240;200;100", fmt.Sprintf("── %s · #%d · %s ──", from, e.Seq, floor.Meta(e))), e.Text)
	case e.Kind == "pass":
		fmt.Println(c.color("2", fmt.Sprintf("#%d %s passes · %s", e.Seq, from, floor.Meta(e))))
	default:
		fmt.Println(c.color("31", fmt.Sprintf("#%d %s: error: %s", e.Seq, from, e.Text)))
	}
	if folded != "" {
		fmt.Println(c.color("1", "report: "+folded))
	} else if failed {
		fmt.Println(c.color("31", "the fold failed; /fold again or pick another seat"))
	}
}

// fold asks a seat, by its ID or name, to fold the debate into report.md; in
// a review session, into the review.
func (c *console) fold(seat string) error {
	s := c.f.Seat(seat)
	if s == nil {
		return fmt.Errorf("no seat %s", seat)
	}
	prompt := foldPrompt
	c.mu.Lock()
	if c.rv != nil {
		if c.rv.stage == posting {
			c.mu.Unlock()
			return errors.New("the review is being posted")
		}
		prompt, c.rv.stage, c.rv.held = reviewFoldPrompt, folding, false
	}
	c.mu.Unlock()
	e, err := c.f.Post("@" + s.Label() + " " + prompt)
	if err != nil {
		c.mu.Lock()
		if c.rv != nil {
			c.rv.stage = debating
		}
		c.mu.Unlock()
		return err
	}
	c.mu.Lock()
	c.folding, c.foldFrom = s.ID, e.Seq
	c.mu.Unlock()
	return nil
}

func (c *console) run() error {
	members := make([]string, len(c.f.Seats))
	for i, s := range c.f.Seats {
		members[i] = s.Label() + " (" + s.Model + ")"
	}
	reading := "no GitHub repository here: the minds read no files"
	if c.sess.Repo != "" {
		reading = "reading " + c.sess.Repo + ": what git tracks at " + c.sess.Root
	}
	c.say(fmt.Sprintf("session %s\nseats: %s\n%s\n/help for commands", c.dir, strings.Join(members, ", "), reading))
	var done <-chan struct{}
	if c.rv != nil {
		where := "posting on " + c.rv.info.URL
		if c.sess.DryRun {
			where = "a dry run: the posts go to " + filepath.Join(c.dir, "github")
		}
		c.say(fmt.Sprintf("reviewing pull request #%d at %s, %s; %s folds once the floor goes quiet", c.rv.info.Number, short(c.rv.info.HeadOid), where, c.label(c.rv.foldBy)))
		done = c.rv.done
	}

	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	go func() {
		for range interrupts {
			if !c.f.Busy() {
				os.Exit(130)
			}
			c.hold()
			c.f.Stop()
			c.say("stopped: the round in flight leaves no trace")
		}
	}()
	if c.rv != nil {
		c.openReview()
	}

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1<<20), 16<<20)
	lines := make(chan string)
	go func() {
		defer close(lines)
		for in.Scan() {
			lines <- in.Text()
		}
	}()
	var block []string
	inBlock := false
	for {
		var line string
		select {
		case l, ok := <-lines:
			if !ok {
				return c.end(in)
			}
			line = l
		case <-done:
			c.say(c.cost())
			return nil
		}
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
}

// end finishes once the chair's input ends: a review session waits for its
// review, unless the chair holds the fold.
func (c *console) end(in *bufio.Scanner) error {
	c.mu.Lock()
	wait := c.rv != nil && !c.rv.held
	c.mu.Unlock()
	if wait {
		<-c.rv.done
		c.say(c.cost())
		return c.rv.err
	}
	c.f.Wait()
	return in.Err()
}

func (c *console) post(text string) {
	c.release()
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
		c.hold()
		c.f.Stop()
		return false
	case "/more":
		c.release()
		c.f.More()
	case "/stop":
		c.hold()
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
			c.say(fmt.Sprintf("%-10s %-10s %-20s %s", s.Label(), s.ID, s.Model, state))
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
		if seat == "" && c.rv != nil {
			seat = c.rv.foldBy
		} else if seat == "" {
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
	s := c.f.Seat(id)
	if s == nil {
		c.say("no seat " + id)
		return
	}
	entries := c.f.Log.Snapshot()
	turns, queue := floor.View(entries, s.ID, len(entries), c.f.Names())
	if len(queue) == 0 {
		c.say(fmt.Sprintf("%s: nothing queued, after %d turns of history", s.Label(), len(turns)))
		return
	}
	from := make([]string, len(queue))
	for i, seq := range queue {
		from[i] = fmt.Sprintf("#%d %s", seq, c.label(entries[seq-1].From))
	}
	c.say(fmt.Sprintf("%s receives next, after %d turns of history: %s\n\n%s",
		s.Label(), len(turns)-1, strings.Join(from, ", "), turns[len(turns)-1].Text))
}

// cost sums each seat's usage and prices it with council.json.
func (c *console) cost() string {
	type sum struct {
		turns                          int
		in, read, write, out, searches int64
		usd                            float64
		unpriced, retried              bool
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
		s.retried = s.retried || len(e.Retries) > 0
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
		if s.retried {
			usd += "*"
		}
		fmt.Fprintf(&b, "%-10s %5d %9d %9d %9d %9d %8d %9s\n", c.label(id), s.turns, s.in, s.read, s.write, s.out, s.searches, usd)
		total += s.usd
	}
	fmt.Fprintf(&b, "total est. $%.4f (prices from council.json; + marks turns with no price; * marks a retried turn, whose broken try may have cost more than it reported; MCP searches are in your MiniMax plan)", total)
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
