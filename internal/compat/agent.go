package compat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
// fish for humans, zsh for agents — which is exactly the partition koi
// exists to collapse.
//
// So the claim is two-sided: an agent pointed at koi gets the user's
// real environment with no syntax errors, and `koi --sandbox` confines
// what that agent can touch. The first half is what this gate holds us
// to, because #11475-class bugs mean even SHELL=koi is not always
// respected and an untested claim about someone else's tool ages badly.
//
// These are the invocation *shapes* harnesses actually write, captured
// from real ones (see #215 and #217), run differentially: the same argv
// through real bash and through koi, same rc, same profile. bash is the
// oracle. The two deliberate divergences carry Expect instead, because
// pretending koi is bash where it has decided not to be would be the
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
	// Expect, when set, is what koi must print — for the places koi
	// deliberately differs from bash and the difference is the feature.
	Expect string
	// Why explains an Expect divergence in the published table.
	Why string
	// MinBashMajor, when set, is the oldest bash that can serve as this
	// case's oracle. A probe *about* a bash version cannot be answered by
	// a bash that predates it: macOS still ships 3.2.57, where a
	// `BASH_VERSINFO[0] -ge 4` probe is correctly false, so comparing koi
	// against it measures the runner's bash rather than koi. Skipped, not
	// tolerated — the case still runs everywhere bash is new enough.
	MinBashMajor int
	// Known is the open issue number for a gap koi has not closed yet.
	//
	// The gate is a gate: a case without this must pass. But a gap that is
	// filed, reproduced and waiting on a fix is a different thing from a
	// regression, and the difference belongs in the corpus rather than in
	// whoever happens to remember it. A Known case that fails is reported,
	// not failed — and a Known case that *passes* fails the build, because
	// the marker has become a lie and the published page is now
	// understating koi.
	//
	// So a fix cannot land without deleting the marker in the same change,
	// and a gap cannot be tolerated without an issue number on it.
	Known int
	// KnownNote is what breaks when this case fails, in one line, for the
	// published table. Required whenever Known is set: "koi does not
	// support X" restates the case name rather than saying what it costs.
	KnownNote string
}

