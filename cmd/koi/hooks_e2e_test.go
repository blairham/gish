//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The bash hook surface, end to end (#159).
//
// These are the hooks every add-on in the ecosystem is built on:
// starship, zoxide, atuin, direnv and mise all install a
// PROMPT_COMMAND, a DEBUG trap, or both. Getting them right is what
// lets koi inherit the ecosystem instead of waiting to be adopted by
// it — the failure mode nushell demonstrates, with more stars than fish
// and a quarter of the installs.
//
// They are tested through a real pty because a hook is a property of
// the interactive loop: "the shell is about to prompt" and "the shell
// is about to run your line" do not happen in a script.

// hookSession starts a shell whose rc is rc.
func hookSession(t *testing.T, rc string) *ptySession {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "koirc")
	if err := os.WriteFile(path, []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
	return startPTY(t, ptyOptions{Dir: dir, Env: []string{"KOI_RC=" + path}})
}

func TestPromptCommandRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := hookSession(t, "PROMPT_COMMAND='printf pc-ran\\n'\n")
	s.waitFor("pc-ran")

	// And again before the next prompt: a hook that runs once is a hook
	// that silently stops updating whatever it maintains.
	s.runProbe("echo between", "between")
	s.waitFor("pc-ran")
}

// bash 5.1 made PROMPT_COMMAND an array, so tools emit whichever form
// their author's bash had. Both are in the wild; both have to work.
func TestPromptCommandArrayForm(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := hookSession(t, "PROMPT_COMMAND=('printf first\\n' 'printf second\\n')\n")
	out := s.waitFor("second")
	if strings.Index(out, "first") > strings.Index(out, "second") {
		t.Errorf("array elements ran out of order: %q", out)
	}
}

// PS0 prints between the line and its output — timing banners and
// rulers are built on it.
func TestPS0PrintsBeforeOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := hookSession(t, "PS0='ps0-marker\\n'\n")
	s.waitForPrompt()
	out := s.runProbe("echo the-out'put'", "the-output")
	if !strings.Contains(out, "ps0-marker") {
		t.Fatalf("PS0 did not print: %q", out)
	}
	if strings.Index(out, "ps0-marker") > strings.Index(out, "the-output") {
		t.Errorf("PS0 printed after the output: %q", out)
	}
}

// The DEBUG trap is the preexec hook, and BASH_COMMAND is how every
// consumer of it — bash-preexec above all — learns what is about to run.
func TestDebugTrapSeesTheCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := hookSession(t, `trap 'printf "pre:%s\n" "$BASH_COMMAND"' DEBUG`+"\n")
	s.waitForPrompt()
	// Quoted on the way in so the marker in the output cannot be the
	// pty's echo of what was typed.
	out := s.runProbe("echo ran'-it'", "ran-it")
	// BASH_COMMAND is the line as written, quotes and all — which is
	// what bash reports and what consumers expect to re-quote.
	if !strings.Contains(out, `pre:echo ran'-it'`) {
		t.Errorf("DEBUG trap did not see the command: %q", out)
	}
}

// Under extdebug, a non-zero return from the DEBUG trap cancels the
// command. That is the semantics tools rely on to say "not that one",
// and without it the trap is a notification rather than a hook.
func TestExtdebugCancelsTheCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := hookSession(t, "shopt -s extdebug\ntrap '[[ $BASH_COMMAND == *forbidden* ]] && exit 1 || true' DEBUG\n")
	s.waitForPrompt()

	s.runProbe("echo allow'ed'", "allowed")

	// The canceled line prints nothing, so there is no marker to wait
	// for and the D mark is the only signal that it is over. Waiting on
	// the *prompt* mark here is what #286 was: the editor redraws the
	// prompt on every echoed keystroke, so a B mark is already in the
	// buffer while the line is still being typed, the wait returns at
	// once, and the next send lands in the raw-mode re-entry window
	// where queued input is discarded — 30s of silence from a shell
	// behaving perfectly.
	s.runLine("echo forbidden-out'put'")
	if strings.Contains(s.plain(), "forbidden-output") {
		t.Errorf("extdebug did not cancel the command: %q", s.plain())
	}
}

