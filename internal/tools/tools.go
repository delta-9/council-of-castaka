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
	Root   string // the repository's top level
	Name   string // owner/name of its GitHub remote
	Remote string // that remote's name in the repository
}

var githubRemote = regexp.MustCompile(`(?m)^(\S+)\s+\S*github\.com[:/]([\w.-]+/[\w.-]+?)(?:\.git)?\s`)

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
	return &Repo{Root: top, Name: m[2], Remote: m[1]}
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

// ReadFile returns lines of a file git tracks, numbered from start: as the
// working tree has it, or as the commit rev has it.
func (r *Repo) ReadFile(ctx context.Context, path string, start, lines int, rev string) (string, error) {
	rel, err := r.relative(path)
	if err != nil {
		return "", err
	}
	var data []byte
	if rev != "" {
		data, err = r.blob(ctx, rev, rel)
	} else {
		data, err = r.tracked(ctx, rel)
	}
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

// tracked reads a file of the working tree that git tracks, refusing a
// symlink that leads out of the repository.
func (r *Repo) tracked(ctx context.Context, rel string) ([]byte, error) {
	if _, err := git(ctx, r.Root, "--literal-pathspecs", "ls-files", "--error-unmatch", "--", rel); err != nil {
		return nil, fmt.Errorf("%s is not tracked by git; only tracked files are readable", rel)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(r.Root, rel))
	if err != nil {
		return nil, err
	}
	if !r.inside(real) {
		return nil, fmt.Errorf("%s points outside the repository", rel)
	}
	return os.ReadFile(real)
}

// blobLimit bounds a file read at a commit.
const blobLimit = 8 << 20

// blob reads a regular file as the commit rev has it: a symlink or a
// submodule there is refused.
func (r *Repo) blob(ctx context.Context, rev, rel string) ([]byte, error) {
	if strings.HasPrefix(rev, "-") {
		return nil, fmt.Errorf("%s is not a revision", rev)
	}
	out, err := git(ctx, r.Root, "--literal-pathspecs", "ls-tree", rev, "--", rel)
	if err != nil {
		return nil, fmt.Errorf("no commit %s in the repository", rev)
	}
	meta, name, _ := strings.Cut(strings.TrimSpace(out), "\t")
	fields := strings.Fields(meta)
	if name != rel || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
		return nil, fmt.Errorf("%s is not a file at %s", rel, rev)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "git", "-C", r.Root, "cat-file", "blob", fields[2]).Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %v", err)
	}
	if len(data) > blobLimit {
		return nil, fmt.Errorf("%s at %s is over %d MB", rel, rev, blobLimit>>20)
	}
	return data, nil
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
	// Rev gives read_file a rev parameter, for a review session. Other
	// sessions keep the tool as it was: Fable's history check compares the
	// tool list, so a resumed session must see the same one.
	Rev bool
}

type input struct {
	Path  string   `json:"path"`
	Start float64  `json:"start"`
	Lines float64  `json:"lines"`
	Rev   string   `json:"rev"`
	Args  []string `json:"args"`
	Query string   `json:"query"`
}

// Tools lists the seat's local tools.
func (k Kit) Tools() []minds.Tool {
	var list []minds.Tool
	if k.Repo != nil {
		read := minds.Tool{
			Name:        "read_file",
			Description: "Read a file of the repository " + k.Repo.Name + " that git tracks, with line numbers. Untracked and ignored files are not readable.",
			Properties: map[string]any{
				"path":  map[string]any{"type": "string", "description": "Path from the repository root"},
				"start": map[string]any{"type": "integer", "description": "First line to read (default 1)"},
				"lines": map[string]any{"type": "integer", "description": "How many lines (default 400, at most 2000)"},
			},
			Required: []string{"path"},
		}
		if k.Rev {
			read.Description += " With rev, the file as that commit has it, with that commit's line numbers."
			read.Properties["rev"] = map[string]any{"type": "string", "description": "A commit to read the file at, instead of the working tree"}
		}
		list = append(list,
			read,
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
		rev := ""
		if k.Rev {
			rev = in.Rev
		}
		return k.Repo.ReadFile(ctx, in.Path, int(in.Start), int(in.Lines), rev)
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
		path := in.Path
		if in.Rev != "" {
			path += "@" + in.Rev[:min(len(in.Rev), 7)]
		}
		if in.Start > 1 {
			return fmt.Sprintf("read %s:%d", path, int(in.Start))
		}
		return "read " + path
	case "git":
		return "git " + strings.Join(in.Args, " ")
	case "web_search":
		return "web search: " + in.Query
	}
	return name
}
