package interp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/blairham/koi-shell/internal/shell/syntax"
)

// tracer prints expressions like a shell would do if its
// options '-o' is set to either 'xtrace' or its shorthand, '-x'.
type tracer struct {
	buf       bytes.Buffer
	printer   *syntax.Printer
	output    io.Writer
	needsPlus bool
	// prefix is PS4, expanded per line. It was a hardcoded "+ ", so
	// `PS4='+ $BASH_SOURCE:$LINENO: '` — the standard debugging recipe
	// — produced nothing but plusses (#413).
	prefix string
}

func (r *Runner) tracer() *tracer {
	if !r.opts[optXTrace] {
		return nil
	}

	out := r.stderr
	// BASH_XTRACEFD sends the trace somewhere other than stderr, which
	// is how a script keeps its trace out of its own error output.
	if fdStr := r.envGet("BASH_XTRACEFD"); fdStr != "" {
		if fd, err := strconv.Atoi(fdStr); err == nil {
			if w := r.fdWriter(fd); w != nil {
				out = w
			}
		}
	}

	return &tracer{
		printer:   syntax.NewPrinter(),
		output:    out,
		needsPlus: true,
		prefix:    r.tracePrefix(),
	}
}

// tracePS4 expands PS4's value the way a prompt string is expanded —
// parameters, command substitutions and arithmetic — without word
// splitting, since the result is one prefix rather than a list.
func (r *Runner) tracePS4(val string) string {
	word, err := syntax.NewParser().Document(strings.NewReader(val))
	if err != nil {
		// An unparsable PS4 is used literally rather than dropped: a
		// broken prefix should not silence the trace.
		return val
	}
	// The expansion must not itself be traced, which would recurse.
	old := r.opts[optXTrace]
	r.opts[optXTrace] = false
	// $LINENO comes from the position of the expansion node, and PS4 is
	// parsed on its own — so line 1 without this. `PS4='+ $LINENO: '`
	// is the recipe the option exists for, and reporting 1 for every
	// line of a script is worse than reporting nothing.
	oldOffset := r.ecfg.LineOffset
	if r.traceLine > 0 {
		r.ecfg.LineOffset = uint64(r.traceLine) - 1
	}
	out := r.document(word)
	r.ecfg.LineOffset = oldOffset
	r.opts[optXTrace] = old
	return out
}

// tracePrefix expands PS4 for one trace line. The first character is
// repeated per nesting level in bash; koi has one level here, so the
// string is used as written.
func (r *Runner) tracePrefix() string {
	vr := r.lookupVar("PS4")
	if !vr.IsSet() {
		return "+ "
	}
	return r.tracePS4(vr.String())
}

// string writes s to tracer.buf if tracer is non-nil,
// prepending "+" if tracer.needsPlus is true.
func (t *tracer) string(s string) {
	if t == nil {
		return
	}

	if t.needsPlus {
		t.buf.WriteString(t.plus())
	}
	t.needsPlus = false
	t.buf.WriteString(s)
}

func (t *tracer) stringf(f string, a ...any) {
	if t == nil {
		return
	}

	t.string(fmt.Sprintf(f, a...))
}

// expr prints x to tracer.buf if tracer is non-nil,
// prepending "+" if tracer.isFirstPrint is true.
func (t *tracer) expr(x syntax.Node) {
	if t == nil {
		return
	}

	if t.needsPlus {
		t.buf.WriteString(t.plus())
	}
	t.needsPlus = false
	if err := t.printer.Print(&t.buf, x); err != nil {
		panic(err)
	}
}

// plus is the line's prefix, defaulting to bash's when PS4 is unset.
func (t *tracer) plus() string {
	if t.prefix == "" {
		return "+ "
	}
	return t.prefix
}

// flush writes the contents of tracer.buf to the tracer.stdout.
func (t *tracer) flush() {
	if t == nil {
		return
	}

	t.output.Write(t.buf.Bytes())
	t.buf.Reset()
}

// newLineFlush is like flush, but with extra new line before tracer.buf gets flushed.
func (t *tracer) newLineFlush() {
	if t == nil {
		return
	}

	t.buf.WriteString("\n")
	t.flush()
	// reset state
	t.needsPlus = true
}