// bash-preexec is the shim a dozen tools install themselves through, and
// it is built entirely out of the two hooks above: a DEBUG trap for
// preexec and PROMPT_COMMAND for precmd. If it works, they work.
func TestBashPreexecStyleShim(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	// The shape of bash-preexec, reduced to its mechanism: a trap that
	// fires preexec functions once per command line, and a
	// PROMPT_COMMAND that fires precmd functions and re-arms it.
	rc := `
__koi_preexec_ready=""
preexec() { printf "preexec:%s\n" "$1"; }
precmd()  { printf "precmd\n"; }
__koi_debug() {
  [ -n "$__koi_preexec_ready" ] || return 0
  __koi_preexec_ready=""
  preexec "$BASH_COMMAND"
}
__koi_prompt() {
  precmd
  __koi_preexec_ready="1"
}
trap '__koi_debug' DEBUG
PROMPT_COMMAND='__koi_prompt'
`
	s := hookSession(t, rc)
	s.waitFor("precmd")
	out := s.runProbe("echo shim-work's'", "shim-works")
	if !strings.Contains(out, `preexec:echo shim-work's'`) {
		t.Errorf("preexec did not fire with the command line: %q", out)
	}
	// precmd runs on the way to the *next* prompt, so what has to be
	// waited for is precmd itself. Waiting on the prompt mark would
	// return at once: every keystroke redraws the prompt, marks and all.
	s.waitFor("precmd")
}

// `bind -x` is how fzf installs Ctrl-T, how atuin takes over Ctrl-R,
// and how rc files add one-key shortcuts. The contract is
// READLINE_LINE and READLINE_POINT: the command reads them, may rewrite
// them, and the editor takes back whatever it left.
func TestBindXRunsAndRewritesTheLine(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	// The shape of an fzf widget, minus fzf: read the line, put
	// something else there, and move the cursor.
	//
	// The rewritten line prints a *variable* rather than a literal, and
	// that indirection is the whole point of the test rather than a
	// flourish. When the widget wrote `echo bound-widget-ran`, the marker
	// appeared the moment the editor echoed the rewritten line back --
	// before anything ran -- so the wait after the Enter was satisfied by
	// that same echo and returned immediately. The test passed whether or
	// not the editor ever took the line back and ran it, which is the half
	// its name claims (#299). Here the echo can only ever show
	// `echo $widget_output`; the marker exists nowhere until the shell
	// expands it, so only execution can satisfy the assertion.
	//
	// The \$ is escaped so the expansion happens when the *line runs*,
	// not when the widget body runs -- unescaped, the widget would write
	// the marker straight into READLINE_LINE and put us back where we
	// started.
	rc := "widget_output=bound-widget-ran\n" +
		`bind -x '"\C-t": READLINE_LINE="echo \$widget_output"; READLINE_POINT=${#READLINE_LINE}'` + "\n"
	s := hookSession(t, rc)
	s.waitForPrompt()
	s.buf.Reset()
	// Ctrl-T, then the rewritten line is echoed back: READLINE_LINE was set.
	s.sendUntil("\x14", "echo $widget_output")
	// No buffer clear between the waits -- #195 bans that, and nothing
	// here needs it: the marker cannot already be present.
	s.send("\r")
	s.waitFor("bound-widget-ran")
	// And the shell got back to a prompt, so the line was accepted rather
	// than left in the editor.
	s.waitFor(commandDone)
}

// A binding koi cannot express must cost that binding and nothing
// else: an rc that sets a dozen keeps the other eleven.
func TestBindTolerantOfWhatItCannotDo(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	rc := `bind -m emacs-standard '"\C-g": redraw-current-line'
bind '"\C-r": reverse-search-history'
bind -x '"\C-t": READLINE_LINE="after-the-noise"'
printf rc-finished\n
`
	s := hookSession(t, rc)
	s.waitFor("rc-finished")
	s.buf.Reset()
	s.sendUntil("\x14", "after-the-noise")
}

