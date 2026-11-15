package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/delta-9/council-of-castaka/internal/floor"
	"github.com/delta-9/council-of-castaka/internal/minds"
	"github.com/delta-9/council-of-castaka/internal/pr"
	"github.com/delta-9/council-of-castaka/internal/tools"
)

const (
	reviewRounds = 3        // rounds a review grants each chair message, unless -n says otherwise
	diffLimit    = 60 << 10 // the largest diff the opening message carries; above it, the file list
	bodyLimit    = 65000    // GitHub takes 65,536 characters in a comment
)

const reviewFoldPrompt = "Fold the council's review into the final review of the pull request, as a standalone Markdown document posted on it. " +
	"Lead with the verdict: merge as it is, merge after fixes, or rework. Then the findings, most severe first, each with its file:line at the head, " +
	"who raised it, what it breaks and the fix. Then what the council checked and found sound, and what is still disputed and who holds which position. " +
	"Check with your tools anything you are unsure of before you write. The word limit does not apply. " +
	"End with the inline comments for the code: a fenced code block tagged inline holding a JSON array of objects with path, line and body. " +
	"The path is from the repository root, the line is in the file at the head commit and must be one the diff adds or shows, " +
	"and the body is the comment in Markdown, where a ```suggestion block proposes the line's replacement. " +
	"At most 10, for findings worth a comment on their line; an empty array when there are none."

// Where a review session stands (console.mu guards it).
const (
	debating = iota
	folding
	posting
)

// review binds the session to a pull request: each message is posted on it as
// it lands, and the fold becomes the review (docs/council.md, Pull request
// reviews).
type review struct {
	info   pr.Info
	lines  map[string]map[int]bool // where the diff takes inline comments
	poster *pr.Poster
	foldBy string        // the seat that folds
	open   string        // the opening message, until it is posted
	focus  string        // the chair's -m
	jobs   chan func()   // the posts, in the order the messages landed
	done   chan struct{} // closed once the review is posted or the session gave up
	url    string        // where the review went
	err    error

	stage int  // debating, folding or posting
	held  bool // the chair stopped: no fold on quiet until the next message
}

// prepareReview reads pull request n of the repository the council runs in,
// makes its commits readable, and returns it with its diff ("" if GitHub
// gives none).
func prepareReview(n int) (pr.Info, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return pr.Info{}, "", err
	}
	repo := tools.Find(cwd)
	if repo == nil {
		return pr.Info{}, "", errors.New("--pr runs inside a checkout of the pull request's GitHub repository")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	info, err := pr.Load(ctx, pr.Run, repo.Name, n)
	if err != nil {
		return info, "", err
	}
	diff, err := pr.Diff(ctx, pr.Run, info)
	if err != nil {
		fmt.Fprintf(os.Stderr, "council: no diff from GitHub (%v): the minds get the file list, and every inline comment goes into the review's body\n", err)
		diff = ""
	}
	if err := pr.Fetch(ctx, pr.Run, repo.Root, repo.Remote, info); err != nil {
		return info, "", fmt.Errorf("fetching the pull request's commits: %w", err)
	}
	return info, diff, nil
}

// reviewCharter is the charter's paragraph for a review session.
func reviewCharter(info pr.Info) string {
	return fmt.Sprintf("This session reviews pull request #%d of %s at its head %s. Every reply you write is posted on the pull request as a comment, "+
		"under the chair's account and headed with your name: write for its author and its reviewers, about the code, never about people. "+
		"Read the pull request's files with read_file's rev set to %s, so that the lines you cite are the head's.",
		info.Number, info.Repo, short(info.HeadOid), info.HeadOid)
}

