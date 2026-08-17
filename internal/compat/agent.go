package compat

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The agent gate (#208).
//
// The 2026 churn conversation moved past "the command I pasted from
// StackOverflow fails" to "my coding agent's subshell fails". Coding
// agents spawn a shell regardless of which one the human chose, and the
// failure mode is silent: with fish as the login shell, functions,
// abbreviations and PATH edits simply are not there inside the agent's
// subshell, and some agents emit a syntax-error preamble on every
// command. The wild's recommended workaround is the dual-shell split —
// fish for humans, zsh for agents — which is exactly the partition gish
// exists to collapse.
//
// So the claim is two-sided: an agent pointed at gish gets the user's
// real environment with no syntax errors, and `gish --sandbox` confines
// what that agent can touch. The first half is what this gate holds us
// to, because #11475-class bugs mean even SHELL=gish is not always
// respected and an untested claim about someone else's tool ages badly.
//
// These are the invocation *shapes* harnesses actually write, captured
// from real ones (see #215 and #217), run differentially: the same argv
// through real bash and through gish, same rc, same profile. bash is the
// oracle. The two deliberate divergences carry Expect instead, because
// pretending gish is bash where it has decided not to be would be the
// dishonest kind of green.

// AgentCase is one harness-shaped invocation.
type AgentCase struct {
	Name string
	// Provenance says which harness does this and where it was seen, so
	// a case can be re-verified rather than trusted.
	Provenance string
	// Argv is everything after the shell binary. $HOME is a scratch
	// directory carrying RC and Profile below.
	Argv []string
	// RC and Profile are written into the scratch home when non-empty.
	RC, Profile string
	// Expect, when set, is what gish must print — for the places gish
	// deliberately differs from bash and the difference is the feature.
	Expect string
	// Why explains an Expect divergence in the published table.
	Why string
}

// AgentCorpus is the published gate.
var AgentCorpus = []AgentCase{
	{
		Name:       "clustered login+command (-lc)",
		Provenance: "how tools spawn a login shell for one command; gish answered \"flag provided but not defined: -lc\" until #217",
		Profile:    "export FROM_PROFILE=yes\n",
		Argv:       []string{"-lc", `echo "profile=${FROM_PROFILE:-MISSING}"`},
	},
	{
		Name:       "options after the command operand (-c -l)",
		Provenance: "captured verbatim from Claude Code: `<shell> -c -l '<generator>'`. The command string is an operand, so -l is a login flag, not -c's value",
		Profile:    "export FROM_PROFILE=yes\n",
		Argv:       []string{"-c", "-l", `echo "profile=${FROM_PROFILE:-MISSING}"`},
	},
	{
		Name:       "snapshot generator: clobbering redirect",
		Provenance: "the first line of Claude Code's shell-snapshot generator is `echo \"# Snapshot file\" >| \"$SNAPSHOT_FILE\"`; it failed silently (exit 1, no message, no file) so the harness lost the snapshot entirely",
		Argv:       []string{"-c", `f="$HOME/snap"; echo "# Snapshot file" >| "$f"; cat "$f"; echo "rc=$?"`},
	},
	{
		Name:       "snapshot generator: declare -F",
		Provenance: "the generator enumerates the user's functions with `declare -F` and pipes it through `cut -d' ' -f3`; refused outright, so no user function survived into any command",
		RC:         "greet() { echo hello; }\ndeploy() { echo deploying; }\n",
		Argv:       []string{"-ic", `declare -F | cut -d' ' -f3 | sort | tr '\n' ' '`},
	},
	{
		Name:       "snapshot generator: shopt -p",
		Provenance: "the generator records shell options the same way; refused, so no option survived either",
		Argv:       []string{"-c", `shopt -p >/dev/null && echo shopt-readable`},
	},
	{
		Name:       "functions reach an agent subshell",
		Provenance: "the fish failure this exists to avoid: functions defined in the user's config are simply absent inside the agent's subshell",
		RC:         "deploy() { echo real-deploy; }\n",
		Argv:       []string{"-ic", "deploy"},
	},
	{
		Name:       "aliases reach an agent subshell",
		Provenance: "same shape as functions; -i is what pulls the rc in for a one-shot command, which is why it is implemented rather than accepted-and-ignored (#217)",
		RC:         "alias ll='echo aliased-ll'\n",
		Argv:       []string{"-ic", "ll"},
	},
	{
		Name:       "PATH edits reach an agent subshell",
		Provenance: "anthropics/claude-code#19983 — installers emitting bash-style PATH exports at users whose shell never reads them",
		RC:         "export PATH=\"$HOME/tools:$PATH\"\n",
		Argv:       []string{"-ic", `case ":$PATH:" in *":$HOME/tools:"*) echo tools-on-path ;; *) echo MISSING ;; esac`},
	},
	{
		Name:       "exported variables survive the hop",
		Provenance: "an agent that re-execs the shell per command must not lose the environment between hops",
		Argv:       []string{"-c", `FOO=bar; export FOO; echo "$FOO"; env | grep -c '^FOO=bar$'`},
	},
	{
		Name:       "no preamble on a clean run",
		Provenance: "with fish as the login shell some agents get a syntax-error preamble on every command; a harness parsing output cannot tell that from real output",
		Argv:       []string{"-lc", "echo only-this"},
	},
	{
		Name:       "$0 says gish, not bash",
		Provenance: "shell identity is decided (#120): gish claims bash's interface, not bash's identity",
		Argv:       []string{"-c", "echo $0"},
		Expect:     "gish\n",
		Why:        "bash prints its own path; gish is not pretending to be bash, and a harness that needs the difference can see it",
	},
	{
		Name:       "BASH_VERSION answers feature probes",
		Provenance: "tools branch on BASH_VERSINFO to pick an implementation — fzf picks its Ctrl-T path on `BASH_VERSINFO[0] < 4`, and unset reads as 0 (#120)",
		Argv:       []string{"-c", `[ -n "$BASH_VERSION" ] && [ "${BASH_VERSINFO[0]}" -ge 4 ] && echo probe-answered`},
	},
}

// AgentResult is one case's verdict.
type AgentResult struct {
	AgentCase
	BashOut, GishOut   string
	BashCode, GishCode int
	Pass               bool
	Reason             string
}

// RunAgentAll runs the whole gate.
func RunAgentAll(ctx context.Context, bashBin, gishBin string) []AgentResult {
	out := make([]AgentResult, 0, len(AgentCorpus))
	for _, c := range AgentCorpus {
		out = append(out, RunAgent(ctx, bashBin, gishBin, c))
	}
	return out
}

// RunAgent runs one case under both shells and compares — or, for a case
// carrying Expect, holds gish to the stated behavior instead.
func RunAgent(ctx context.Context, bashBin, gishBin string, c AgentCase) AgentResult {
	r := AgentResult{AgentCase: c}
	r.GishOut, r.GishCode = runAgentArgv(ctx, gishBin, c)
	if c.Expect != "" {
		r.Pass = r.GishOut == c.Expect
		if !r.Pass {
			r.Reason = "want " + firstLine(strings.TrimSpace(c.Expect))
		}
		return r
	}
	r.BashOut, r.BashCode = runAgentArgv(ctx, bashBin, c)
	r.BashOut = dropJobControlPreamble(r.BashOut)
	switch {
	case r.BashOut == r.GishOut && r.BashCode == r.GishCode:
		r.Pass = true
	case r.BashOut != r.GishOut && r.BashCode != r.GishCode:
		r.Reason = "output and exit status differ"
	case r.BashOut != r.GishOut:
		r.Reason = "output differs"
	default:
		r.Reason = "exit status differs"
	}
	return r
}

// runAgentArgv gives the shell its own scratch home so the rc and profile
// under test are the only ones it can find. The runner's real dotfiles
// must never decide whether this gate passes.
func runAgentArgv(ctx context.Context, shell string, c AgentCase) (string, int) {
	home, err := os.MkdirTemp("", "gish-agent-")
	if err != nil {
		return "[runner error: " + err.Error() + "]", -1
	}
	defer os.RemoveAll(home) //nolint:errcheck // scratch dir

	// bash and gish read different file names for the same two roles, so
	// each shell is given the pair it actually looks for. Writing only
	// gish's would test gish against a bash that was never configured.
	if c.RC != "" {
		for _, name := range []string{".bashrc", ".gishrc"} {
			if werr := os.WriteFile(filepath.Join(home, name), []byte(c.RC), 0o600); werr != nil {
				return "[runner error: " + werr.Error() + "]", -1
			}
		}
	}
	if c.Profile != "" {
		for _, name := range []string{".bash_profile", ".profile", ".gish_profile"} {
			if werr := os.WriteFile(filepath.Join(home, name), []byte(c.Profile), 0o600); werr != nil {
				return "[runner error: " + werr.Error() + "]", -1
			}
		}
	}
	if merr := os.MkdirAll(filepath.Join(home, "tools"), 0o755); merr != nil {
		return "[runner error: " + merr.Error() + "]", -1
	}

	rctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, shell, c.Argv...) //nolint:gosec // fixed shells, curated corpus
	cmd.Dir = home
	cmd.Env = []string{
		"PATH=" + pathEnv(), "HOME=" + home, "LC_ALL=C", "TERM=dumb",
		// Point every XDG root into the scratch home too: gish resolves its
		// rc through XDG first, and a developer's real config would
		// otherwise leak into the gate.
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"GISH_WELCOME=off",
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err = cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		return buf.String() + "\n[runner error: " + err.Error() + "]", -1
	}
	return buf.String(), code
}

