package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	pluginapi "github.com/blairham/koi-shell/pkg/pluginapi/v1"
)

// The completion half of the flagship (#476): branches, remotes and
// changed files, served as a second service on the same connection as
// the prompt segment — the multi-capability shape koi-aws proved out.
//
// The reason this exists next to koi-carapace: carapace answers by
// shelling out to the user's carapace binary, which shells out to git.
// Refs don't need any of that — they are files, and this plugin is
// already resident in the repo with the root cached for the prompt
// segment. So branch and remote candidates are read natively (loose
// refs, packed-refs, .git/config) with no subprocess on the Tab path;
// only changed-file completion runs git, because worktree state has no
// stable on-disk shape, and that call is bounded by the host's
// completion budget like everything else.
//
// Scope is deliberate: candidates for the *arguments* of subcommands
// whose arguments are refs, remotes or changed files. Subcommand and
// flag completion stays with carapace, which has the breadth; a line
// this plugin doesn't understand gets an empty final batch, never a
// guess.

// refSubcommands complete branch names (local and remote-tracking).
var refSubcommands = map[string]bool{
	"checkout": true, "switch": true, "merge": true, "rebase": true,
	"branch": true, "log": true, "diff": true, "reset": true,
	"cherry-pick": true, "revert": true,
}

// remoteSubcommands complete a remote name first, then branches —
// `git push <remote> <branch>`.
var remoteSubcommands = map[string]bool{
	"push": true, "pull": true, "fetch": true,
}

// fileSubcommands complete changed (modified or untracked) paths.
// `git rm` is deliberately absent: it operates on tracked files whether
// or not they changed, and it *fails* on untracked ones — so a changed-
// files candidate list would offer arguments that error. The shell
// core's file completion covers it correctly.
var fileSubcommands = map[string]bool{
	"add": true, "restore": true,
}

type completion struct {
	pluginapi.UnimplementedCompletionProviderServer
	cache *repoCache
}

func (c completion) Complete(req *pluginapi.CompleteRequest, stream pluginapi.CompletionProvider_CompleteServer) error {
	values := c.candidates(stream.Context(), req)

	limit := int(req.GetMaxCandidates())
	if limit == 0 || limit > len(values) {
		limit = len(values)
	}
	batch := &pluginapi.CompleteBatch{Final: true, Candidates: values[:limit]}
	return stream.Send(batch)
}

func (c completion) candidates(ctx context.Context, req *pluginapi.CompleteRequest) []*pluginapi.Candidate {
	words := splitLine(req.GetLine(), int(req.GetCursor()))
	// Needs at least "git <sub> <cur>": the command name is the shell
	// core's, the subcommand is carapace's.
	if len(words) < 3 || words[0] != "git" {
		return nil
	}
	// A global flag before the subcommand (`git -C dir checkout`) would
	// make "first non-flag word" pick the flag's *value* as the
	// subcommand. Refusing beats guessing.
	if strings.HasPrefix(words[1], "-") {
		return nil
	}
	sub := words[1]
	cur := words[len(words)-1]

	rs := c.cache.lookup(ctx, req.GetCwd())
	if rs == nil {
		return nil
	}

	switch {
	case remoteSubcommands[sub]:
		// Count the operands already typed between the subcommand and
		// the word under the cursor: none yet means the remote position.
		operands := 0
		for _, w := range words[2 : len(words)-1] {
			if !strings.HasPrefix(w, "-") {
				operands++
			}
		}
		if operands == 0 {
			return rank(remotes(refsRoot(rs.root)), cur, 2)
		}
		// `git push origin <branch>` names a local branch.
		local, _ := listRefs(refsRoot(rs.root))
		return rank(local, cur, 2)
	case refSubcommands[sub]:
		local, remote := listRefs(refsRoot(rs.root))
		return append(rank(local, cur, 2), rank(remote, cur, 1)...)
	case fileSubcommands[sub]:
		return rank(changedFiles(ctx, rs.root, req.GetCwd()), cur, 2)
	}
	return nil
}

// rank filters names by prefix and wraps them as candidates. The host
// merges providers by score, so local branches (2) sort ahead of
// remote-tracking refs (1).
func rank(names []string, prefix string, score uint32) []*pluginapi.Candidate {
	var out []*pluginapi.Candidate
	for _, n := range names {
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		out = append(out, &pluginapi.Candidate{Value: n, Score: score})
	}
	return out
}