// `complete -F` is the largest single piece of the ecosystem koi
// inherits by speaking bash: bash-completion ships hundreds of these,
// and every modern CLI emits one from `<tool> completion bash`. They are
// all written against the same three things — the `complete` builtin,
// the COMP_* variables, and COMPREPLY.
func TestCompleteFunctionDrivesTab(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	rc := `
_demo_complete() {
  # The shape every bash completion has: read the current word out of
  # COMP_WORDS, generate against it, leave the answer in COMPREPLY.
  local cur="${COMP_WORDS[COMP_CWORD]}"
  COMPREPLY=( $(compgen -W "deploy destroy diagnose" -- "$cur") )
}
complete -F _demo_complete demo
`
	s := hookSession(t, rc)
	s.waitForPrompt()

	// Two candidates share the prefix "de", so Tab completes as far as
	// the common prefix and lists the rest.
	s.buf.Reset()
	s.send("demo d")
	s.sendUntil("\t", "deploy")
	out := s.plain()
	for _, want := range []string{"deploy", "destroy", "diagnose"} {
		if !strings.Contains(out, want) {
			t.Errorf("candidate %q missing from the listing: %q", want, out)
		}
	}

	// A unique prefix completes outright, and the completion actually
	// runs — which is the part a listing alone would not prove.
	s.buf.Reset()
	s.send("i")
	s.sendUntil("\t", "diagnose")
}

// COMP_LINE and COMP_POINT are the other half of the contract: the
// completions that cannot work from word splitting alone read them.
func TestCompletionSeesTheWholeLine(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	rc := `
_line_complete() { COMPREPLY=( "point-${COMP_POINT}-cword-${COMP_CWORD}" ); }
complete -F _line_complete probe
`
	s := hookSession(t, rc)
	s.waitForPrompt()
	s.buf.Reset()
	s.send("probe arg ")
	s.sendUntil("\t", "point-10-cword-2")
}

// `complete -I` is the initial-word completion bash 5.1 added, and koi
// accepted it, stored it, and never asked it anything (#609): the command
// position was answered by internal/complete's PATH/builtin/function
// providers, which never consult the spec registry. A "complete anything
// in command position" registration was a line bash takes and koi
// silently dropped.
//
// It is a property of a live completion, so this drives a real pty, and
// every case runs the completed line and reads what it *printed* — the
// buffer redraws on every keystroke, so an assertion about the screen is
// a redraw detector (#240).
const initialWordRC = `
initcand() { printf 'res[%s]\n' "$*"; }
zzzunique() { printf 'res[Z]\n'; }
argprobe() { printf 'res[%s]\n' "$*"; }
_ini() { COMPREPLY=( initcand ); }
_ini_none() { COMPREPLY=(); INI_RAN=yes; }
`

func TestCompleteIAnswersTheCommandPosition(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := hookSession(t, initialWordRC+"complete -I -F _ini\n")
	s.waitForPrompt()

	// A word in command position: the spec answers, and its candidate
	// replaces what was typed — bash does not prefix-filter COMPREPLY,
	// so `xyz` becomes `initcand`.
	s.buf.Reset()
	s.send("xyz")
	s.sendUntil("\t", "initcand")
	s.send("\r")
	s.waitFor("res[]")

	// Still the initial word after a `;`, which is command position and
	// not the start of the line. Measured: bash answers there too.
	s.buf.Reset()
	s.send("printf 'resA\\n'; xyz")
	s.sendUntil("\t", "initcand")
	s.send("\r")
	s.waitFor("resA")
	s.waitFor("res[]")

	// And not an argument. `argprobe zz<TAB>` completes nothing in either
	// shell, so the word must survive — and the assertion is what the
	// command *printed* rather than the absence of a candidate, since
	// argprobe echoes its arguments: a spec that answered here would make
	// this `res[initcand]`.
	s.buf.Reset()
	s.send("argprobe zz")
	s.waitFor("argprobe zz")
	s.send("\t\r")
	s.waitFor("res[zz]")
}

