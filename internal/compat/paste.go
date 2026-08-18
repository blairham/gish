//go:build unix

package compat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
)

// The paste gate (#161).
//
// "Pasted bash one-liners fail" is the top abandonment cause on two
// independent corpora — 22 first-person accounts on HN/Lobsters over a
// decade, and 27 of 62 churn accounts on Reddit. docs/compat.md scores
// bash *scripts*; nothing scored the thing people actually do, which is
// paste a line at an interactive prompt and press Enter.
//
// The difference is not academic. A script runs through `-c`, where
// koi is deliberately POSIX-clean; a pasted line goes through the line
// editor, bracketed paste, history expansion, the diagnostics pass and
// the accept-or-continue decision before the interpreter ever sees it.
// Every one of those is a place a line can change meaning, and none of
// them is exercised by the script corpus.
//
// bash stays the oracle: each case's expected output comes from running
// the same text through real bash. Nothing here encodes what we think
// bash ought to do with `!!` inside a paste — bash is asked.

// PasteCase is one line pasted at an interactive prompt.
type PasteCase struct {
	Name       string
	Text       string // exactly what is pasted
	Provenance string
	// Setup is pasted and run first, when a case needs history to exist
	// (the `!!` family) or a file to be there.
	Setup string
	// OracleScript overrides what bash is asked, for the cases where a
	// pasted line and a `-c` script are not the same question — history
	// expansion is off in bash's non-interactive mode, so `sudo !!` has
	// to be posed to bash differently to get an honest answer.
	OracleScript string
	// MinBashMajor skips the case when the oracle is older than this.
	//
	// macOS still ships bash 3.2.57 from 2007, where `${x^^}` is a "bad
	// substitution" — so on that runner koi is *ahead* of the oracle and
	// the differential dutifully reports a difference. docs/compat.md
	// already handles this for the script corpus by comparing majors;
	// this is the same rule per case, because one paste case needing
	// bash 4 should not disable the gate for the other seventeen.
	MinBashMajor int
}

// PasteCorpus is the published gate. The constructs are the ones named
// in churn accounts, not a survey of bash syntax.
var PasteCorpus = []PasteCase{
	{
		Name:       "command substitution",
		Provenance: "the single most-pasted construct; named in HN 2024-01-23",
		Text:       `echo "today is $(echo Tuesday)"`,
	},
	{
		Name:       "backticks",
		Provenance: "older one-liners and README snippets still use them",
		Text:       "echo \"kernel is `echo 6.1`\"",
	},
	{
		Name:       "&& and || chains",
		Provenance: "every install one-liner",
		Text:       `true && echo ok || echo unreachable; false && echo no || echo fallback`,
	},
	{
		Name:       "one-shot environment prefix",
		Provenance: "`FOO=bar cmd` — the idiom people paste from CI docs",
		Text:       `GREETING=hello env | grep '^GREETING='`,
	},
	{
		Name:       "while loop on one line",
		Provenance: "r/commandline paste threads; a compound command typed as one line",
		Text:       `i=0; while [ $i -lt 3 ]; do echo "n=$i"; i=$((i+1)); done`,
	},
	{
		Name:       "for loop with brace expansion",
		Provenance: "brace expansion is bash-only and is pasted constantly",
		Text:       `for f in file{1..3}.txt; do echo "$f"; done`,
	},
	{
		Name:       "[[ ]] test with pattern match",
		Provenance: "bash conditional expressions in pasted snippets",
		Text:       `s=hello-world; if [[ $s == hello-* ]]; then echo matched; fi`,
	},
	{
		Name:       "process substitution",
		Provenance: "`diff <(a) <(b)` — named as a fish blocker",
		Text:       `diff <(echo same) <(echo same) && echo identical`,
	},
	{
		Name:       "unquoted URL with a query string",
		Provenance: "`mpv https://…?v=X` — a day-one quit in fish and unconfigured zsh",
		Text:       `echo https://example.com/watch?v=dQw4w9WgXcQ`,
	},
	{
		Name:       "unquoted URL with bracket characters",
		Provenance: "the same class: [ and ] are glob metacharacters too",
		Text:       `echo https://example.com/a[b]c`,
	},
	{
		Name:       "single quotes protect everything",
		Provenance: "install lines that embed $ and ! inside single quotes",
		Text:       `echo 'literal $HOME and !! and $(date)'`,
	},
	{
		Name:       "heredoc pasted as one block",
		Provenance: "config-writing one-liners in READMEs",
		Text:       "cat <<'EOF'\nline one\nline two\nEOF",
	},
	{
		Name:         "arithmetic and parameter expansion together",
		Provenance:   "the mixed-expansion lines that break naive parsers",
		Text:         `x=hello; echo "${x^^} has ${#x} chars, twice is $((2*${#x}))"`,
		MinBashMajor: 4, // ${x^^} is a bad substitution in Apple's bash 3.2
	},
	{
		Name:       "pipeline with quoting",
		Provenance: "grep/awk/sed one-liners, the bread and butter of a paste",
		Text:       `printf 'a b\nc d\n' | awk '{print $2}' | tr '\n' ',' ; echo`,
	},
	{
		Name:       "escaped newline continuation",
		Provenance: "multi-line install commands pasted whole",
		Text:       "echo one \\\n  two \\\n  three",
	},
	{
		Name:       "history expansion: !! with a command prefix",
		Provenance: "`sudo !!` — the most-named muscle memory in the corpus",
		Setup:      "echo first-command",
		Text:       "env !!",
		// bash expands !! only when history expansion is on, which it is
		// not under -c. Asking bash the equivalent question directly is
		// more honest than asserting what we think it would print.
		OracleScript: "env echo first-command",
	},
	{
		Name:         "history expansion: !$ last argument",
		Provenance:   "!$ is second only to !! in the accounts",
		Setup:        "echo alpha beta",
		Text:         "echo !$",
		OracleScript: "echo beta",
	},
	{
		Name:       "a bang inside single quotes does not expand",
		Provenance: "the classic zsh papercut: 'don't!' at a prompt",
		Text:       `echo 'wait for it!'`,
	},
}

