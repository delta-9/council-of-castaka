package pr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// diff is a pull request's diff: two hunks of one file, a deleted file and a
// renamed file whose last line has no newline. An empty context line is a
// single space, as git writes it.
var diff = strings.Join([]string{
	"diff --git a/a.go b/a.go",
	"index 111..222 100644",
	"--- a/a.go",
	"+++ b/a.go",
	"@@ -1,4 +1,5 @@",
	" package a",
	"-func old() {}",
	"+func renamed() {}",
	"+func added() {}",
	" ",
	" // tail",
	"@@ -20,3 +21,3 @@ func f() {",
	" \tx := 1",
	"-\ty := 2",
	"+\ty := 3",
	" \treturn",
	"diff --git a/gone.go b/gone.go",
	"deleted file mode 100644",
	"index 333..000",
	"--- a/gone.go",
	"+++ /dev/null",
	"@@ -1,2 +0,0 @@",
	"-package gone",
	"-var x = 1",
	"diff --git a/old/name.go b/new/name.go",
	"similarity index 90%",
	"rename from old/name.go",
	"rename to new/name.go",
	"index 444..555 100644",
	"--- a/old/name.go",
	"+++ b/new/name.go",
	"@@ -3,2 +3,3 @@",
	" a",
	"+++ b, an added line that looks like a header",
	" c",
	`\ No newline at end of file`,
	"",
}, "\n")

// TestLines checks which lines of the head take an inline comment: those the
// diff shows on its new side, context included. GitHub refuses a whole review
// over a single line outside them.
func TestLines(t *testing.T) {
	lines := Lines(diff)
	want := map[string][]int{"a.go": {1, 2, 3, 4, 5, 21, 22, 23}, "new/name.go": {3, 4, 5}}
	for path, nums := range want {
		for _, n := range nums {
			if !lines[path][n] {
				t.Errorf("%s:%d is in the diff, but Lines left it out", path, n)
			}
		}
		if len(lines[path]) != len(nums) {
			t.Errorf("%s: lines %v, want %v", path, lines[path], nums)
		}
	}
	for _, path := range []string{"gone.go", "old/name.go"} {
		if len(lines[path]) > 0 {
			t.Errorf("%s has no new side, but Lines gave it %v", path, lines[path])
		}
	}
}

// report ends with the inline block the fold writes, a heading before it.
const report = "## Verdict\n\nMerge after one fix.\n\n## Inline comments\n\n```inline\n" +
	`[{"path": "a.go", "line": 2, "body": "Call it ` + "`newName`" + `."},` +
	` {"path": "a.go", "line": 10, "body": "Outside the diff."},` +
	` {"path": "gone.go", "line": 1, "body": "Deleted."},` +
	` {"path": "./new/name.go", "line": 4, "body": "Two lines:\nkeep both."}]` + "\n```\n"

// TestSplitAndPlace checks that the inline block comes off the report, that
// the comments the diff can hold stay inline, and that the others are listed
// in the body with their file and line, so none is lost.
func TestSplitAndPlace(t *testing.T) {
	body, comments, err := Split(report)
	if err != nil || len(comments) != 4 {
		t.Fatalf("Split: %d comments, %v", len(comments), err)
	}
	if strings.Contains(body, "```inline") || strings.Contains(body, "Inline comments") || !strings.HasSuffix(body, "Merge after one fix.") {
		t.Errorf("the body kept the block or its heading, or lost the report: %q", body)
	}
	body, inline := Place(body, comments, Lines(diff))
	if len(inline) != 2 || inline[0].Path != "a.go" || inline[1].Path != "new/name.go" || inline[1].Line != 4 {
		t.Errorf("inline %+v, want a.go:2 and new/name.go:4", inline)
	}
	for _, ref := range []string{"`a.go:10`", "`gone.go:1`"} {
		if !strings.Contains(body, ref) {
			t.Errorf("%s is off the diff and must be listed in the body: %q", ref, body)
		}
	}
	if strings.Contains(body, "`a.go:2`") {
		t.Errorf("a.go:2 went inline and must not be listed too: %q", body)
	}

	if b, c, err := Split("No block here."); b != "No block here." || c != nil || err != nil {
		t.Errorf("a report without a block: %q, %v, %v", b, c, err)
	}
	broken := "Report.\n\n```inline\n[{\"path\": \"a.go\", \"line\": \n```\n"
	if b, c, err := Split(broken); b != broken || c != nil || err == nil {
		t.Errorf("a block that does not parse must stay in the body, with an error: %q, %v, %v", b, c, err)
	}
}

type call struct {
	args  []string
	stdin string
}

// fakeGH answers gh api calls with a comment URL, and fails those fail picks.
type fakeGH struct {
	mu    sync.Mutex
	calls []call
	fail  func(call) bool
}

func (f *fakeGH) run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := call{append([]string{name}, args...), string(stdin)}
	f.calls = append(f.calls, c)
	if f.fail != nil && f.fail(c) {
		return nil, errors.New("gh: Unprocessable Entity (HTTP 422)")
	}
	return []byte(fmt.Sprintf(`{"html_url": "https://github.com/o/r/pull/7#c%d"}`, len(f.calls))), nil
}

func (f *fakeGH) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

var info = Info{Repo: "o/r", Number: 7, HeadOid: "abc123"}