// reviewOpening is the chair's first message of a review session.
func reviewOpening(info pr.Info, diff, focus string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review pull request #%d of %s, %q (%s). It merges %s, at its head %s, into %s, at %s.\n\n",
		info.Number, info.Repo, info.Title, info.URL, info.Head, info.HeadOid, info.Base, info.BaseOid)
	if body := strings.TrimSpace(info.Body); body != "" {
		fmt.Fprintf(&b, "Its description, as its author wrote it:\n\n<description>\n%s\n</description>\n\n", body)
	}
	fmt.Fprintf(&b, "Review what it changes: whether it is correct, what it breaks, what it misses (tests, edge cases, docs), and how to fix what you find. "+
		"Read its files with read_file at rev %s, and its diff with git diff %s...%s. Cite file:line at the head.", info.HeadOid, info.BaseOid, info.HeadOid)
	if focus != "" {
		fmt.Fprintf(&b, "\n\nThe chair adds: %s", focus)
	}
	if diff != "" && len(diff) <= diffLimit {
		fmt.Fprintf(&b, "\n\nThe diff, as GitHub shows it:\n\n````diff\n%s\n````", strings.TrimRight(diff, "\n"))
		return b.String()
	}
	reason := "GitHub gave no diff"
	if diff != "" {
		reason = fmt.Sprintf("The diff is %d KB, too long to include", len(diff)>>10)
	}
	fmt.Fprintf(&b, "\n\n%s. The files it changes, with the lines added and removed:\n", reason)
	for _, f := range info.Files {
		fmt.Fprintf(&b, "\n- %s (+%d −%d)", f.Path, f.Additions, f.Deletions)
	}
	return b.String()
}

// startReview binds the console to the session's pull request. A new session
// gets its opening message; a resumed one posts what it had not posted yet,
// up to its first fold.
func (c *console) startReview(foldBy, focus string, fresh bool) error {
	info := *c.sess.PR
	diff, err := os.ReadFile(filepath.Join(c.dir, "pr.diff"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	dry := ""
	if c.sess.DryRun {
		dry = filepath.Join(c.dir, "github")
	}
	poster, err := pr.Open(filepath.Join(c.dir, "pr.json"), info, pr.Run, dry)
	if err != nil {
		return err
	}
	if url := poster.Reviewed(); url != "" {
		return fmt.Errorf("this session already posted its review, %s: start a new session to review again", url)
	}
	if foldBy == "" {
		foldBy = c.f.Seats[0].ID
	}
	s := c.f.Seat(foldBy)
	if s == nil {
		return fmt.Errorf("no seat %s to fold the review", foldBy)
	}
	c.rv = &review{info: info, lines: pr.Lines(string(diff)), poster: poster, foldBy: s.ID, focus: focus,
		jobs: make(chan func(), 1024), done: make(chan struct{})}
	if fresh {
		c.rv.open = reviewOpening(info, string(diff), focus)
	}
	go func() {
		for job := range c.rv.jobs {
			job()
		}
	}()
	c.f.OnQuiet = c.onQuiet
	for _, e := range c.f.Log.Snapshot() {
		if e.From == "chair" && strings.HasSuffix(e.Text, reviewFoldPrompt) {
			break
		}
		c.mu.Lock()
		c.queueComment(e)
		c.mu.Unlock()
	}
	return nil
}

// openReview posts the opening message of a new review session.
func (c *console) openReview() {
	c.mu.Lock()
	text := c.rv.open
	c.rv.open = ""
	c.mu.Unlock()
	if text != "" {
		c.post(text)
	}
}

// onQuiet folds the review once the floor goes quiet, unless the chair holds
// the fold or one is under way.
func (c *console) onQuiet() {
	c.mu.Lock()
	ready := c.rv.stage == debating && !c.rv.held
	c.mu.Unlock()
	if !ready {
		return
	}
	if err := c.fold(c.rv.foldBy); err != nil {
		c.say("fold: " + err.Error())
	}
}

// hold keeps the fold from starting on its own after the chair's stop, which
// also cancels a fold under way.
func (c *console) hold() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rv == nil {
		return
	}
	c.rv.held = true
	if c.rv.stage == folding {
		c.rv.stage, c.folding = debating, ""
	}
}

// release lets the fold start on its own again.
func (c *console) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rv != nil {
		c.rv.held = false
	}
}

// reviewEntry handles an entry of a review session, under c.mu: the review
// once the fold landed, or the message's comment.
func (c *console) reviewEntry(e floor.Entry, fold, failed bool) {
	rv := c.rv
	switch {
	case fold && !failed:
		c.queueReview(e)
	case fold:
		rv.stage, rv.held = debating, true
		if c.quiet {
			rv.jobs <- func() {
				rv.err = fmt.Errorf("the fold failed: %s", e.Text)
				close(rv.done)
			}
		}
	default:
		c.queueComment(e)
	}
}