// AgentCorpus is the published gate.
var AgentCorpus = []AgentCase{
	{
		Name:       "clustered login+command (-lc)",
		Provenance: "how tools spawn a login shell for one command; koi answered \"flag provided but not defined: -lc\" until #217",
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
		// Keeps the harness's exact shape (`declare -F | cut -d' ' -f3`) but
		// asks only about the functions this case defined. A system-wide rc is
		// outside the scratch $HOME and cannot be isolated: Debian and Ubuntu
		// ship /etc/bash.bashrc defining command_not_found_handle, which bash
		// lists and koi does not, so an unfiltered listing compares the
		// runner's OS rather than the claim — that the user's own functions
		// reach an agent's subshell.
		Argv: []string{"-ic", `declare -F | cut -d' ' -f3 | grep -E '^(deploy|greet)$' | sort | tr '\n' ' '`},
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
		Name:       "$0 says koi, not bash",
		Provenance: "shell identity is decided (#120): koi claims bash's interface, not bash's identity",
		Argv:       []string{"-c", "echo $0"},
		Expect:     "koi\n",
		Why:        "bash prints its own path; koi is not pretending to be bash, and a harness that needs the difference can see it",
	},
	{
		Name:         "BASH_VERSION answers feature probes",
		Provenance:   "tools branch on BASH_VERSINFO to pick an implementation — fzf picks its Ctrl-T path on `BASH_VERSINFO[0] < 4`, and unset reads as 0 (#120)",
		Argv:         []string{"-c", `[ -n "$BASH_VERSION" ] && [ "${BASH_VERSINFO[0]}" -ge 4 ] && echo probe-answered`},
		MinBashMajor: 4,
	},

	// The cases below came from pointing a real Claude Code session at koi
	// for a working day and then running a bash-vs-koi differential sweep
	// over the constructs it used (128 snippets, each run direct and
	// through `eval`). Every one is filed, and every one is written to
	// print a deterministic marker on both shells rather than to compare
	// error text, because a diagnostic's wording is not the claim.
	//
	// The pattern across them is not "koi lacks a feature". It is **koi
	// lacking a feature and reporting success** — which is why they belong
	// on this page rather than in the compat corpus: a harness cannot
	// route around a failure it is never told about.

	{
		Name: "find/grep shim: exec -a argv0 override",
		Provenance: "Claude Code ships bundled bfs and ugrep behind one binary and installs them as snapshot functions that rewrite argv[0]: " +
			"`(exec -a bfs \"$_cc_bin\" -S dfs ...)`. The zsh arm uses `ARGV0=`, which koi honors; the bash arm uses `exec -a`, which koi " +
			"treats as a command named `-a`. koi announces bash (#120), so it always takes the arm it cannot run — and 39.6% of 59,836 " +
			"recorded Bash-tool calls invoke bare find or grep. Fixed in the substrate (mvdan/sh#1386, our exec-flags carry) " +
			"rather than here, so this case is now an ordinary enforced gate",
		Argv: []string{"-c", `(exec -a nm /bin/echo shim-ran) 2>/dev/null || echo SHIM-FAILED`},
	},
	{
		Name:       "snapshot generator: declare -F survives eval",
		Provenance: "#215 taught declare its -F flag as a REPL parse-time rewrite; text arriving through eval is parsed by the substrate directly and still refuses the flag, so a harness helper that evals its probes sees the unfixed shell",
		Argv:       []string{"-c", `f(){ :; }; eval 'declare -F f' 2>/dev/null || echo EVAL-REFUSED`},
		Known:      242,
		KnownNote:  "the #215 fix is invisible to eval, command substitution and sourced files",
	},
	{
		Name:       "read -d '' consumes NUL-delimited input",
		Provenance: "`find -print0 | while read -r -d '' f` is the only safe way to walk filenames with spaces or newlines. -d was refused with exit 2, which nothing calling `read` checks, so the loop body simply never ran",
		Argv:       []string{"-c", `printf 'a\000b\000' | { read -r -d '' x 2>/dev/null; echo "[${x}]"; }`},
	},
	{
		Name:       "read -s does not silently return empty",
		Provenance: "the same sweep found -s returning nothing with no diagnostic. It was implemented, but read the process's own fd 0 rather than the shell's stdin, so every pipe and redirect — and every embedder supplying its own reader, koi included — got an empty string",
		Argv:       []string{"-c", `echo hi | { read -r -s x 2>/dev/null; echo "[${x}]"; }`},
	},
	{
		Name: "quoted heredoc writes the file it was given",
		Provenance: "`cat > f <<'EOF'` is the idiom for writing a file whose content must not be interpreted, and it is how an agent writes a script. " +
			"koi processes `\\\\`, `\\$` and backtick escapes anyway, so the file on disk is not the file requested",
		Argv:      []string{"-c", `cat > "$HOME/w" <<'X'` + "\n" + `re='\\d+' and \$var` + "\nX\n" + `cat "$HOME/w"`},
		Known:     244,
		KnownNote: "silently corrupts any heredoc-written file containing a doubled backslash or an escaped $ — scripts, regexes, JSON, Makefiles",
	},
	{
		Name:       "strict-mode header takes effect",
		Provenance: "`set -Eeuo pipefail` is the header on essentially every modern bash script and CI job. koi refuses the -E, applies none of the remaining flags, and returns 0",
		Argv:       []string{"-c", `set -Eeuo pipefail 2>/dev/null; false; echo REACHED`},
		Known:      245,
		KnownNote:  "one unsupported letter voids the whole call, so the script runs unprotected past the failure it was written to stop at",
	},
	{
		Name:       "noclobber protects an existing file",
		Provenance: "`set -C` was refused, so a redirect the script asked to have prevented proceeded — the data-loss corner of the same bug. Note that the stderr suppression has to come *before* the failing redirect: redirections are applied left to right, so a trailing `2>/dev/null` is set up too late to hide the diagnostic, in bash exactly as much as here",
		Argv:       []string{"-c", `printf old > "$HOME/n"; set -C; echo new 2>/dev/null > "$HOME/n"; cat "$HOME/n"`},
	},
	{
		Name:       "`>|` overrides noclobber",
		Provenance: "Claude Code's shell snapshot opens with `echo … >| \"$SNAPSHOT_FILE\"`, so `>|` is on the first line of the first thing the harness runs. It earns a case of its own because koi used to rewrite it to a plain `>`, which was harmless while the substrate had no noclobber and became the exact opposite of the intent once it did",
		Argv:       []string{"-c", `printf old > "$HOME/n"; set -C; echo new >| "$HOME/n"; cat "$HOME/n"`},
	},
	{
		Name:       "file descriptors above 2 carry data",
		Provenance: "fd 3+ is how a script separates a log or trace channel from stdout, and how `flock` and `read -u` are wired. Every spelling — exec 3>, 3<, per-command 3>, {v}> — accepts the write and discards it",
		Argv:       []string{"-c", `exec 3> "$HOME/fd"; echo hi >&3; exec 3>&-; cat "$HOME/fd"`},
		Known:      246,
		KnownNote:  "the shell reports success and produces an empty artifact, pointing blame at the program that was meant to write it",
	},
	{
		Name:       "PIPESTATUS reports each stage",
		Provenance: "`cmd | tee log` then `[ \"${PIPESTATUS[0]}\" -ne 0 ]` is the standard way to recover the status $? cannot answer; koi leaves the array empty, so the test either errors on an empty operand or inverts the outcome",
		Argv:       []string{"-c", `false | true; echo "[${PIPESTATUS[0]}:${PIPESTATUS[1]}]"`},
		Known:      247,
		KnownNote:  "a succeeding pipeline can read as failed and a failing one as succeeded, depending on which idiom the script used",
	},
	{
		Name:         "case ;;& falls through",
		Provenance:   "`;;&` is how a case classifies along several axes at once — the canonical `*.tar*) untar=1 ;;& *.gz) decomp=gunzip ;;` dispatch. koi parses the terminator and treats it as plain ;;",
		Argv:         []string{"-c", `case ab in a*) echo one;;& *b) echo two;; esac`},
		MinBashMajor: 4,
		Known:        248,
		KnownNote:    "wrong control flow with no diagnostic; a parse error would at least stop on the line responsible",
	},
	{
		Name:       "declare -i does arithmetic",
		Provenance: "an integer attribute that is refused leaves the literal source text in the variable, so every later comparison or accumulation is wrong",
		Argv:       []string{"-c", `declare -i n 2>/dev/null; n=1+1; echo "$n"`},
		Known:      249,
		KnownNote:  "the variable holds `1+1`; a numeric test on it errors and a value written onward is source text, not a number",
	},
	{
		Name:       "declare -r rejects assignment",
		Provenance: "scripts use readonly as a guard — fail if anything rebinds this — and to freeze configuration before sourcing untrusted fragments. koi keeps the original value but accepts the assignment with exit 0",
		Argv:       []string{"-c", `declare -r v=1 2>/dev/null; if v=2 2>/dev/null; then echo ASSIGN-ACCEPTED; else echo ASSIGN-REJECTED; fi`},
		Known:      249,
		KnownNote:  "the guard is decorative: no diagnostic and no status, so the intent behind readonly is silently unmet",
	},
	{
		Name:       "FUNCNAME locates an error",
		Provenance: "`die() { echo \"${BASH_SOURCE[1]}:${BASH_LINENO[0]}: ${FUNCNAME[1]}: $*\" >&2; }` is every hand-rolled error helper; under koi it prints the message with the location blank",
		Argv:       []string{"-c", `f(){ echo "fn=${FUNCNAME[0]:-MISSING}"; }; f`},
		Known:      250,
		KnownNote:  "a script's own diagnostics lose the part worth having, and nothing errors to say so",
	},
	{
		Name:       "compgen -A function enumerates functions",
		Provenance: "the other way a harness asks which functions exist, alongside the declare -F that #215 fixed; this one still answers nothing",
		Argv:       []string{"-c", `g(){ :; }; compgen -A function 2>/dev/null | grep -c '^g$'`},
		Known:      250,
		KnownNote:  "a snapshot generator using compgen rather than declare -F carries none of the user's functions across",
	},
}

