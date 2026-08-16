package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The direnv CLI adapter.
//
// Everything here was verified against real direnv (2.37.1) rather than
// inferred.
//
// The allow state is checked *before* exporting, and the reason is that
// export alone cannot answer the question that matters. On a blocked
// .envrc, `direnv export json` exits 1 with a prose message on stderr —
// but it exits 1 the same way for a directory the user deliberately
// denied, and for an .envrc whose own code failed. Those three demand
// opposite responses: offer approval, stay silent, or report a broken
// .envrc. Only `status --json` distinguishes them, and it does so with
// a documented enum rather than a colored English sentence.
//
// (It also prints its bookkeeping JSON to stdout while failing, so a
// bridge keying on "did I get parseable JSON back" would read a blocked
// directory as an empty diff.)

const direnvBin = "direnv"

// allowState mirrors direnv's own enum, confirmed by driving each
// transition (allow → edit → deny) and reading `status --json`.
type allowState int

const (
	stateAllowed allowState = 0
	stateBlocked allowState = 1 // never allowed, or the content changed
	stateDenied  allowState = 2 // an explicit `direnv deny`
	stateNoRC    allowState = -1
)

// status is the shape of `direnv status --json` this bridge reads. The
// machine-readable status is used rather than parsing stderr: that
// message is colored, prose, and not a contract.
type status struct {
	State struct {
		FoundRC *struct {
			Allowed int    `json:"allowed"`
			Path    string `json:"path"`
		} `json:"foundRC"`
	} `json:"state"`
}

var errNoDirenv = errors.New("direnv is not installed")

type bridge struct {
	once      sync.Once
	path      string
	available bool
}

func (b *bridge) resolve() (string, bool) {
	b.once.Do(func() {
		p, err := exec.LookPath(direnvBin)
		b.path, b.available = p, err == nil
	})
	return b.path, b.available
}

// run invokes direnv in dir with a deliberately minimal environment.
//
// The env matters: the host sends this plugin an allowlisted subset
// (PATH, HOME, TERM, LANG, LC_ALL, USER) and deliberately not the
// session's DIRENV_* bookkeeping. So direnv is invoked *stateless* on
// every call and always computes a load-from-clean diff — which is
// exactly right here, because reverting is gish's job (#12 snapshots
// every variable it applies and restores it on leaving the subtree),
// not direnv's.
func (b *bridge) run(ctx context.Context, dir string, env map[string]string, args ...string) (stdout, stderr []byte, err error) {
	path, ok := b.resolve()
	if !ok {
		return nil, nil, errNoDirenv
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = envSlice(env)
	// Killing on deadline is not returning on deadline: Wait drains the
	// pipes, and an .envrc that spawns something long-lived keeps the
	// write end open. Without this the host's 100ms budget is advisory.
	cmd.WaitDelay = 250 * time.Millisecond

	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), errb.Bytes(), fmt.Errorf("direnv %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), errb.Bytes(), nil
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// state reports direnv's view of dir, and the .envrc it found.
func (b *bridge) state(ctx context.Context, dir string, env map[string]string) (allowState, string, error) {
	out, _, err := b.run(ctx, dir, env, "status", "--json")
	if err != nil {
		return stateNoRC, "", err
	}
	var s status
	if err := json.Unmarshal(out, &s); err != nil {
		return stateNoRC, "", fmt.Errorf("direnv status: %w", err)
	}
	if s.State.FoundRC == nil {
		return stateNoRC, "", nil
	}
	return allowState(s.State.FoundRC.Allowed), s.State.FoundRC.Path, nil
}

// export returns the variables the .envrc sets, with direnv's internal
// bookkeeping removed.
func (b *bridge) export(ctx context.Context, dir string, env map[string]string) (map[string]string, error) {
	out, _, err := b.run(ctx, dir, env, "export", "json")
	if err != nil {
		return nil, err
	}
	// An empty export is legitimate — an .envrc that only runs commands
	// sets nothing — and direnv prints nothing at all in that case.
	if len(bytes.TrimSpace(out)) == 0 {
		return map[string]string{}, nil
	}
	var raw map[string]string
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("direnv export: %w", err)
	}
	return stripBookkeeping(raw), nil
}

// stripBookkeeping removes DIRENV_*.
//
// These are how direnv tracks state across invocations when a shell
// hook carries them between calls. Here nothing carries them: the host
// does not send them back (they are not in its allowlist), so they
// would have no consumer. Proposing them would put four opaque,
// base64-blob variables into the user's environment, show them in the
// trust prompt where they are pure noise, and confuse any real direnv
// the user later runs in a subshell.
func stripBookkeeping(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if strings.HasPrefix(k, "DIRENV_") {
			continue
		}
		out[k] = v
	}
	return out
}

// allow runs `direnv allow` for dir — the second half of the one-gesture
// trust flow. gish's own `trust allow` calls this, so a user approves
// once and both systems record it.
//
// Both key on content, which is what makes one gesture safe: editing the
// .envrc re-blocks direnv *and* changes gish's diff hash, so the two
// re-prompt together rather than drifting apart. Verified rather than
// assumed (see #137).
func (b *bridge) allow(ctx context.Context, dir string, env map[string]string) error {
	_, _, err := b.run(ctx, dir, env, "allow", dir)
	return err
}

// rcDir is the directory the diff derives from: where the .envrc lives.
//
// The host requires for_dir to be cwd or an ancestor, and keys trust on
// it — so a diff approved in a project root stays applied all the way
// down the tree, which is exactly direnv's own semantics. Falling back
// to cwd when the path is unreadable keeps the response valid rather
// than discarded.
func rcDir(rcPath, cwd string) string {
	if rcPath == "" {
		return cwd
	}
	dir := filepath.Dir(rcPath)
	if dir == "" || dir == "." || dir == cwd {
		return cwd
	}
	// direnv reports a symlink-resolved path — on macOS everything under
	// /tmp and /var comes back as /private/... — while the shell's cwd is
	// the unresolved one it was given. The host requires for_dir to be
	// cwd or an ancestor *as strings*, so handing back direnv's namespace
	// would get every proposal silently discarded on macOS. Map the
	// answer into the caller's namespace by walking the same distance up
	// from the cwd we were handed.
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return dir
	}
	rel, err := filepath.Rel(dir, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return dir // not an ancestor after all; let the host judge it
	}
	out := cwd
	if rel != "." {
		for range strings.Split(rel, string(filepath.Separator)) {
			out = filepath.Dir(out)
		}
	}
	return out
}
