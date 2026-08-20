package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	pluginapi "github.com/blairham/koi-shell/pkg/pluginapi/v1"
)

// completeRepo is initRepo plus the shapes completion serves: a second
// branch, a remote with a remote-tracking ref, and worktree changes.
func completeRepo(t *testing.T) string {
	t.Helper()
	dir := initRepo(t)
	gitIn(t, dir, "branch", "feature/login")
	gitIn(t, dir, "remote", "add", "origin", "https://example.invalid/repo.git")
	gitIn(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	return dir
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// captureStream is the minimal server-stream stand-in: Complete only
// calls Send and Context.
type captureStream struct {
	pluginapi.CompletionProvider_CompleteServer
	ctx     context.Context
	batches []*pluginapi.CompleteBatch
}

func (s *captureStream) Send(b *pluginapi.CompleteBatch) error {
	s.batches = append(s.batches, b)
	return nil
}
func (s *captureStream) Context() context.Context     { return s.ctx }
func (s *captureStream) SetHeader(metadata.MD) error  { return nil }
func (s *captureStream) SendHeader(metadata.MD) error { return nil }
func (s *captureStream) SetTrailer(metadata.MD)       {}

// complete drives the service the way the host does and returns the
// candidate values of the single final batch.
func complete(t *testing.T, c completion, cwd, line string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := &captureStream{ctx: ctx}
	err := c.Complete(&pluginapi.CompleteRequest{
		Line:   line,
		Cursor: uint32(len(line)),
		Cwd:    cwd,
	}, stream)
	if err != nil {
		t.Fatalf("Complete(%q): %v", line, err)
	}
	if len(stream.batches) != 1 || !stream.batches[0].GetFinal() {
		t.Fatalf("Complete(%q) sent %d batches, want one final", line, len(stream.batches))
	}
	var values []string
	for _, cand := range stream.batches[0].GetCandidates() {
		values = append(values, cand.GetValue())
	}
	return values
}

func newCompletion(t *testing.T) completion {
	t.Helper()
	cache := newRepoCache()
	t.Cleanup(cache.close)
	return completion{cache: cache}
}

func TestCompleteBranches(t *testing.T) {
	t.Parallel()
	dir := completeRepo(t)
	c := newCompletion(t)

	got := complete(t, c, dir, "git checkout ")
	for _, want := range []string{"trunk", "feature/login", "origin/main"} {
		if !slices.Contains(got, want) {
			t.Errorf("git checkout completion %v is missing %q", got, want)
		}
	}

	if got := complete(t, c, dir, "git checkout fe"); !slices.Equal(got, []string{"feature/login"}) {
		t.Errorf("prefix-filtered completion = %v, want just feature/login", got)
	}
}

// Loose refs disappear once packed; packed-refs must be read too, or
// completion goes blank on exactly the long-lived repos that gc.
func TestCompleteBranchesFromPackedRefs(t *testing.T) {
	t.Parallel()
	dir := completeRepo(t)
	gitIn(t, dir, "pack-refs", "--all")

	got := complete(t, newCompletion(t), dir, "git switch ")
	for _, want := range []string{"trunk", "feature/login"} {
		if !slices.Contains(got, want) {
			t.Errorf("packed-refs completion %v is missing %q", got, want)
		}
	}
}

// A linked worktree's .git is a file pointing at a private gitdir whose
// commondir names the shared one; refs must resolve through both hops.
func TestCompleteBranchesFromLinkedWorktree(t *testing.T) {
	t.Parallel()
	dir := completeRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(t, dir, "worktree", "add", wt, "feature/login")

	got := complete(t, newCompletion(t), wt, "git checkout ")
	if !slices.Contains(got, "trunk") {
		t.Errorf("worktree completion %v is missing trunk", got)
	}
}

func TestCompleteRemotesThenBranches(t *testing.T) {
	t.Parallel()
	dir := completeRepo(t)
	c := newCompletion(t)

	if got := complete(t, c, dir, "git push "); !slices.Equal(got, []string{"origin"}) {
		t.Errorf("remote-position completion = %v, want [origin]", got)
	}
	// After the remote, the operand is a local branch.
	got := complete(t, c, dir, "git push origin ")
	if !slices.Contains(got, "trunk") || slices.Contains(got, "origin/main") {
		t.Errorf("branch-position completion = %v, want local branches only", got)
	}
}

func TestCompleteChangedFiles(t *testing.T) {
	t.Parallel()
	dir := completeRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "new.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newCompletion(t)

	got := complete(t, c, dir, "git add ")
	for _, want := range []string{"a.txt", filepath.Join("sub", "new.txt")} {
		if !slices.Contains(got, want) {
			t.Errorf("git add completion %v is missing %q", got, want)
		}
	}

	// From the subdirectory the same files complete relative to *it* —
	// the value is inserted into the buffer where the user stands.
	got = complete(t, c, sub, "git add ")
	for _, want := range []string{filepath.Join("..", "a.txt"), "new.txt"} {
		if !slices.Contains(got, want) {
			t.Errorf("git add completion from sub %v is missing %q", got, want)
		}
	}
}

// A staged rename's porcelain record carries the origin path as a
// second NUL field; offering it would complete a file that no longer
// exists under that name.
func TestCompleteChangedFilesRename(t *testing.T) {
	t.Parallel()
	dir := completeRepo(t)
	gitIn(t, dir, "mv", "a.txt", "b.txt")

	// Exactly the new name: a containment check passed vacuously here,
	// because a mis-skipped origin field is parsed as a *garbage* record
	// ("xt"), not as "a.txt".
	got := complete(t, newCompletion(t), dir, "git add ")
	if !slices.Equal(got, []string{"b.txt"}) {
		t.Errorf("rename completion = %v, want exactly [b.txt]", got)
	}
}

// Everything out of scope answers an empty final batch, never a guess:
// other commands, the subcommand position (carapace's job), global
// flags before the subcommand, and directories with no repo.
func TestCompleteOutOfScope(t *testing.T) {
	t.Parallel()
	dir := completeRepo(t)
	c := newCompletion(t)

	for name, line := range map[string]string{
		"not git":                   "ls ",
		"subcommand position":       "git ",
		"unknown subcommand":        "git frobnicate ",
		"global flag before subcmd": "git -C /elsewhere checkout ",
		"empty line":                "",
	} {
		if got := complete(t, c, dir, line); len(got) != 0 {
			t.Errorf("%s: completion for %q = %v, want none", name, line, got)
		}
	}

	if got := complete(t, c, t.TempDir(), "git checkout "); len(got) != 0 {
		t.Errorf("non-repo completion = %v, want none", got)
	}
}

// The plugin claims exactly what it serves — prompt segment and
// completion — derived from the same struct that registers them.
func TestDescribeClaimsBothCapabilities(t *testing.T) {
	t.Parallel()
	p := newPlugin()
	resp, err := p.Info.Describe(context.Background(), &pluginapi.DescribeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := []pluginapi.Capability{
		pluginapi.Capability_CAPABILITY_COMPLETION,
		pluginapi.Capability_CAPABILITY_PROMPT_SEGMENT,
	}
	if !slices.Equal(resp.GetCapabilities(), want) {
		t.Errorf("capabilities = %v, want %v", resp.GetCapabilities(), want)
	}
}