// AgentResult is one case's verdict.
type AgentResult struct {
	AgentCase
	BashOut, KoiOut   string
	BashCode, KoiCode int
	Pass              bool
	Reason            string
	// Skipped marks a case whose oracle is too old to answer it. Not a
	// pass: it says the gate did not get to run here.
	Skipped bool
}

// RunAgentAll runs the whole gate.
func RunAgentAll(ctx context.Context, bashBin, koiBin string) []AgentResult {
	out := make([]AgentResult, 0, len(AgentCorpus))
	for _, c := range AgentCorpus {
		out = append(out, RunAgent(ctx, bashBin, koiBin, c))
	}
	return out
}

// RunAgent runs one case under both shells and compares — or, for a case
// carrying Expect, holds koi to the stated behavior instead.
func RunAgent(ctx context.Context, bashBin, koiBin string, c AgentCase) AgentResult {
	r := AgentResult{AgentCase: c}
	r.KoiOut, r.KoiCode = runAgentArgv(ctx, koiBin, c)
	if c.Expect != "" {
		r.Pass = r.KoiOut == c.Expect
		if !r.Pass {
			r.Reason = "want " + firstLine(strings.TrimSpace(c.Expect))
		}
		return r
	}
	if c.MinBashMajor > 0 {
		if major := oracleBashMajor(ctx, bashBin); major > 0 && major < c.MinBashMajor {
			r.Skipped = true
			r.Reason = fmt.Sprintf("oracle is bash %d: this case needs bash %d or newer to answer it", major, c.MinBashMajor)
			return r
		}
	}
	r.BashOut, r.BashCode = runAgentArgv(ctx, bashBin, c)
	r.BashOut = dropJobControlPreamble(r.BashOut)
	switch {
	case r.BashOut == r.KoiOut && r.BashCode == r.KoiCode:
		r.Pass = true
	case r.BashOut != r.KoiOut && r.BashCode != r.KoiCode:
		r.Reason = "output and exit status differ"
	case r.BashOut != r.KoiOut:
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
	home, err := os.MkdirTemp("", "koi-agent-")
	if err != nil {
		return "[runner error: " + err.Error() + "]", -1
	}
	defer os.RemoveAll(home) //nolint:errcheck // scratch dir

	// bash and koi read different file names for the same two roles, so
	// each shell is given the pair it actually looks for. Writing only
	// koi's would test koi against a bash that was never configured.
	if c.RC != "" {
		for _, name := range []string{".bashrc", ".koirc"} {
			if werr := os.WriteFile(filepath.Join(home, name), []byte(c.RC), 0o600); werr != nil {
				return "[runner error: " + werr.Error() + "]", -1
			}
		}
	}
	if c.Profile != "" {
		for _, name := range []string{".bash_profile", ".profile", ".koi_profile"} {
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
		// Point every XDG root into the scratch home too: koi resolves its
		// rc through XDG first, and a developer's real config would
		// otherwise leak into the gate.
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"KOI_WELCOME=off",
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

// oracleBashMajor asks the oracle its own major version. 0 means unknown, which
// is deliberately treated as "run the case": an unreadable version must
// not silently disable a gate.
func oracleBashMajor(ctx context.Context, bashBin string) int {
	out, err := exec.CommandContext(ctx, bashBin, "-c", "echo ${BASH_VERSINFO[0]}").Output()
	if err != nil {
		return 0
	}
	major, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return major
}

// AgentFailures returns the failing results, corpus order. A skipped case
// is not a failure — it is a case the oracle could not answer.
func AgentFailures(results []AgentResult) []AgentResult {
	var out []AgentResult
	for _, r := range results {
		if !r.Pass && !r.Skipped {
			out = append(out, r)
		}
	}
	return out
}

// AgentRegressions is the set that must be empty: a case failing with no
// issue number on it. Everything Known is accounted for elsewhere.
func AgentRegressions(results []AgentResult) []AgentResult {
	var out []AgentResult
	for _, r := range results {
		if !r.Pass && !r.Skipped && r.Known == 0 {
			out = append(out, r)
		}
	}
	return out
}

// AgentKnownGaps is the filed-and-still-failing set — what the published
// page owes the reader.
func AgentKnownGaps(results []AgentResult) []AgentResult {
	var out []AgentResult
	for _, r := range results {
		if !r.Pass && !r.Skipped && r.Known > 0 {
			out = append(out, r)
		}
	}
	return out
}

// AgentStaleKnown is a Known case that now passes.
//
// This is a build failure rather than good news quietly absorbed. The
// marker suppresses a real gate, so leaving one in place after the fix
// means the case stops being enforced the moment it starts working — and
// the published page keeps claiming a gap koi no longer has.
func AgentStaleKnown(results []AgentResult) []AgentResult {
	var out []AgentResult
	for _, r := range results {
		if r.Pass && r.Known > 0 {
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
		return "koi: " + firstLine(strings.TrimSpace(r.KoiOut)) + " · " + r.Reason
	}
	bash := strings.TrimSpace(r.BashOut)
	koi := strings.TrimSpace(r.KoiOut)
	if bash == koi {
		return "exit " + itoa(r.BashCode) + " vs " + itoa(r.KoiCode)
	}
	return "bash: " + firstLine(bash) + " · koi: " + firstLine(koi)
}

// dropJobControlPreamble removes the two lines bash prints when asked for
// an interactive shell without a controlling terminal:
//
//	bash: cannot set terminal process group (…): Inappropriate ioctl for device
//	bash: no job control in this shell
//
// That is an artifact of running the gate from a test process, not a
// compatibility difference — and normalizing it is the honest move only
// because the direction is in koi's favor: the preamble is bash's, koi
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