// What a `-I` spec that generates nothing means was the question the
// issue said to measure rather than assume, and the guess in it was
// wrong: bash does **not** fall back to its own command completion. The
// spec replaces it, and only `-o bashdefault` — the option that exists to
// ask for the fallback — brings it back.
//
// Both halves are asserted, because either alone passes vacuously: a
// shell that completed nothing ever would pass the no-fallback case, and
// one that ignored `-I` entirely would pass the bashdefault case.
func TestCompleteIReplacesCommandCompletion(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}

	// The control: with no spec at all, the command position completes.
	s := hookSession(t, initialWordRC)
	s.waitForPrompt()
	s.buf.Reset()
	s.send("zzzuniq")
	s.sendUntil("\t", "zzzunique")
	s.send("\r")
	s.waitFor("res[Z]")

	// A spec that generates nothing takes the position with it — and the
	// absence has to be paid for with evidence that Tab arrived, or a
	// dropped keystroke passes this case for the wrong reason (#240).
	// The spec records that it ran, and the probe after it is the proof.
	s = hookSession(t, initialWordRC+"complete -I -F _ini_none\n")
	s.waitForPrompt()
	s.send("zzzuniq")
	s.waitFor("zzzuniq")
	s.buf.Reset()
	s.send("\t\t; printf 'tail\\n'\r")
	s.waitFor(commandDone)
	if out := s.plain(); strings.Contains(out, "res[Z]") {
		t.Errorf("the command position completed under an -I spec that generated nothing:\n%s", out)
	}
	// The marker cannot appear in the echoed source, which is what makes
	// this a reading of the answer rather than of the keystrokes.
	s.runLine(`printf '%s\n' "SPEC${INI_RAN:-MISSED}"`)
	if out := s.plain(); !strings.Contains(out, "SPECyes") {
		t.Errorf("the -I spec never ran, so the missing completion proves nothing:\n%s", out)
	}

	// And `-o bashdefault` asks for it back.
	s = hookSession(t, initialWordRC+"complete -I -o bashdefault -F _ini_none\n")
	s.waitForPrompt()
	s.buf.Reset()
	s.send("zzzuniq")
	s.sendUntil("\t", "zzzunique")
	s.send("\r")
	s.waitFor("res[Z]")
}

// `-E` is the empty-line spec and `-I` the initial-word one, and an empty
// line is both. bash consults `-E` there, measured with both registered.
func TestCompleteEBeatsIOnAnEmptyLine(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	rc := initialWordRC + `
_empty() { COMPREPLY=( "printf 'resE\n'" ); }
complete -E -F _empty
complete -I -F _ini
`
	s := hookSession(t, rc)
	s.waitForPrompt()
	s.buf.Reset()
	s.sendUntil("\t", "resE")
	s.send("\r")
	s.waitFor("resE")
}

// `compopt -o nospace` from inside a completion function (#612).
//
// This is the form the bash-completion corpus writes — a function turning
// nospace on for one branch of its answer — and koi's compopt answered 0
// and did nothing, so the space was inserted anyway. It is a property of
// a live completion twice over: the option has to reach the *caller* of
// the function, since nospace is read once the function has returned, and
// the effect is a keystroke the human sees.
//
// The control case is the point of the pair: a shell that inserted no
// space for either spec would pass the nospace half on its own.
func TestCompoptNoSpaceReachesTheEditor(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	rc := `
demo() { printf 'res[%s]\n' "$*"; }
demoplain() { printf 'res[%s]\n' "$*"; }
_demo_tight() { compopt -o nospace; COMPREPLY=( uniquecand ); }
_demo_loose() { COMPREPLY=( uniquecand ); }
complete -F _demo_tight demo
complete -F _demo_loose demoplain
`
	s := hookSession(t, rc)
	s.waitForPrompt()

	// Without compopt the editor inserts the candidate and a space, so
	// the next character typed starts a second word.
	s.send("demoplain uniq")
	s.sendUntil("\t", "uniquecand")
	s.send("Z\r")
	s.waitFor("res[uniquecand Z]")

	// With it, there is no space and the character joins the candidate.
	s.buf.Reset()
	s.send("demo uniq")
	s.sendUntil("\t", "uniquecand")
	s.send("Z\r")
	s.waitFor("res[uniquecandZ]")

	// And the adjustment was transient, as bash's is: the registration it
	// ran from is unchanged, which is why runCompletionSpec publishes a
	// copy rather than the spec itself.
	out := s.runProbe(`complete -p demo`, "complete -F _demo_tight demo")
	if strings.Contains(out, "nospace") {
		t.Errorf("compopt inside a completion persisted into the registration:\n%s", out)
	}
}