// queueComment queues the post of a message, under c.mu. The opening becomes a
// header without the diff; the fold request, passes and errors stay local.
func (c *console) queueComment(e floor.Entry) {
	rv := c.rv
	if e.Kind != "msg" || rv.poster.Posted(e.Seq) {
		return
	}
	var body string
	switch name, isFile := strings.CutPrefix(e.Text, "Attached `"); {
	case e.Seq == 1:
		body = c.reviewHeader()
	case e.From == "chair" && strings.HasSuffix(e.Text, reviewFoldPrompt):
		return
	case e.From == "chair" && isFile:
		name, _, _ = strings.Cut(name, "`")
		body = fmt.Sprintf("**Chair** attached `%s` for the council; the file is not posted here.", name)
	case e.From == "chair":
		body = "**Chair**\n\n" + e.Text
	default:
		body = fmt.Sprintf("I am %s.\n\n%s", c.label(e.From), e.Text)
	}
	seq := e.Seq
	rv.jobs <- func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, err := rv.poster.Comment(ctx, seq, fit(body))
		if err != nil {
			time.Sleep(2 * time.Second)
			_, err = rv.poster.Comment(ctx, seq, fit(body))
		}
		if err != nil {
			c.warn(fmt.Sprintf("#%d was not posted on the pull request (a resume retries it): %v", seq, err))
		}
	}
}

// queueReview queues the review the fold e wrote, after every comment queued
// before it, under c.mu.
func (c *console) queueReview(e floor.Entry) {
	rv := c.rv
	rv.stage = posting
	body, comments, splitErr := pr.Split(e.Text)
	body, inline := pr.Place(body, comments, rv.lines)
	body = fmt.Sprintf("**The council's review**, folded by %s\n\n%s", c.label(e.From), body)
	rv.jobs <- func() {
		if splitErr != nil {
			c.warn("the review keeps its inline block in its body: " + splitErr.Error())
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		rv.url, rv.err = rv.poster.Review(ctx, fit(body), inline)
		switch {
		case rv.err != nil:
			rv.err = fmt.Errorf("the review was not posted (the report is in %s): %w", filepath.Join(c.dir, "report.md"), rv.err)
			if !c.quiet {
				c.warn(rv.err.Error())
			}
		case c.out != nil:
			c.emit(event{Type: "review", URL: rv.url, Text: fmt.Sprintf("%d inline comments", len(inline))})
		case !c.quiet:
			c.say(c.color("1", fmt.Sprintf("review posted, %d inline comments: %s", len(inline), rv.url)))
		}
		close(rv.done)
	}
}

// reviewHeader is the opening's comment: who reviews, at which commit.
func (c *console) reviewHeader() string {
	members := make([]string, len(c.sess.Roster))
	for i, s := range c.sess.Roster {
		name := s.Seat
		if s.Name != "" {
			name = s.Name
		}
		members[i] = fmt.Sprintf("%s (%s, %s)", name, s.Model, minds.Lab(s.Provider))
	}
	h := fmt.Sprintf("**The council reviews this pull request** at %s: %s. Each member's replies follow as comments, headed with its name, "+
		"and the review the council folds comes last.", short(c.rv.info.HeadOid), strings.Join(members, ", "))
	if c.rv.focus != "" {
		h += "\n\n**Chair:** " + c.rv.focus
	}
	return h
}

// fit keeps a post within GitHub's limit, cut on a character boundary.
func fit(s string) string {
	if len(s) <= bodyLimit {
		return s
	}
	cut := bodyLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n\n[cut at 65,000 characters; the whole text is in the session folder]"
}

// warn reports a failed post: in red on the console, on stderr with -q, as
// an error event in the events modes.
func (c *console) warn(s string) {
	if c.out != nil {
		c.emit(event{Type: "error", Text: s})
		return
	}
	if c.quiet {
		fmt.Fprintln(os.Stderr, "council:", s)
		return
	}
	c.say(c.color("31", s))
}

func short(oid string) string { return oid[:min(len(oid), 7)] }
