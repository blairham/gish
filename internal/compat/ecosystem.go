//go:build unix

package compat

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/creack/pty"
)

// ecosystemTimeout bounds one tool. Generous because some of these
// shell out on every prompt, and a slow init is a different complaint
// from a broken one.
const ecosystemTimeout = 30 * time.Second

// commandDone is the OSC 133;D mark: "the command finished", which is
// how the driver knows one line is over without guessing from output.
const commandDone = "\x1b]133;D;"

var (
	errShellExited = errors.New("the shell exited before the assertion could be made")
	errTimeout     = errors.New("timed out waiting for the tool's effect")
)

// The ecosystem matrix (#159).
//
// The deliverable of the hook work is a matrix, not an assertion. Each
// tool here is installed unmodified, its own documented init line is
// sourced, and its actual *behavior* is asserted in a live interactive
// shell — the prompt renders, the widget binds, the environment
// changes. "We implement PROMPT_COMMAND" is a claim; "starship's prompt
// renders in gish" is the thing anyone actually cares about.
//
// The launch claim this exists to support is "your entire existing
// shell stack, unchanged". A matrix that only ran the tools installed
// on a developer laptop would not support it, so absence is reported
// per tool rather than quietly skipped.

// EcosystemCase is one tool: how to load it, and what proves it worked.
type EcosystemCase struct {
	Name       string
	Binary     string // the tool's own executable; absence means "not installed"
	Provenance string
	// Init is the rc content, with $TOOLDIR available as a scratch path.
	Init string
	// Setup runs before the shell starts (fixtures, `direnv allow`).
	Setup func(dir string) error
	// Type is what gets typed at the prompt, one command per line;
	// empty means the assertion is about the prompt itself.
	//
	// Several lines matter for the hook-driven tools: a PROMPT_COMMAND
	// hook applies before the *next* prompt, so `cd project; echo $VAR`
	// on one line does not see the change — in bash either.
	Type string
	// Want must appear in what comes back.
	Want string
	// What the case actually proves, for the published table.
	Asserts string
}

