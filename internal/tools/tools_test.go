package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testRepo is a git repository with a GitHub remote holding one tracked
// file, plus what a mind must never read: an untracked .env.local, an ignored
// file, and a tracked symlink to a file outside the repository.
func testRepo(t *testing.T) *Repo {
	t.Helper()
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	files := map[string]string{
		"main.go":     "package main\n// line two\n",
		".gitignore":  "ignored.txt\n",
		"ignored.txt": "ignored secret\n",
		".env.local":  "KEY=untracked secret\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(outside, []byte("outside secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "main.go", ".gitignore", "link.txt"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "first commit"},
		{"remote", "add", "origin", "git@github.com:owner/repo.git"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	repo := Find(dir)
	if repo == nil || repo.Name != "owner/repo" {
		t.Fatalf("Find(%s) = %+v, want owner/repo", dir, repo)
	}
	return repo
}

// TestReadOnly checks that the minds read what git tracks and nothing else,
// and that no git option writes a file or reaches past the repository.
func TestReadOnly(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	if out, err := repo.ReadFile(ctx, "main.go", 2, 0, ""); err != nil || !strings.Contains(out, "2\t// line two") {
		t.Errorf("reading a tracked file: %q, %v", out, err)
	}
	for _, path := range []string{".env.local", "ignored.txt", "link.txt", "../secret.txt", filepath.Join(repo.Root, "..", "x")} {
		if out, err := repo.ReadFile(ctx, path, 1, 0, ""); err == nil || strings.Contains(out, "secret") {
			t.Errorf("read_file %s should be refused, got %q", path, out)
		}
	}

	written := filepath.Join(repo.Root, "written.txt")
	for _, args := range [][]string{
		{"push"},
		{"diff", "--output=" + written},
		{"log", "--outp=" + written},
		{"grep", "--no-index", "secret"},
		{"grep", "--untracked", "secret"},
		{"grep", "-nOcat", "package"},
		{"grep", "-f", ".env.local"},
		{"blame", "--contents=" + filepath.Join(repo.Root, ".env.local"), "main.go"},
		{"blame", "-S", ".env.local", "main.go"},
		{"diff", "--no-index", "main.go", ".env.local"},
	} {
		if out, err := repo.Git(ctx, args); err == nil || strings.Contains(out, "secret") {
			t.Errorf("git %v should be refused, got %q", args, out)
		}
	}
	if _, err := os.Stat(written); err == nil {
		t.Error("a git option wrote a file")
	}

	if out, err := repo.Git(ctx, []string{"grep", "-n", "line two"}); err != nil || !strings.Contains(out, "main.go:2:") {
		t.Errorf("git grep: %q, %v", out, err)
	}
	if out, err := repo.Git(ctx, []string{"log", "--oneline"}); err != nil || !strings.Contains(out, "first commit") {
		t.Errorf("git log: %q, %v", out, err)
	}
}

// TestReadFileAtRev checks that a review reads a file as a commit has it,
// with that commit's line numbers, whatever the working tree holds, and still
// reads nothing git does not track as a file.
func TestReadFileAtRev(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(repo.Root, "main.go"), []byte("changed in the working tree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := repo.ReadFile(ctx, "main.go", 2, 0, "HEAD"); err != nil || !strings.Contains(out, "2\t// line two") {
		t.Errorf("main.go at HEAD: %q, %v", out, err)
	}
	for _, c := range []struct{ path, rev string }{
		{"main.go", "--output=x"},
		{".env.local", "HEAD"},
		{"link.txt", "HEAD"},
		{"missing.go", "HEAD"},
		{"main.go", "no-such-rev"},
	} {
		if out, err := repo.ReadFile(ctx, c.path, 1, 0, c.rev); err == nil || strings.Contains(out, "secret") {
			t.Errorf("read_file %s at %s should be refused, got %q", c.path, c.rev, out)
		}
	}
	if repo.Remote != "origin" {
		t.Errorf("Find gave the remote %q, want origin", repo.Remote)
	}
}

// TestFindNeedsGitHub checks that a repository without a GitHub remote gets no tools.
func TestFindNeedsGitHub(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if repo := Find(dir); repo != nil {
		t.Errorf("Find on a repository with no GitHub remote = %+v, want nil", repo)
	}
}