// PasteResult is one case's verdict. Skipped=true means the oracle on
// this machine is too old to answer, which is neither a pass nor a
// failure — see PasteCase.MinBashMajor.
type PasteResult struct {
	PasteCase
	Skipped           bool
	BashOut, KoiOut   string
	BashCode, KoiCode int
	Pass              bool
	Reason            string
}

// RunPaste runs one case: the text through real bash for the oracle,
// and the same text pasted into an interactive koi on a pty.
func RunPaste(ctx context.Context, bashBin, koiBin string, c PasteCase) PasteResult {
	r := PasteResult{PasteCase: c}
	if c.MinBashMajor > 0 && bashMajorVersion(bashBin) < c.MinBashMajor {
		r.Skipped = true
		r.Reason = "needs bash " + itoa(c.MinBashMajor) + "+; this machine's oracle is older"
		return r
	}
	oracle := c.OracleScript
	if oracle == "" {
		oracle = c.Text
	}
	if c.Setup != "" && c.OracleScript == "" {
		oracle = c.Setup + "\n" + oracle
	}
	r.BashOut, r.BashCode = runScript(ctx, bashBin, oracle)

	out, code, err := pasteIntoKoi(ctx, koiBin, c)
	if err != nil {
		r.Reason = "koi: " + err.Error()
		return r
	}
	r.KoiOut, r.KoiCode = out, code

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

// bashMajorVersion asks the oracle what it is. Cached, because the
// answer cannot change during a run and the corpus asks per case.
func bashMajorVersion(bashBin string) int {
	bashMajorOnce.Do(func() {
		out, err := exec.Command(bashBin, "-c", "echo ${BASH_VERSINFO[0]}").Output() //nolint:gosec // the oracle we were handed
		if err != nil {
			bashMajor = 0
			return
		}
		bashMajor, _ = strconv.Atoi(strings.TrimSpace(string(out)))
	})
	return bashMajor
}

var (
	bashMajorOnce sync.Once
	bashMajor     int
)

// RunPasteAll runs the whole paste corpus.
func RunPasteAll(ctx context.Context, bashBin, koiBin string) []PasteResult {
	out := make([]PasteResult, 0, len(PasteCorpus))
	for _, c := range PasteCorpus {
		out = append(out, RunPaste(ctx, bashBin, koiBin, c))
	}
	return out
}

// pasteTimeout caps one pasted case outright, as a backstop against a
// shell that emits forever without ever printing the mark. See
// pasteIntoKoi.
const pasteTimeout = 4 * time.Minute

// pasteIdle is the bound that actually fires: how long the terminal may
// say *nothing* before the case is called stuck (#189).
//
// The paste gate is enforced as an absolute pass count, on the reasoning
// that it "is hermetic — it depends on nothing but bash and koi". That
// is true of its dependencies and false of its scheduling: `go test
// -race ./...` runs every package at once, and one starved case drops
// 18/18 to 17/18 and hard-fails a build whose diff was innocent. Exactly
// that happened, with `saw ""` — not a wrong answer, no answer at all.
//
// So the two are separated: a case that is being served slowly keeps
// resetting this and passes; a case whose shell has wedged trips it, and
// sooner than the old 60s total did.
const pasteIdle = 30 * time.Second

// Semantic marks (#99) delimit each command's output exactly: C opens
// it, D closes it and carries the status. Parsing the screen without
// them would mean guessing where the prompt ended and the output began,
// which is precisely the ambiguity the marks exist to remove.
var (
	markPromptEnd = "\x1b]133;B"
	outputRe      = regexp.MustCompile(`\x1b\]133;C(?:;[^\x1b\a]*)?(?:\x1b\\|\a)([\s\S]*?)\x1b\]133;D;(-?\d+)`)
	// OSC strings (to their ST or BEL terminator) and CSI sequences,
	// including the one space intermediate that DECSCUSR's cursor-shape
	// request carries. Written with named classes rather than raw byte
	// ranges: `[@-~]` is the correct final-byte class and reads like a
	// typo, and a linter that says so is right more often than not.
	ansiEscapes = regexp.MustCompile(`\x1b\][^\x1b\a]*(?:\x1b\\|\a)|\x1b\[[0-9;?]* ?[A-Za-z@\[\]^_` + "`" + `{|}~]|\x1b[A-Z\\_]`)
)

// pasteIntoKoi drives a real interactive koi: bracketed paste in,
// Enter, and the output read back from between the semantic marks.
func pasteIntoKoi(ctx context.Context, koiBin string, c PasteCase) (string, int, error) {
	home, err := os.MkdirTemp("", "koi-paste-*")
	if err != nil {
		return "", 0, err
	}
	defer os.RemoveAll(home)

	// The pty path gets its own budget rather than the script one. A
	// case here starts a real shell, waits for a real prompt and types
	// into it, and a loaded CI runner redraws the prompt for every
	// echoed keystroke — the same reason cmd/koi's e2e harness settled
	// on a minute. The gate is not measuring speed, so a tight bound
	// only buys flakes: a macOS runner missed the *first prompt* inside
	// 20s and reported a shell that had produced nothing.
	ctx, cancel := context.WithTimeout(ctx, pasteTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, koiBin)
	cmd.Dir = home
	cmd.Env = []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"TERM=xterm-256color",
		"PATH=" + pathEnv(),
		// The gate is about what the shell does with the text, not about
		// what a theme paints around it.
		"KOI_PROMPT=koi-paste-gate% ",
		"NO_COLOR=1",
	}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 200})
	if err != nil {
		return "", 0, err
	}
	defer func() {
		f.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// A pty read cannot take a deadline, so it lives in a goroutine and
	// the waiting happens in a select — the same rule the benchmark and
	// e2e harnesses learned the hard way.
	var closed atomic.Bool
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
				closed.Store(true)
				return
			}
		}
	}()

	var seen bytes.Buffer
	// Silence, not elapsed time, is what says the shell is not coming
	// back (#189). ctx still caps the case outright — see pasteIdle.
	idle := time.NewTimer(pasteIdle)
	defer idle.Stop()
	// waitFor blocks until want appears, leaving the buffer intact: the
	// final wait's buffer *is* the payload the case is judged on.
	waitFor := func(want string) error {
		for !bytes.Contains(seen.Bytes(), []byte(want)) {
			select {
			case chunk, ok := <-chunks:
				if !ok {
					return fmt.Errorf("shell exited before %q", want)
				}
				seen.Write(chunk)
				idle.Reset(pasteIdle)
			case <-idle.C:
				return fmt.Errorf("no output for %s waiting for %q; saw %q", pasteIdle, want, plainText(seen.String()))
			case <-ctx.Done():
				return fmt.Errorf("timeout waiting for %q; saw %q", want, plainText(seen.String()))
			}
		}
		return nil
	}
	// waitPast is waitFor for the synchronization waits, dropping what it
	// matched so the *next* wait cannot re-match it — the non-destructive
	// half of what `seen.Reset()` used to do, without discarding the
	// bytes that shared the read (#195).
	waitPast := func(want string) error {
		if err := waitFor(want); err != nil {
			return err
		}
		consumeThrough(&seen, bytes.Index(seen.Bytes(), []byte(want))+len(want))
		return nil
	}

	if err := waitFor(markPromptEnd); err != nil {
		return "", 0, err
	}
	if c.Setup != "" {
		seen.Reset()
		if _, err := f.WriteString(bracketed(c.Setup) + "\r"); err != nil {
			return "", 0, err
		}
		// Wait for the setup command to *finish* — its D mark — and then
		// for the next prompt. Waiting on the prompt mark alone matches
		// the one already on screen and returns immediately, which sent
		// the real paste while the setup command still owned the
		// terminal: the escape sequences were echoed as text and the
		// case looked like a shell bug rather than a harness bug.
		//
		// waitPast, not waitFor plus Reset. The shell writes the D mark
		// and then renders the next prompt, and on a loaded machine the
		// reader is not scheduled in between, so both land in one read.
		// Clearing the buffer here threw the prompt mark away with the
		// rest, and the second wait then sat for a mark that had already
		// come and gone while the shell idled — reported as "no output
		// for 30s ... saw \"\"" (#195). It failed only under load, which
		// is why every rerun passed.
		if err := waitPast("\x1b]133;D;"); err != nil {
			return "", 0, err
		}
		if err := waitFor(markPromptEnd); err != nil {
			return "", 0, err
		}
	}
	seen.Reset()
	if _, err := f.WriteString(bracketed(c.Text) + "\r"); err != nil {
		return "", 0, err
	}
	if err := waitFor("\x1b]133;D;"); err != nil {
		return "", 0, err
	}
	// Give the tail of the output a moment to arrive; the D mark is
	// written before the shell's own trailing writes are flushed.
	drain(chunks, &seen, 50*time.Millisecond)

	m := outputRe.FindStringSubmatch(seen.String())
	if m == nil {
		return "", 0, errors.New("no command output found between the semantic marks: " + plainText(seen.String()))
	}
	code, _ := strconv.Atoi(m[2])
	return normalizeOutput(m[1]), code, nil
}

