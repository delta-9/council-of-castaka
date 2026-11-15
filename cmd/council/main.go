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
//	council --events | --detached [the console's flags]
//	council keys [set <provider> | import <env file> | check]
//	council options | roster [set]
//	council -search "query"
//
// The config is the built-in one (defaults.json), then ~/.council/council.json,
// then the file -config names. The keys come from the environment, then
// ~/.council/keys.env, then the config's envFile.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/delta-9/council-of-castaka/internal/floor"
	"github.com/delta-9/council-of-castaka/internal/minds"
	"github.com/delta-9/council-of-castaka/internal/pr"
	"github.com/delta-9/council-of-castaka/internal/tools"
)

type price struct {
	Provider   string  `json:"provider"` // the lab whose seats may take the model
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
	EnvFile      string                  `json:"envFile"` // an env file to read keys from, after ~/.council/keys.env
	Keys         map[string]string       `json:"keys"`    // provider → variable name
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
	quiet, fake, dry, events, detached           bool
	rounds, pr                                   int
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "keys" || os.Args[1] == "options" || os.Args[1] == "roster") {
		if err := subcommand(os.Args[1], os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "council:", err)
			os.Exit(1)
		}
		return
	}
	var fl flags
	flag.StringVar(&fl.config, "config", "", "a config file over the built-in one and ~/.council/council.json")
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
	flag.BoolVar(&fl.events, "events", false, "write the console's output as JSON events, one per line, for a program to show")
	flag.BoolVar(&fl.detached, "detached", false, "with events: read the chair from the session's chair pipe and write the events to its events.jsonl")
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
	keys, _ := loadKeys(cfg)
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
	case fl.quiet && (fl.events || fl.detached):
		return errors.New("-q and the events modes do not go together: the events are the console's")
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
	c := &console{cfg: cfg, sess: sess, dir: dir, quiet: fl.quiet, tty: term.IsTerminal(int(os.Stdout.Fd())), quit: make(chan struct{})}
	if fl.detached {
		in, out, cleanup, err := detach(dir)
		if err != nil {
			return err
		}
		defer cleanup()
		c.in, c.out = in, out
	} else if fl.events {
		c.out = os.Stdout
	}
	if c.out != nil {
		defer c.emit(event{Type: "end"})
	}
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
			return fmt.Errorf("seat %s: no %s key (%s); set it with: council keys set %s", spec.Seat, minds.Lab(spec.Provider), cfg.Keys[spec.Provider], spec.Provider)
		}
		maxTokens := budget(spec, cfg.MaxTokens)
		label := spec.Seat
		if spec.Name != "" {
			label = spec.Name
		}
		mind, err := minds.New(spec, key, minds.Options{
			MaxTokens: maxTokens, MaxCalls: cfg.MaxCalls, Session: filepath.Base(dir), Tools: kit, Notify: c.notifier(spec.Seat, label),
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
	c.emit(event{Type: "session", Dir: dir, Repo: sess.Repo, Seats: sess.Roster, PR: sess.PR, DryRun: sess.DryRun})
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
	all, ok := cfg.Rosters[name]
	if !ok {
		return s, fmt.Errorf("no roster %q in the config", name)
	}
	var roster []minds.Spec
	for _, spec := range all {
		if !spec.Off {
			roster = append(roster, spec)
		}
	}
	if len(roster) < minSeats {
		return s, fmt.Errorf("roster %s has %d seats on; a council needs %d at least", name, len(roster), minSeats)
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

	in     io.Reader     // the chair's lines; nil is standard input
	out    io.Writer     // where events go in the events modes; nil is the console
	emitMu sync.Mutex    // keeps event lines whole
	quit   chan struct{} // closed on SIGTERM
}

// event is one line of the events modes: what happened, for a program to
// show. Type says which fields are set: session, round, call, retry, entry,
// cost, report, review, note, error and end.
type event struct {
	Type     string       `json:"type"`
	TS       time.Time    `json:"ts"`
	Dir      string       `json:"dir,omitempty"`
	Repo     string       `json:"repo,omitempty"`
	Seats    []minds.Spec `json:"seats,omitempty"`
	PR       *pr.Info     `json:"pr,omitempty"`
	DryRun   bool         `json:"dryRun,omitempty"`
	Round    int          `json:"round,omitempty"`
	Speakers []string     `json:"speakers,omitempty"`
	Seat     string       `json:"seat,omitempty"`
	Text     string       `json:"text,omitempty"`
	Entry    *floor.Entry `json:"entry,omitempty"` // without its raw exchange
	Cost     *costs       `json:"cost,omitempty"`
	Path     string       `json:"path,omitempty"`
	URL      string       `json:"url,omitempty"`
}

// emit writes one event in the events modes, and does nothing otherwise.
func (c *console) emit(e event) {
	if c.out == nil {
		return
	}
	e.TS = time.Now().UTC()
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	c.emitMu.Lock()
	defer c.emitMu.Unlock()
	_, _ = c.out.Write(append(b, '\n'))
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
	if c.out != nil {
		c.emit(event{Type: "note", Text: s})
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Println(s)
}

// sayCost shows what the session cost so far.
func (c *console) sayCost() {
	if c.out != nil {
		cs := c.costs()
		c.emit(event{Type: "cost", Cost: &cs})
		return
	}
	c.say(c.cost())
}

// notifier shows a seat's tool calls and retries as they happen.
func (c *console) notifier(seat, label string) func(string) {
	return func(call string) {
		switch {
		case c.out != nil:
			kind := "call"
			if strings.HasPrefix(call, "retrying after ") {
				kind = "retry"
			}
			c.emit(event{Type: kind, Seat: seat, Text: call})
		case !c.quiet:
			c.say(c.color("2", "  "+label+" · "+call))
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
	if c.out != nil {
		c.emit(event{Type: "round", Round: round, Speakers: seats})
		return
	}
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
	if c.out != nil {
		bare := e
		bare.Raw = nil
		c.emit(event{Type: "entry", Entry: &bare})
		if e.Usage != nil {
			cs := c.costs()
			c.emit(event{Type: "cost", Cost: &cs})
		}
		if folded != "" {
			c.emit(event{Type: "report", Path: folded})
		} else if failed {
			c.emit(event{Type: "error", Text: "the fold failed; /fold again or pick another seat"})
		}
		return
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
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
	go func() {
		for sig := range interrupts {
			if sig == syscall.SIGTERM {
				// Whoever started the council ends it: like /quit, so the
				// session folder is left clean.
				c.hold()
				c.f.Stop()
				close(c.quit)
				return
			}
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

	src := c.in
	if src == nil {
		src = os.Stdin
	}
	in := bufio.NewScanner(src)
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
			c.sayCost()
			return nil
		case <-c.quit:
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
		c.sayCost()
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
		c.sayCost()
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

// seatCost is what one seat's turns cost.
type seatCost struct {
	Seat       string  `json:"seat"`
	Name       string  `json:"name"`
	Turns      int     `json:"turns"`
	Input      int64   `json:"input"`
	CacheRead  int64   `json:"cacheRead"`
	CacheWrite int64   `json:"cacheWrite"`
	Output     int64   `json:"output"`
	Searches   int64   `json:"searches"`
	USD        float64 `json:"usd"`
	Unpriced   bool    `json:"unpriced,omitempty"` // a turn whose model has no price
	Retried    bool    `json:"retried,omitempty"`  // a turn whose broken try may have cost more than it reported
}

// costs is what the session cost so far, per seat, in the order seats spoke.
type costs struct {
	Seats []seatCost `json:"seats"`
	USD   float64    `json:"usd"`
}

// costs sums each seat's usage and prices it with the config's prices.
func (c *console) costs() costs {
	var out costs
	index := map[string]int{}
	for _, e := range c.f.Log.Snapshot() {
		if e.Usage == nil {
			continue
		}
		i, ok := index[e.From]
		if !ok {
			i = len(out.Seats)
			index[e.From] = i
			out.Seats = append(out.Seats, seatCost{Seat: e.From, Name: c.label(e.From)})
		}
		s := &out.Seats[i]
		u := *e.Usage
		s.Turns++
		s.Input, s.CacheRead, s.CacheWrite, s.Output = s.Input+u.Input, s.CacheRead+u.CacheRead, s.CacheWrite+u.CacheWrite, s.Output+u.Output
		s.Searches += u.Searches
		s.Retried = s.Retried || len(e.Retries) > 0
		p, ok := c.cfg.Prices[e.Served]
		if !ok {
			p, ok = c.cfg.Prices[e.Model]
		}
		if !ok {
			s.Unpriced = true
			continue
		}
		write := p.CacheWrite
		if write == 0 {
			write = p.In
		}
		usd := (float64(u.Input)*p.In+float64(u.CacheRead)*p.CacheRead+float64(u.CacheWrite)*write+float64(u.Output)*p.Out)/1e6 +
			float64(u.Searches)*p.Search
		s.USD += usd
		out.USD += usd
	}
	return out
}

// cost is costs as the console's table.
func (c *console) cost() string {
	cs := c.costs()
	var b strings.Builder
	fmt.Fprintf(&b, "%-10s %5s %9s %9s %9s %9s %8s %9s\n", "seat", "turns", "input", "cached", "written", "output", "searches", "est. $")
	for _, s := range cs.Seats {
		usd := fmt.Sprintf("%.4f", s.USD)
		if s.Unpriced {
			usd += "+"
		}
		if s.Retried {
			usd += "*"
		}
		fmt.Fprintf(&b, "%-10s %5d %9d %9d %9d %9d %8d %9s\n", s.Name, s.Turns, s.Input, s.CacheRead, s.CacheWrite, s.Output, s.Searches, usd)
	}
	fmt.Fprintf(&b, "total est. $%.4f (prices from the config; + marks turns with no price; * marks a retried turn, whose broken try may have cost more than it reported; MCP searches are in your MiniMax plan)", cs.USD)
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
