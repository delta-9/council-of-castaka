// Package tools gives the minds read-only access to the repository the
// council runs in, limited to what git tracks, and web search through an MCP
// server.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/delta-9/council-of-castaka/internal/minds"
)

const (
	gitLimit  = 64 << 10  // bytes of git output a call returns
	fileLimit = 100 << 10 // bytes of a file a call returns
)

// Repo is a GitHub repository the minds can read: what git tracks, nothing else.
type Repo struct {
	Root string // the repository's top level
	Name string // owner/name of its GitHub remote
}

var githubRemote = regexp.MustCompile(`github\.com[:/]([\w.-]+/[\w.-]+?)(?:\.git)?\s`)

// Find returns the GitHub repository that contains dir, or nil when dir is
// not inside a git repository with a GitHub remote.
func Find(dir string) *Repo {
	top, err := git(context.Background(), dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil
	}
	top = strings.TrimSpace(top)
	remotes, err := git(context.Background(), top, "remote", "-v")
	if err != nil {
		return nil
	}
	m := githubRemote.FindStringSubmatch(remotes)
	if m == nil {
		return nil
	}
	return &Repo{Root: top, Name: m[1]}
}

// gitCommands are the read-only subcommands the git tool runs.
var gitCommands = []string{"log", "show", "diff", "blame", "grep", "ls-files", "ls-tree", "status", "describe", "rev-parse", "shortlog"}

// refused are options that would write a file, read past what git tracks, or
// run a program; an abbreviation of one is refused too.
var refused = []string{"--output", "--no-index", "--contents", "--untracked", "--open-files-in-pager", "--ext-diff", "--ignore-revs-file"}

// refusedShort are, per subcommand, short options with the same effects:
// grep's -O runs a pager and -f reads patterns from any file; blame's -S
// reads revisions from any file.
var refusedShort = map[string]string{"grep": "Of", "blame": "S"}

// checkGit returns why args may not run, or nil.
func checkGit(args []string) error {
	if len(args) == 0 || !contains(gitCommands, args[0]) {
		return fmt.Errorf("the git tool runs only: %s", strings.Join(gitCommands, ", "))
	}
	for _, a := range args[1:] {
		if a == "--" {
			break // what follows is paths
		}
		if name, _, _ := strings.Cut(a, "="); strings.HasPrefix(name, "--") && len(name) > 2 {
			for _, r := range refused {
				if strings.HasPrefix(r, name) {
					return fmt.Errorf("git %s: %s is refused here", args[0], a)
				}
			}
		} else if strings.HasPrefix(a, "-") && strings.ContainsAny(a[1:], refusedShort[args[0]]) {
			return fmt.Errorf("git %s: %s is refused here", args[0], a)
		}
	}
	return nil
}

// Git runs a read-only git command in the repository.
func (r *Repo) Git(ctx context.Context, args []string) (string, error) {
	if err := checkGit(args); err != nil {
		return "", err
	}
	if contains([]string{"log", "show", "diff"}, args[0]) {
		args = append([]string{args[0], "--no-ext-diff"}, args[1:]...)
	}
	return git(ctx, r.Root, args...)
}

// ReadFile returns lines of a file git tracks, numbered from start.
func (r *Repo) ReadFile(ctx context.Context, path string, start, lines int) (string, error) {
	rel, err := r.relative(path)
	if err != nil {
		return "", err
	}
	if _, err := git(ctx, r.Root, "--literal-pathspecs", "ls-files", "--error-unmatch", "--", rel); err != nil {
		return "", fmt.Errorf("%s is not tracked by git; only tracked files are readable", rel)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(r.Root, rel))
	if err != nil {
		return "", err
	}
	if !r.inside(real) {
		return "", fmt.Errorf("%s points outside the repository", rel)
	}
	data, err := os.ReadFile(real)
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return "", fmt.Errorf("%s is a binary file", rel)
	}
	all := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	start = max(start, 1)
	if lines < 1 {
		lines = 400
	}
	end := min(start-1+min(lines, 2000), len(all))
	var b strings.Builder
	last := start - 1
	for i := start - 1; i < end && b.Len() < fileLimit; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, all[i])
		last = i + 1
	}
	if last < len(all) {
		fmt.Fprintf(&b, "[lines %d-%d of %d; read on with start=%d]\n", start, last, len(all), last+1)
	}
	return b.String(), nil
}