// consumeThrough drops the first n bytes of buf, keeping the remainder.
//
// This is what makes a wait non-destructive: everything the shell wrote
// after the awaited marker — commonly the next prompt, coalesced into
// the same read — stays available to the next wait (#195).
func consumeThrough(buf *bytes.Buffer, n int) {
	rest := append([]byte(nil), buf.Bytes()[n:]...)
	buf.Reset()
	buf.Write(rest)
}

func drain(chunks <-chan []byte, seen *bytes.Buffer, d time.Duration) {
	deadline := time.After(d)
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				return
			}
			seen.Write(chunk)
		case <-deadline:
			return
		}
	}
}

// bracketed wraps text in the bracketed-paste sequence, which is how a
// terminal actually delivers a paste — and the reason a pasted `!!` or
// a pasted Tab does not run a keybinding.
func bracketed(text string) string {
	return "\x1b[200~" + text + "\x1b[201~"
}

// normalizeOutput converts the pty's CRLF line endings back to the LF a
// pipe would have carried, strips the escapes the editor wrote, and
// drops the echoed input line — a terminal echoes what was typed, and
// that echo is not output.
func normalizeOutput(raw string) string {
	s := plainText(raw)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimLeft(s, "\n")
}

func plainText(s string) string { return ansiEscapes.ReplaceAllString(s, "") }