// refsRoot resolves where the repository's refs actually live. Two
// indirections stand between a worktree and its refs, both plain text:
// a linked worktree's .git is a file naming its private gitdir, and
// that gitdir's `commondir` file names the shared .git where refs and
// config are kept.
func refsRoot(root string) string {
	gitDir := filepath.Join(root, ".git")
	if fi, err := os.Stat(gitDir); err != nil {
		return ""
	} else if !fi.IsDir() {
		data, err := os.ReadFile(gitDir)
		if err != nil {
			return ""
		}
		p := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
		if p == "" {
			return ""
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		gitDir = p
	}
	if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		p := strings.TrimSpace(string(data))
		if !filepath.IsAbs(p) {
			p = filepath.Join(gitDir, p)
		}
		return p
	}
	return gitDir
}

// listRefs reads branch names from the loose ref directories and
// packed-refs. Loose and packed can both name the same branch (a ref
// is packed and later moves); the set dedups them.
func listRefs(gitDir string) (local, remote []string) {
	if gitDir == "" {
		return nil, nil
	}
	localSet := map[string]bool{}
	remoteSet := map[string]bool{}
	looseRefs(filepath.Join(gitDir, "refs", "heads"), "", localSet)
	looseRefs(filepath.Join(gitDir, "refs", "remotes"), "", remoteSet)
	packedRefs(filepath.Join(gitDir, "packed-refs"), localSet, remoteSet)
	delete(remoteSet, "") // defensive: never offer an empty candidate
	return sorted(localSet), sorted(remoteSet)
}

func looseRefs(dir, prefix string, into map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		full := prefix + name
		if e.IsDir() {
			looseRefs(filepath.Join(dir, name), full+"/", into)
			continue
		}
		// remotes/<name>/HEAD is a pointer, not a branch.
		if name == "HEAD" {
			continue
		}
		into[full] = true
	}
}

// packedRefs parses the packed-refs file: `<sha> <refname>` lines, with
// `#` header lines and `^` peel lines skipped.
func packedRefs(path string, local, remote map[string]bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == '^' {
			continue
		}
		_, ref, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			local[name] = true
		} else if name, ok := strings.CutPrefix(ref, "refs/remotes/"); ok && !strings.HasSuffix(name, "/HEAD") {
			remote[name] = true
		}
	}
}

// remotes parses `[remote "name"]` sections out of the repository
// config. A full INI parser is not needed to read a section header.
func remotes(gitDir string) []string {
	if gitDir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, `[remote "`)
		if !ok {
			continue
		}
		name, _, ok := strings.Cut(rest, `"`)
		if ok && name != "" {
			set[name] = true
		}
	}
	return sorted(set)
}

// changedFiles lists modified and untracked paths, relative to the
// cwd the user is completing from. The one subprocess in this file:
// worktree state has no stable on-disk shape to read, and the call is
// bounded by the host's completion budget via ctx.
//
// -z because file names are exactly the strings that contain newlines
// and quotes; porcelain v1's unquoted-vs-quoted split is a parser trap.
func changedFiles(ctx context.Context, root, cwd string) []string {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain", "-z")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	// root came from git and is symlink-resolved (on macOS /tmp and /var
	// answer as /private/...), while cwd is whatever the shell was given.
	// Rel across the two namespaces produces a ../../../private/... path
	// — the same trap koi-direnv's rcDir documents. Resolving the cwd
	// puts both sides in git's namespace, and a relative path is equally
	// valid from the unresolved directory it aliases.
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	set := map[string]bool{}
	records := strings.Split(string(out), "\x00")
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if len(rec) < 4 {
			continue
		}
		status, path := rec[:2], rec[3:]
		// A rename/copy record is followed by the *origin* path as its
		// own NUL-separated field; the origin is not a completion
		// candidate (it no longer exists under that name).
		if status[0] == 'R' || status[0] == 'C' {
			i++
		}
		rel, err := filepath.Rel(cwd, filepath.Join(root, path))
		if err != nil {
			continue
		}
		set[rel] = true
	}
	return sorted(set)
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// splitLine turns the buffer up to the cursor into words, preserving a
// trailing empty word when the cursor follows a space — that is how
// "git checkout <cursor>" asks for all refs. Same shape as
// koi-carapace's splitter; fifteen lines is cheaper than a shared
// package neither plugin would own.
func splitLine(line string, cursor int) []string {
	runes := []rune(line)
	if cursor > len(runes) {
		cursor = len(runes)
	}
	head := string(runes[:cursor])
	words := strings.Fields(head)
	if head == "" {
		return nil
	}
	if strings.HasSuffix(head, " ") || strings.HasSuffix(head, "\t") {
		words = append(words, "")
	}
	return words
}