// EcosystemCorpus is the published matrix.
var EcosystemCorpus = []EcosystemCase{
	{
		Name:       "starship",
		Binary:     "starship",
		Provenance: "ships 11 per-shell init scripts; the bash one is what gish has to run",
		Init: `export STARSHIP_CONFIG="$TOOLDIR/starship.toml"
eval "$(starship init bash)"`,
		Setup: func(dir string) error {
			// A format with a fixed marker, so the assertion is about
			// starship rendering our prompt rather than about whatever
			// the developer's own config happens to say.
			return os.WriteFile(filepath.Join(dir, "starship.toml"),
				[]byte("format = \"STARSHIP-RENDERED$character\"\n[character]\nsuccess_symbol = \"%\"\nerror_symbol = \"%\"\n"), 0o600)
		},
		Want:    "STARSHIP-RENDERED",
		Asserts: "the prompt renders through starship's own bash init",
	},
	{
		Name:       "oh-my-posh",
		Binary:     "oh-my-posh",
		Provenance: "the other cross-shell prompt; ~90 themes, and the default for most arrivals from PowerShell",
		// Its bash init is a two-stage bootstrap: the eval sets
		// POSH_SESSION_ID and sources a *cached* script, so what
		// actually has to run is that second file — PS0, a
		// PROMPT_COMMAND array, `shopt -u promptvars` around the
		// render, and PS1 holding a command substitution that the
		// shell is expected to expand at prompt time.
		Init: `eval "$(oh-my-posh init bash --config "$TOOLDIR/omp.json")"`,
		Setup: func(dir string) error {
			// A single text segment with a fixed marker, so this
			// asserts oh-my-posh rendering our config rather than
			// whatever theme the developer happens to have.
			return os.WriteFile(filepath.Join(dir, "omp.json"), []byte(`{
  "version": 3,
  "final_space": true,
  "blocks": [
    {
      "type": "prompt",
      "alignment": "left",
      "segments": [
        { "type": "text", "style": "plain", "template": "OMP-RENDERED" }
      ]
    }
  ]
}
`), 0o600)
		},
		Want:    "OMP-RENDERED",
		Asserts: "the prompt renders through oh-my-posh's own bash init",
	},
	{
		Name:       "zoxide",
		Binary:     "zoxide",
		Provenance: "ships 9 init scripts; its bash init is a PROMPT_COMMAND hook plus a `z` function",
		Init:       `eval "$(zoxide init bash)"`,
		Type:       "zoxide add /tmp; z tmp; pwd",
		Want:       "/tmp",
		Asserts:    "`z` jumps — the user's own zoxide, not gish's native jumper",
	},
	{
		Name:       "atuin",
		Binary:     "atuin",
		Provenance: "ships 7 init scripts; takes over Ctrl-R through bind",
		Init:       `eval "$(atuin init bash --disable-up-arrow)"`,
		Type:       "bind -X",
		Want:       `\C-r`,
		Asserts:    "atuin's Ctrl-R binding is installed",
	},
	{
		Name:       "direnv",
		Binary:     "direnv",
		Provenance: "`direnv hook bash` is in a great many rc files",
		Init:       `eval "$(direnv hook bash)"`,
		Setup: func(dir string) error {
			project := filepath.Join(dir, "project")
			if err := os.MkdirAll(project, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(project, ".envrc"), []byte("export DIRENV_PROOF=applied\n"), 0o600); err != nil {
				return err
			}
			cmd := exec.Command("direnv", "allow", project)
			cmd.Dir = project
			cmd.Env = append(os.Environ(), "DIRENV_CONFIG="+dir, "XDG_DATA_HOME="+filepath.Join(dir, "data"))
			return cmd.Run()
		},
		Type:    "cd project\necho \"proof=$DIRENV_PROOF\"",
		Want:    "proof=applied",
		Asserts: "the .envrc applies on cd, through direnv's own hook",
	},
	{
		Name:       "mise",
		Binary:     "mise",
		Provenance: "out-installs asdf 8:1; its activate line is a PROMPT_COMMAND hook",
		Init:       `eval "$(mise activate bash)"`,
		Type:       "mise --version >/dev/null && echo mise-active-$?",
		Want:       "mise-active-0",
		Asserts:    "mise activates and stays usable",
	},
	{
		Name:       "fzf",
		Binary:     "fzf",
		Provenance: "its key bindings are `bind -x`; the most-installed of the set",
		// Two ways in, because distributions are two years behind
		// upstream and both are what real rc files contain. `fzf --bash`
		// arrived in 0.48; Ubuntu 24.04 ships 0.44.1, whose integration
		// is the packaged key-bindings file — and CI found that by
		// answering "unknown option: --bash". A matrix that only tests
		// the newest release is not testing what users run.
		Init: `if fzf --bash >/dev/null 2>&1; then
  eval "$(fzf --bash)"
else
  for f in /usr/share/doc/fzf/examples/key-bindings.bash \
           /usr/share/fzf/key-bindings.bash \
           /opt/homebrew/opt/fzf/shell/key-bindings.bash \
           /usr/local/opt/fzf/shell/key-bindings.bash; do
    [ -f "$f" ] && . "$f" && break
  done
fi`,
		Type:    "bind -X",
		Want:    `\C-t`,
		Asserts: "fzf's Ctrl-T widget is bound, from --bash or the packaged key-bindings file",
	},
}

// EcosystemResult is one tool's verdict.
type EcosystemResult struct {
	EcosystemCase
	Present bool
	Pass    bool
	Reason  string
	Output  string
}