// relative turns path into a clean path under the root, or refuses it.
func (r *Repo) relative(path string) (string, error) {
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(r.Root, path)
		if err != nil {
			return "", err
		}
		path = rel
	}
	path = filepath.Clean(path)
	if path == "." || path == ".." || strings.HasPrefix(path, "../") || filepath.IsAbs(path) {
		return "", fmt.Errorf("%s is outside the repository", path)
	}
	return path, nil
}

// inside reports whether a resolved path is under the root, symlinks resolved.
func (r *Repo) inside(real string) bool {
	root, err := filepath.EvalSymlinks(r.Root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// git runs git in dir with no pager, no fsmonitor hook and no optional locks,
// so a read never rewrites the index; the output is capped.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "--no-pager", "-c", "core.fsmonitor=false", "-c", "core.pager=cat"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0")
	out := &capped{max: gitLimit}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// capped keeps the first max bytes written to it.
type capped struct {
	b   strings.Builder
	max int
	cut bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); len(p) > room {
		c.b.Write(p[:max(room, 0)])
		c.cut = true
	} else {
		c.b.Write(p)
	}
	return len(p), nil
}

func (c *capped) String() string {
	if c.cut {
		return c.b.String() + "\n[output cut]"
	}
	return c.b.String()
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Kit is what one seat can call locally: the repository tools, and web search
// through the MCP server when the seat does not search natively.
type Kit struct {
	Repo   *Repo // nil: no repository tools
	Search *MCP  // nil: no MCP web search
}

type input struct {
	Path  string   `json:"path"`
	Start float64  `json:"start"`
	Lines float64  `json:"lines"`
	Args  []string `json:"args"`
	Query string   `json:"query"`
}

// Tools lists the seat's local tools.
func (k Kit) Tools() []minds.Tool {
	var list []minds.Tool
	if k.Repo != nil {
		list = append(list,
			minds.Tool{
				Name:        "read_file",
				Description: "Read a file of the repository " + k.Repo.Name + " that git tracks, with line numbers. Untracked and ignored files are not readable.",
				Properties: map[string]any{
					"path":  map[string]any{"type": "string", "description": "Path from the repository root"},
					"start": map[string]any{"type": "integer", "description": "First line to read (default 1)"},
					"lines": map[string]any{"type": "integer", "description": "How many lines (default 400, at most 2000)"},
				},
				Required: []string{"path"},
			},
			minds.Tool{
				Name: "git",
				Description: "Run a read-only git command in the repository " + k.Repo.Name + ": " + strings.Join(gitCommands, ", ") +
					`. Pass the arguments as a list, without "git", e.g. ["grep", "-n", "func Post", "--", "internal/"]. Output past 64 KB is cut.`,
				Properties: map[string]any{
					"args": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "The subcommand and its arguments"},
				},
				Required: []string{"args"},
			})
	}
	if k.Search != nil {
		list = append(list, minds.Tool{
			Name:        "web_search",
			Description: "Search the web for fresh facts; returns results with their links.",
			Properties:  map[string]any{"query": map[string]any{"type": "string", "description": "What to search for"}},
			Required:    []string{"query"},
		})
	}
	return list
}

// Run runs one of the seat's local tools.
func (k Kit) Run(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("bad input for %s: %w", name, err)
	}
	switch {
	case name == "read_file" && k.Repo != nil:
		return k.Repo.ReadFile(ctx, in.Path, int(in.Start), int(in.Lines))
	case name == "git" && k.Repo != nil:
		return k.Repo.Git(ctx, in.Args)
	case name == "web_search" && k.Search != nil:
		return k.Search.Search(ctx, in.Query)
	}
	return "", fmt.Errorf("no tool %s", name)
}

// Describe names a tool call for the chair.
func (k Kit) Describe(name string, raw json.RawMessage) string {
	var in input
	_ = json.Unmarshal(raw, &in)
	switch name {
	case "read_file":
		if in.Start > 1 {
			return fmt.Sprintf("read %s:%d", in.Path, int(in.Start))
		}
		return "read " + in.Path
	case "git":
		return "git " + strings.Join(in.Args, " ")
	case "web_search":
		return "web search: " + in.Query
	}
	return name
}