// TestCommentsPostOnce checks that each message is posted once, to the pull
// request's conversation, and that a resumed session skips what it posted.
func TestCommentsPostOnce(t *testing.T) {
	gh := &fakeGH{}
	path := filepath.Join(t.TempDir(), "pr.json")
	p, err := Open(path, info, gh.run, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	url, err := p.Comment(ctx, 2, "**Morkaï** · round 1\n\nFirst look.")
	if err != nil || url == "" {
		t.Fatalf("Comment: %q, %v", url, err)
	}
	want := "gh api -X POST repos/o/r/issues/7/comments --input -"
	if got := strings.Join(gh.calls[0].args, " "); got != want {
		t.Errorf("ran %q, want %q", got, want)
	}
	var sent struct{ Body string }
	if json.Unmarshal([]byte(gh.calls[0].stdin), &sent) != nil || sent.Body != "**Morkaï** · round 1\n\nFirst look." {
		t.Errorf("sent %q", gh.calls[0].stdin)
	}
	if _, err := p.Comment(ctx, 2, "again"); err != nil || gh.count() != 1 {
		t.Errorf("a posted message went out again: %d calls, %v", gh.count(), err)
	}

	resumed, err := Open(path, info, gh.run, "")
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Posted(2) || resumed.Posted(3) {
		t.Errorf("the resumed poster lost what was posted")
	}
	if _, err := resumed.Comment(ctx, 3, "Second look."); err != nil || gh.count() != 2 {
		t.Errorf("a new message after a resume: %d calls, %v", gh.count(), err)
	}
}

// TestReviewFallsBackToBody checks the review's request, and that when GitHub
// refuses its inline comments, the review goes out with them in its body.
func TestReviewFallsBackToBody(t *testing.T) {
	gh := &fakeGH{fail: func(c call) bool { return strings.Contains(c.stdin, `"comments"`) }}
	p, err := Open(filepath.Join(t.TempDir(), "pr.json"), info, gh.run, "")
	if err != nil {
		t.Fatal(err)
	}
	url, err := p.Review(context.Background(), "Merge after one fix.", []Comment{{Path: "a.go", Line: 2, Body: "Rename it."}})
	if err != nil || url == "" || gh.count() != 2 {
		t.Fatalf("Review: %q, %v after %d calls, want a URL after two", url, err, gh.count())
	}
	var first, second struct {
		CommitID string `json:"commit_id"`
		Event    string
		Body     string
		Comments []struct {
			Path string
			Line int
			Side string
		}
	}
	_ = json.Unmarshal([]byte(gh.calls[0].stdin), &first)
	_ = json.Unmarshal([]byte(gh.calls[1].stdin), &second)
	if first.CommitID != "abc123" || first.Event != "COMMENT" || len(first.Comments) != 1 || first.Comments[0].Side != "RIGHT" {
		t.Errorf("first review %+v, want a comment-type review on abc123 with one inline comment on the new side", first)
	}
	if !strings.HasSuffix(gh.calls[1].args[4], "repos/o/r/pulls/7/reviews") || len(second.Comments) != 0 || !strings.Contains(second.Body, "`a.go:2`") {
		t.Errorf("the fallback review %v %+v must hold the comment in its body", gh.calls[1].args, second)
	}
	if resumed, _ := Open(p.path, info, gh.run, ""); resumed.Reviewed() != url {
		t.Errorf("the review's URL was not recorded")
	}
}

// TestDryRunWritesFiles checks that a dry run posts nothing and writes each
// comment and the review into its folder.
func TestDryRunWritesFiles(t *testing.T) {
	gh := &fakeGH{}
	dir := t.TempDir()
	p, err := Open(filepath.Join(dir, "pr.json"), info, gh.run, filepath.Join(dir, "github"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	comment, err := p.Comment(ctx, 2, "First look.")
	if err != nil {
		t.Fatal(err)
	}
	review, err := p.Review(ctx, "Verdict.", []Comment{{Path: "a.go", Line: 2, Body: "Rename it."}})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{comment, review} {
		if b, err := os.ReadFile(path); err != nil || len(b) == 0 {
			t.Errorf("%s: %v", path, err)
		}
	}
	if gh.count() != 0 {
		t.Errorf("a dry run called gh %d times", gh.count())
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestFetchMissingCommit checks that a head commit missing from the local
// repository is fetched by its SHA, without a new branch or a checkout.
func TestFetchMissingCommit(t *testing.T) {
	remote, local := t.TempDir(), t.TempDir()
	git(t, remote, "init", "-q")
	git(t, remote, "config", "uploadpack.allowAnySHA1InWant", "true")
	git(t, remote, "commit", "-q", "--allow-empty", "-m", "base")
	base := git(t, remote, "rev-parse", "HEAD")
	git(t, local, "clone", "-q", remote, ".")
	git(t, remote, "commit", "-q", "--allow-empty", "-m", "head")
	head := git(t, remote, "rev-parse", "HEAD")
	branches := git(t, local, "branch", "-a")

	if err := Fetch(context.Background(), Run, local, "origin", Info{BaseOid: base, HeadOid: head}); err != nil {
		t.Fatal(err)
	}
	git(t, local, "cat-file", "-e", head+"^{commit}")
	if got := git(t, local, "branch", "-a"); got != branches {
		t.Errorf("branches changed from %q to %q", branches, got)
	}
	if got := git(t, local, "rev-parse", "HEAD"); got != base {
		t.Errorf("the checkout moved to %s", got)
	}
}