// RunEcosystem loads one tool in a live interactive gish and asserts
// what it does.
func RunEcosystem(ctx context.Context, gishBin string, c EcosystemCase) EcosystemResult {
	r := EcosystemResult{EcosystemCase: c}
	if _, err := exec.LookPath(c.Binary); err != nil {
		return r
	}
	r.Present = true

	dir, err := os.MkdirTemp("", "gish-eco-*")
	if err != nil {
		r.Reason = err.Error()
		return r
	}
	defer os.RemoveAll(dir)

	if c.Setup != nil {
		if err := c.Setup(dir); err != nil {
			r.Reason = "setup: " + err.Error()
			return r
		}
	}
	rc := strings.ReplaceAll(c.Init, "$TOOLDIR", dir)
	rcPath := filepath.Join(dir, "gishrc")
	if err := os.WriteFile(rcPath, []byte(rc+"\n"), 0o600); err != nil {
		r.Reason = err.Error()
		return r
	}

	out, err := runInGish(ctx, gishBin, dir, rcPath, c.Type, c.Want)
	r.Output = out
	if err != nil {
		r.Reason = err.Error()
		return r
	}
	r.Pass = strings.Contains(out, c.Want)
	if !r.Pass {
		r.Reason = "expected " + c.Want + " in the session's output"
	}
	return r
}

// RunEcosystemAll runs the whole matrix.
func RunEcosystemAll(ctx context.Context, gishBin string) []EcosystemResult {
	out := make([]EcosystemResult, 0, len(EcosystemCorpus))
	for _, c := range EcosystemCorpus {
		out = append(out, RunEcosystem(ctx, gishBin, c))
	}
	return out
}

// runInGish starts an interactive gish with rcPath as its rc, types
// line (when there is one), and returns the visible text once want
// appears.
//
// Waiting for the *marker* rather than for a prompt is what makes the
// matrix honest: a tool that installs a hook which never fires would
// still reach a prompt, and a test that waited for one would pass.
func runInGish(ctx context.Context, gishBin, dir, rcPath, line, want string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ecosystemTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, gishBin)
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=" + dir,
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "config"),
		"XDG_DATA_HOME=" + filepath.Join(dir, "data"),
		"XDG_STATE_HOME=" + filepath.Join(dir, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(dir, "cache"),
		"TERM=xterm-256color",
		"PATH=" + pathEnv(),
		"GISH_RC=" + rcPath,
		// The tools under test own the prompt; gish's own themes and
		// its native jumper must not be what the assertion sees.
		"GISH_JUMP=off",
	}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 200})
	if err != nil {
		return "", err
	}
	defer func() {
		f.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	chunks := make(chan []byte, 64)
	go func() {
		defer close(chunks)
		for {
			buf := make([]byte, 8192)
			n, rerr := f.Read(buf)
			if n > 0 {
				chunks <- buf[:n]
			}
			if rerr != nil {
				return
			}
		}
	}()

	var seen strings.Builder
	// The wait takes a predicate over everything seen so far, which is
	// what lets one helper serve both "the mark has appeared" and "it
	// has appeared three times".
	wait := func(done func(string) bool) error {
		for !done(seen.String()) {
			select {
			case chunk, ok := <-chunks:
				if !ok {
					return errShellExited
				}
				seen.Write(chunk)
			case <-ctx.Done():
				return errTimeout
			}
		}
		return nil
	}
	// Two views of the same bytes: the prompt mark is an escape sequence
	// and exists only in the raw stream, while a tool's output is only
	// legible with the escapes stripped. Matching everything against the
	// stripped view never matched the mark, so the harness typed into a
	// shell that had not reached raw mode yet and every case timed out
	// with the shell looking innocent.
	waitFor := func(want string) error {
		return wait(func(s string) bool {
			return strings.Contains(s, want) || strings.Contains(plainText(s), want)
		})
	}

	if err := waitFor(markPromptEnd); err != nil {
		return plainText(seen.String()), err
	}
	// Commands are sent one at a time, each waited out before the next:
	// the hooks under test run *between* commands, and typing ahead
	// would measure the shell's type-ahead buffer instead of the tool.
	//
	// The waiting counts "command done" marks rather than clearing what
	// has been seen, because the last command's output is the answer —
	// clearing after it threw away the very thing being asserted.
	done := 0
	for _, cmd := range strings.Split(line, "\n") {
		if cmd == "" {
			continue
		}
		if _, err := f.WriteString(cmd + "\r"); err != nil {
			return plainText(seen.String()), err
		}
		done++
		if err := wait(func(seen string) bool {
			return strings.Count(seen, commandDone) >= done
		}); err != nil {
			return plainText(seen.String()), err
		}
	}

	err = waitFor(want)
	return plainText(seen.String()), err
}
