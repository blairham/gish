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
// lets gish inherit the ecosystem instead of waiting to be adopted by
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
	path := filepath.Join(dir, "gishrc")
	if err := os.WriteFile(path, []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
	return startPTY(t, ptyOptions{Dir: dir, Env: []string{"GISH_RC=" + path}})
}

func TestPromptCommandRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := hookSession(t, "PROMPT_COMMAND='printf pc-ran\\n'\n")
	s.waitFor("pc-ran")

	// And again before the next prompt: a hook that runs once is a hook
	// that silently stops updating whatever it maintains.
	s.buf.Reset()
	s.send("echo between\r")
	s.waitFor("between")
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
	s.buf.Reset()
	s.send("echo the-out'put'\r")
	out := s.waitFor("the-output")
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
	s.buf.Reset()
	// Quoted on the way in so the marker in the output cannot be the
	// pty's echo of what was typed.
	s.send("echo ran'-it'\r")
	out := s.waitFor("ran-it")
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

	s.buf.Reset()
	s.send("echo allow'ed'\r")
	s.waitFor("allowed")

	s.buf.Reset()
	s.send("echo forbidden-out'put'\r")
	s.waitForPrompt()
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
__gish_preexec_ready=""
preexec() { printf "preexec:%s\n" "$1"; }
precmd()  { printf "precmd\n"; }
__gish_debug() {
  [ -n "$__gish_preexec_ready" ] || return 0
  __gish_preexec_ready=""
  preexec "$BASH_COMMAND"
}
__gish_prompt() {
  precmd
  __gish_preexec_ready="1"
}
trap '__gish_debug' DEBUG
PROMPT_COMMAND='__gish_prompt'
`
	s := hookSession(t, rc)
	s.waitFor("precmd")
	s.buf.Reset()
	s.send("echo shim-work's'\r")
	out := s.waitFor("shim-works")
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
	s := hookSession(t, `bind -x '"\C-t": READLINE_LINE="echo bound-widget-ran"; READLINE_POINT=${#READLINE_LINE}'`+"\n")
	s.waitForPrompt()
	s.buf.Reset()
	s.sendUntil("\x14", "bound-widget-ran") // Ctrl-T, then the rewritten line is echoed back
	s.send("\r")
	s.waitFor("bound-widget-ran")
}

// A binding gish cannot express must cost that binding and nothing
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

// `complete -F` is the largest single piece of the ecosystem gish
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
