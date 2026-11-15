// Package pr binds a council session to a GitHub pull request: it reads the
// pull request with gh, posts each message as a comment, and posts the fold as
// a review whose inline comments sit on the lines the diff shows.
package pr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Info is the pull request as a session pins it when it starts.
type Info struct {
	Repo    string `json:"repo"` // owner/name
	Number  int    `json:"number"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Body    string `json:"body,omitempty"`
	Base    string `json:"base"`
	BaseOid string `json:"baseOid"`
	Head    string `json:"head"`
	HeadOid string `json:"headOid"` // the commit the session reviews
	Files   []File `json:"files"`
}

// File is a file the pull request changes.
type File struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// Runner runs a program with stdin and returns its standard output; tests
// replace it.
type Runner func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)

// Run is the Runner that runs the program.
func Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %v: %s", name, strings.Join(args[:min(len(args), 3)], " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Load reads pull request n of repo with gh.
func Load(ctx context.Context, run Runner, repo string, n int) (Info, error) {
	out, err := run(ctx, nil, "gh", "pr", "view", strconv.Itoa(n), "-R", repo,
		"--json", "number,title,body,url,baseRefName,baseRefOid,headRefName,headRefOid,files")
	if err != nil {
		return Info{}, err
	}
	var v struct {
		Number      int
		Title, Body string
		URL         string
		BaseRefName string
		BaseRefOid  string
		HeadRefName string
		HeadRefOid  string
		Files       []File
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return Info{}, fmt.Errorf("gh pr view: %w", err)
	}
	return Info{Repo: repo, Number: v.Number, URL: v.URL, Title: v.Title, Body: v.Body,
		Base: v.BaseRefName, BaseOid: v.BaseRefOid, Head: v.HeadRefName, HeadOid: v.HeadRefOid, Files: v.Files}, nil
}

// Diff returns the pull request's diff as GitHub computes it, which is what
// GitHub checks inline comments against. GitHub refuses very large diffs.
func Diff(ctx context.Context, run Runner, info Info) (string, error) {
	out, err := run(ctx, nil, "gh", "pr", "diff", strconv.Itoa(info.Number), "-R", info.Repo, "--color", "never")
	return string(out), err
}

// Fetch makes the base and head commits readable in the repository at root,
// fetching from remote by SHA those it lacks: no branch is created and the
// working tree is untouched.
func Fetch(ctx context.Context, run Runner, root, remote string, info Info) error {
	var missing []string
	for _, oid := range []string{info.BaseOid, info.HeadOid} {
		if oid == "" || strings.HasPrefix(oid, "-") {
			return fmt.Errorf("pull request %d has no commit %q", info.Number, oid)
		}
		if _, err := run(ctx, nil, "git", "-C", root, "cat-file", "-e", oid+"^{commit}"); err != nil {
			missing = append(missing, oid)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	_, err := run(ctx, nil, "git", append([]string{"-C", root, "fetch", "--no-tags", "--quiet", remote}, missing...)...)
	return err
}

var hunk = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// Lines maps each file of a unified diff to the lines of its new version that
// the diff shows, added or context: the lines GitHub takes inline comments on.
func Lines(diff string) map[string]map[int]bool {
	lines := map[string]map[int]bool{}
	var file map[int]bool
	header, n := false, 0
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			file, header, n = nil, true, 0
		case header && strings.HasPrefix(l, "+++ "):
			if path := diffPath(strings.TrimPrefix(l, "+++ ")); path != "" {
				file = map[int]bool{}
				lines[path] = file
			}
		case strings.HasPrefix(l, "@@ "): // inside a hunk, every line starts with +, - or a space
			if m := hunk.FindStringSubmatch(l); m != nil {
				header, n = false, atoi(m[1])
			}
		case header || file == nil || n == 0:
		case strings.HasPrefix(l, "+"), strings.HasPrefix(l, " "):
			file[n] = true
			n++
		}
	}
	return lines
}

// diffPath is the path of a "+++ b/path" line, unquoted the way git quotes
// unusual names; "" for /dev/null.
func diffPath(s string) string {
	if strings.HasPrefix(s, `"`) {
		if u, err := strconv.Unquote(s); err == nil {
			s = u
		}
	}
	path, ok := strings.CutPrefix(s, "b/")
	if !ok {
		return ""
	}
	return path
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// Comment is an inline comment the fold asks for, on a line of the head.
type Comment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
}

// Split takes the closing inline block off a report, with a heading that
// introduces it, and returns the report without it and the comments in it.
// A block that does not parse stays in the report, and the error says why.
func Split(report string) (string, []Comment, error) {
	i := strings.LastIndex(report, "```inline")
	if i < 0 {
		return report, nil, nil
	}
	rest := report[i+len("```inline"):]
	nl := strings.IndexByte(rest, '\n')
	end := -1
	if nl >= 0 {
		end = strings.Index(rest[nl:], "\n```")
	}
	if end < 0 {
		return report, nil, errors.New("the inline block is not closed")
	}
	var comments []Comment
	if err := json.Unmarshal([]byte(rest[nl+1:nl+end]), &comments); err != nil {
		return report, nil, fmt.Errorf("the inline block is not a JSON array of comments: %w", err)
	}
	body := strings.TrimRight(report[:i], " \t\n")
	if last := strings.LastIndexByte(body, '\n'); strings.HasPrefix(body[last+1:], "#") && strings.Contains(strings.ToLower(body[last+1:]), "inline") {
		body = strings.TrimRight(body[:last+1], " \t\n")
	}
	if after := strings.TrimSpace(rest[nl+end+len("\n```"):]); after != "" {
		body += "\n\n" + after
	}
	return body, comments, nil
}