// AgentCLIs are the harnesses this gate is about. Their presence is
// reported rather than required: CI has none of them installed and
// authenticated, and a gate that silently skips is a gate that reads as
// green while testing nothing.
var AgentCLIs = []struct{ Name, Bin, Note string }{
	{"Claude Code", "claude", "sources a generated shell snapshot before every command"},
	{"Codex CLI", "codex", "spawns $SHELL -lc"},
	{"Gemini CLI", "gemini", "spawns a shell per tool call"},
}

// DetectedAgents reports which agent CLIs are on this machine, so the
// published page can say what was actually exercised here versus what
// was only reasoned about.
func DetectedAgents() map[string]bool {
	found := map[string]bool{}
	for _, a := range AgentCLIs {
		if _, err := exec.LookPath(a.Bin); err == nil {
			found[a.Name] = true
		}
	}
	return found
}

// AgentFailures returns the failing results, corpus order.
func AgentFailures(results []AgentResult) []AgentResult {
	var out []AgentResult
	for _, r := range results {
		if !r.Pass {
			out = append(out, r)
		}
	}
	return out
}

// Diff renders a compact one-line difference for the published table.
func (r AgentResult) Diff() string {
	if r.Pass {
		return ""
	}
	if r.Expect != "" {
		return "gish: " + firstLine(strings.TrimSpace(r.GishOut)) + " · " + r.Reason
	}
	bash := strings.TrimSpace(r.BashOut)
	gish := strings.TrimSpace(r.GishOut)
	if bash == gish {
		return "exit " + itoa(r.BashCode) + " vs " + itoa(r.GishCode)
	}
	return "bash: " + firstLine(bash) + " · gish: " + firstLine(gish)
}

// dropJobControlPreamble removes the two lines bash prints when asked for
// an interactive shell without a controlling terminal:
//
//	bash: cannot set terminal process group (…): Inappropriate ioctl for device
//	bash: no job control in this shell
//
// That is an artifact of running the gate from a test process, not a
// compatibility difference — and normalizing it is the honest move only
// because the direction is in gish's favor: the preamble is bash's, gish
// prints nothing, and leaving it in would fail every `-i` case for the
// wrong reason while hiding any real difference underneath.
//
// It is deliberately narrow. Anything else bash says on stderr is kept,
// because "no preamble on a clean run" is itself one of the cases here.
func dropJobControlPreamble(out string) string {
	var kept []string
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "bash: cannot set terminal process group") ||
			line == "bash: no job control in this shell" {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