// tracedCall runs a simple command through [Runner.call] bracketed by the
// hook installed with [TraceHook] (#474): position and unexpanded text
// are taken before the call, exit status and duration after it. The argv
// is cloned because a sink may hold the event past this command's life.
func (r *Runner) tracedCall(ctx context.Context, cm *syntax.CallExpr, fields []string) {
	pos := cm.Args[0].Pos()
	var sb strings.Builder
	if err := syntax.NewPrinter().Print(&sb, cm); err != nil {
		sb.Reset() // a node that cannot print still traces, with empty text
	}
	ev := TraceEvent{
		Src:           r.currentSource(),
		Line:          pos.Line(),
		Col:           pos.Col(),
		Cmd:           sb.String(),
		Expanded:      slices.Clone(fields),
		StartedUnixMs: time.Now().UnixMilli(),
	}
	if r.inFunction() {
		ev.Func = r.frames[0].name
	}
	start := time.Now()
	r.call(ctx, pos, fields)
	ev.DurationMs = time.Since(start).Milliseconds()
	ev.Exit = int(r.exit.code)
	r.traceHook(ev)
}

// call prints one traced command: the command word and each expanded
// field, quoted individually.
//
// Individually is the whole point. koi joined the fields into one string
// and quoted *that*, only for builtins, so `echo a b` traced as `echo 'a
// b'` — one field where the command received two — while
// `/bin/echo "a b"` traced as two fields where it received one. A trace
// is what someone re-runs to reproduce a failure, and both of those
// re-run as a different command than the one that ran.
//
// The `set -x` that turns tracing on needs no special case: the tracer is
// built from the option as it stood before the command ran, so there is
// nothing to print from. `set` used to be skipped entirely, which dropped
// every later `set` from the trace and still emitted the line's newline —
// a stray blank line in the middle of a trace (#413).
func (t *tracer) call(cmd string, args ...string) {
	if t == nil {
		return
	}

	var sb strings.Builder
	sb.WriteString(traceQuote(cmd))
	for _, arg := range args {
		sb.WriteByte(' ')
		sb.WriteString(traceQuote(arg))
	}
	t.string(sb.String())
}

// traceQuote quotes one field for a trace line the way bash's
// xtrace_print_word_list does, which is not the same as quoting it to be
// re-parsed: bash reaches for `'...'` where [syntax.Quote] picks the
// shortest form and answers `"it's"`, and it leaves `#`, `,` and `]`
// alone where a general-purpose quoter does not. Measured against bash
// 5.3 byte by byte: the two disagree on five shapes, so this is its own
// function rather than a call into that one.
func traceQuote(s string) string {
	if s == "" {
		return "''"
	}
	// A control byte wins over everything else, even when the field also
	// holds a space: bash reaches for the ANSI-C form first.
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return traceQuoteANSIC(s)
		}
	}
	if !traceNeedsQuote(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// traceNeedsQuote reports whether a field holds anything the shell would
// have read as syntax, which is what decides whether bash quotes it.
func traceNeedsQuote(s string) bool {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case ' ', '\'', '"', '\\', '|', '&', ';', '(', ')', '<', '>',
			'!', '{', '}', '*', '[', '?', ']', '^', '$', '`':
			return true
		case '~':
			// Tilde expansion only happens at the front of a word or
			// just past the `:` or `=` of an assignment, so that is the
			// only place a tilde needs hiding.
			if i == 0 || s[i-1] == ':' || s[i-1] == '=' {
				return true
			}
		case '#':
			// A comment can only start a word, so `a#b` needs nothing.
			if i == 0 {
				return true
			}
		}
	}
	return false
}

// traceQuoteANSIC renders a field as bash's ansic_quote does: the named
// escapes it knows, three-digit octal for any other unprintable byte,
// and `"` and `$` straight through, since `$'...'` expands neither.
func traceQuoteANSIC(s string) string {
	var sb strings.Builder
	sb.WriteString("$'")
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\a':
			sb.WriteString(`\a`)
		case '\b':
			sb.WriteString(`\b`)
		case '\f':
			sb.WriteString(`\f`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		case '\v':
			sb.WriteString(`\v`)
		case 0x1b:
			// bash spells escape `\E`, never `\e`.
			sb.WriteString(`\E`)
		case '\\':
			sb.WriteString(`\\`)
		case '\'':
			sb.WriteString(`\'`)
		default:
			if c < 0x20 || c == 0x7f {
				fmt.Fprintf(&sb, `\%03o`, c)
				continue
			}
			sb.WriteByte(c)
		}
	}
	sb.WriteByte('\'')
	return sb.String()
}