// Place keeps inline the comments on lines the diff shows, and lists the
// others at the end of body with their file and line, so none is lost.
func Place(body string, comments []Comment, lines map[string]map[int]bool) (string, []Comment) {
	var inline []Comment
	var listed []string
	for _, c := range comments {
		c.Path = strings.TrimPrefix(c.Path, "./")
		if strings.TrimSpace(c.Body) == "" {
			continue
		}
		if lines[c.Path][c.Line] {
			inline = append(inline, c)
			continue
		}
		listed = append(listed, fmt.Sprintf("- `%s:%d`: %s", c.Path, c.Line, strings.ReplaceAll(strings.TrimSpace(c.Body), "\n", "\n  ")))
	}
	if len(listed) > 0 {
		body += "\n\n### On lines outside the diff\n\n" + strings.Join(listed, "\n")
	}
	return body, inline
}

// state is what a session has posted on its pull request, kept in pr.json.
type state struct {
	Posted map[int]string `json:"posted"` // the seq of each message posted → where it went
	Review string         `json:"review,omitempty"`
}

// Poster posts on one pull request, one call at a time, and records what it
// posted so that a resumed session posts only what is missing.
type Poster struct {
	info Info
	run  Runner
	path string // pr.json
	dry  string // a dry run's folder: the posts are written there instead

	mu    sync.Mutex
	state state
}

// Open returns the poster for info whose record is at path. With dry set, it
// writes each post into that folder instead of posting it.
func Open(path string, info Info, run Runner, dry string) (*Poster, error) {
	p := &Poster{info: info, run: run, path: path, dry: dry, state: state{Posted: map[int]string{}}}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &p.state); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if p.state.Posted == nil {
			p.state.Posted = map[int]string{}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return p, nil
}

// Posted reports whether the message seq was posted.
func (p *Poster) Posted(seq int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.state.Posted[seq]
	return ok
}

// Reviewed returns where the review went, or "".
func (p *Poster) Reviewed() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state.Review
}

// Comment posts the message seq to the pull request's conversation, unless it
// was posted already, and returns where it went.
func (p *Poster) Comment(ctx context.Context, seq int, body string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if url, ok := p.state.Posted[seq]; ok {
		return url, nil
	}
	url, err := p.send(ctx, fmt.Sprintf("issues/%d/comments", p.info.Number), fmt.Sprintf("comment-%04d", seq), body,
		map[string]string{"body": body})
	if err != nil {
		return "", err
	}
	p.state.Posted[seq] = url
	return url, p.save()
}

type reviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

type review struct {
	CommitID string          `json:"commit_id"`
	Event    string          `json:"event"`
	Body     string          `json:"body"`
	Comments []reviewComment `json:"comments,omitempty"`
}

// Review posts the review on the head the session pinned, as a comment: never
// an approval or a request for changes. GitHub refuses a whole review over
// one line it cannot place, so a refused review goes out again with its
// inline comments listed in its body.
func (p *Poster) Review(ctx context.Context, body string, comments []Comment) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	endpoint := fmt.Sprintf("pulls/%d/reviews", p.info.Number)
	r := review{CommitID: p.info.HeadOid, Event: "COMMENT", Body: body}
	for _, c := range comments {
		r.Comments = append(r.Comments, reviewComment{Path: c.Path, Line: c.Line, Side: "RIGHT", Body: c.Body})
	}
	url, err := p.send(ctx, endpoint, "review", reviewText(r), r)
	if err != nil && len(comments) > 0 {
		r.Body, _ = Place(body, comments, nil)
		r.Comments = nil
		url, err = p.send(ctx, endpoint, "review", reviewText(r), r)
	}
	if err != nil {
		return "", err
	}
	p.state.Review = url
	return url, p.save()
}

// reviewText is a review as a dry run writes it for reading.
func reviewText(r review) string {
	var b strings.Builder
	b.WriteString(r.Body)
	for _, c := range r.Comments {
		fmt.Fprintf(&b, "\n\n---\n`%s:%d`\n\n%s", c.Path, c.Line, c.Body)
	}
	return b.String()
}

// send posts payload to the pull request's endpoint with gh and returns the
// URL GitHub gives the post. A dry run writes the payload and its text into
// its folder under name instead, and returns the text's path.
func (p *Poster) send(ctx context.Context, endpoint, name, text string, payload any) (string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	if p.dry != "" {
		if err := os.MkdirAll(p.dry, 0o700); err != nil {
			return "", err
		}
		path := filepath.Join(p.dry, name+".md")
		if err := os.WriteFile(filepath.Join(p.dry, name+".json"), b, 0o600); err != nil {
			return "", err
		}
		return path, os.WriteFile(path, []byte(text+"\n"), 0o600)
	}
	out, err := p.run(ctx, b, "gh", "api", "-X", "POST", "repos/"+p.info.Repo+"/"+endpoint, "--input", "-")
	if err != nil {
		return "", err
	}
	var reply struct {
		URL string `json:"html_url"`
	}
	if err := json.Unmarshal(out, &reply); err != nil || reply.URL == "" {
		return "", fmt.Errorf("gh api %s: unexpected reply %.200q", endpoint, out)
	}
	return reply.URL, nil
}

func (p *Poster) save() error {
	b, err := json.MarshalIndent(p.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}
