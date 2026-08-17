package builtins

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// printf, implemented natively (#55).
//
// The interpreter ships one, and it covers %s %b %c %d %i %u %o %x with
// flags and width — but it rejects a precision outright ("invalid format
// char: .") and has no %f %e %g %X %q at all. That is seven of the
// fourteen forms a shell script actually uses: `%.2f` is how every
// script prints a number, and `%q` is how a script quotes a value safely
// enough to hand back to a shell. A printf that cannot do either is not
// a printf.
//
// Taking the name over is the ordinary route for a builtin the
// interpreter already claims (the package doc calls it out: such names
// never reach the ExecHandler seam, so interception happens at the
// CallHandler). Nothing else about the substrate is replaced — this is a
// gap fill, not a fork, and the gap is reported upstream (#119).
//
// Formatting delegates to Go's fmt for the numeric and string
// conversions, because the verb grammars agree almost everywhere. The
// places they do not are exactly where this file has code: POSIX reuses
// the format string until the arguments run out, %b expands escapes in
// the *argument*, %c takes one character, %q shell-quotes rather than
// Go-quotes, and a missing argument is an empty string or a zero rather
// than an error.

// Printf writes the POSIX printf(1) rendering of format/args to w.
// A non-nil error is a usage error; output written before it stands,
// which is what bash does.
func Printf(w io.Writer, args []string) error {
	if len(args) == 0 {
		return ErrUsage
	}
	format, rest := args[0], args[1:]

	// POSIX: the format is reused until the arguments are consumed. One
	// pass always happens, even with no arguments at all.
	for first := true; first || len(rest) > 0; first = false {
		consumed, stop, err := printfOnce(w, format, rest)
		if err != nil {
			return err
		}
		if stop {
			return nil // \c in a %b argument ends all output
		}
		if consumed == 0 {
			// A format with no conversions would otherwise repeat for
			// ever against a non-empty argument list.
			return nil
		}
		// consumed can exceed what is left: a format with more
		// conversions than arguments fills the rest with empties and
		// still counts them, which is what makes the reuse loop
		// terminate. Clamping here rather than there keeps that counting
		// honest.
		if consumed >= len(rest) {
			return nil
		}
		rest = rest[consumed:]
	}
	return nil
}

// printfOnce renders the format once, reporting how many arguments it
// used and whether a \c asked for output to stop entirely.
func printfOnce(w io.Writer, format string, args []string) (consumed int, stop bool, err error) {
	var sb strings.Builder
	next := func() string {
		if consumed < len(args) {
			v := args[consumed]
			consumed++
			return v
		}
		consumed++ // still counts, so the reuse loop terminates
		return ""
	}

	for i := 0; i < len(format); i++ {
		c := format[i]
		if c == '\\' {
			text, width, done := unescape(format[i:])
			if done {
				sb.WriteString(text)
				_, werr := io.WriteString(w, sb.String())
				return consumed, true, wrapWrite(werr)
			}
			sb.WriteString(text)
			i += width - 1
			continue
		}
		if c != '%' {
			sb.WriteByte(c)
			continue
		}
		spec, verb, width := parseSpec(format[i:])
		if width == 0 {
			return consumed, false, fmt.Errorf("printf: `%s': invalid format", format[i:])
		}
		i += width - 1
		if verb == '%' {
			sb.WriteByte('%')
			continue
		}
		// A `*` takes its value from the arguments, in order.
		for strings.Contains(spec, "*") {
			n, _ := strconv.Atoi(strings.TrimSpace(next()))
			spec = strings.Replace(spec, "*", strconv.Itoa(n), 1)
		}
		if err := writeVerb(&sb, spec, verb, next); err != nil {
			if errors.Is(err, errStopOutput) {
				// \c: what has been built still prints, nothing more does.
				_, werr := io.WriteString(w, sb.String())
				return consumed, true, wrapWrite(werr)
			}
			return consumed, false, err
		}
	}
	_, werr := io.WriteString(w, sb.String())
	return consumed, false, wrapWrite(werr)
}

// writeVerb renders one conversion.
func writeVerb(sb *strings.Builder, spec string, verb byte, next func() string) error {
	switch verb {
	case 'c':
		// One character, not a string: width still applies.
		arg := next()
		r := ""
		for _, ch := range arg {
			r = string(ch)
			break
		}
		fmt.Fprintf(sb, spec+"s", r)
	case 's':
		fmt.Fprintf(sb, spec+"s", next())
	case 'b':
		// The argument's own escapes are expanded, and its \c stops
		// everything. Precision applies to the expanded text.
		text, stopped := expandEscapes(next())
		fmt.Fprintf(sb, spec+"s", text)
		if stopped {
			return errStopOutput
		}
	case 'q':
		// Shell quoting, not Go quoting: the point of %q is that the
		// result can be pasted back into a shell.
		fmt.Fprintf(sb, spec+"s", shellQuote(next()))
	case 'd', 'i':
		fmt.Fprintf(sb, spec+"d", parseInt(next()))
	case 'u':
		fmt.Fprintf(sb, spec+"d", uint64(parseInt(next())))
	case 'o', 'x', 'X':
		fmt.Fprintf(sb, spec+string(verb), uint64(parseInt(next())))
	case 'e', 'E', 'f', 'F', 'g', 'G':
		fmt.Fprintf(sb, spec+string(verb), parseFloat(next()))
	case 'a', 'A':
		// Go spells hex floats x/X; the C name is a/A.
		v := map[byte]string{'a': "x", 'A': "X"}[verb]
		fmt.Fprintf(sb, spec+v, parseFloat(next()))
	default:
		return fmt.Errorf("printf: `%%%c': invalid format character", verb)
	}
	return nil
}

// errStopOutput is \c inside a %b argument: stop, without an error.
var errStopOutput = errors.New("printf: output stopped")

// ErrWrite marks a failure to write the output, as opposed to a problem
// with the format. The two need opposite handling: a bad format is the
// user's mistake and deserves a message, while a failed write almost
// always means the reader went away — `printf x | head -1` — and bash
// says nothing there because printf simply dies of SIGPIPE.
//
// Reporting it instead produced a diagnostic on stderr for an ordinary
// pipeline, and, because the failing write happens on the pipeline's own
// goroutine, a data race against whatever else was writing stderr.
var ErrWrite = errors.New("printf: write failed")

// ErrUsage marks a call with no format at all. It is its own sentinel
// because bash answers usage with status 2 rather than the 1 a bad
// format gets, and the caller is the only side that can set a status.
//
// The wording names -v even though this package does not implement it:
// the assignment belongs to the shell (internal/repl/printfcmd.go), but
// the usage line the user sees has to describe the printf they actually
// have.
var ErrUsage = errors.New("printf: usage: printf [-v var] format [arguments]")

// wrapWrite tags a write failure so callers can stay quiet about it.
func wrapWrite(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrWrite, err)
}

// parseSpec splits a conversion at the head of s into its flag/width/
// precision prefix and its verb, returning how many bytes it spans.
// A width of 0 means s does not begin a valid conversion.
func parseSpec(s string) (spec string, verb byte, width int) {
	i := 1 // s[0] is '%'
	for i < len(s) && strings.IndexByte("-+ #0'", s[i]) >= 0 {
		i++
	}
	for i < len(s) && (s[i] == '*' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && (s[i] == '*' || (s[i] >= '0' && s[i] <= '9')) {
			i++
		}
	}
	if i >= len(s) {
		return "", 0, 0
	}
	// `'` is a POSIX grouping flag Go does not know; drop it rather than
	// hand fmt something it will render as an error string.
	return "%" + strings.ReplaceAll(s[1:i], "'", ""), s[i], i + 1
}

// parseInt reads a shell numeric argument. bash accepts 0x/0 prefixes
// and treats junk as zero rather than failing the whole format.
func parseInt(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if v, err := strconv.ParseInt(s, 0, 64); err == nil {
		return v
	}
	if v, err := strconv.ParseUint(s, 0, 64); err == nil {
		return int64(v)
	}
	return 0
}

func parseFloat(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// expandEscapes processes backslash escapes in a %b argument, reporting
// whether a \c asked for output to stop.
func expandEscapes(s string) (string, bool) {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			sb.WriteByte(s[i])
			continue
		}
		text, width, stop := unescape(s[i:])
		if stop {
			sb.WriteString(text)
			return sb.String(), true
		}
		sb.WriteString(text)
		i += width - 1
	}
	return sb.String(), false
}

// unescape decodes the escape sequence at the head of s (which starts
// with a backslash), returning its text, how many bytes it spans, and
// whether it was \c — "stop producing output".
//
// An unknown escape keeps its backslash, which is what both bash and the
// interpreter do: printf '\q' prints \q rather than q.
func unescape(s string) (text string, width int, stop bool) {
	if len(s) < 2 {
		return `\`, 1, false
	}
	switch s[1] {
	case 'a':
		return "\a", 2, false
	case 'b':
		return "\b", 2, false
	case 'c':
		return "", 2, true
	case 'e', 'E':
		return "\x1b", 2, false
	case 'f':
		return "\f", 2, false
	case 'n':
		return "\n", 2, false
	case 'r':
		return "\r", 2, false
	case 't':
		return "\t", 2, false
	case 'v':
		return "\v", 2, false
	case '\\':
		return `\`, 2, false
	case '"':
		return `"`, 2, false
	case '\'':
		return "'", 2, false
	case '0', '1', '2', '3', '4', '5', '6', '7':
		// Octal comes in two spellings and both are used in the wild:
		// \0NNN (POSIX printf's format string) and \NNN (what people
		// actually type, and what bash accepts). Handling only the first
		// left `printf 'oct:\101'` printing the digits back.
		start := 1
		if s[1] == '0' {
			start = 2 // the leading zero is a prefix, not a digit
		}
		n := start
		for n < len(s) && n < start+3 && s[n] >= '0' && s[n] <= '7' {
			n++
		}
		if n == start {
			return "\x00", 2, false // bare \0 is NUL
		}
		v, _ := strconv.ParseUint(s[start:n], 8, 32)
		return string(rune(v)), n, false
	case 'x':
		n := 2
		for n < len(s) && n < 4 && isHex(s[n]) {
			n++
		}
		if n == 2 {
			return `\x`, 2, false
		}
		v, _ := strconv.ParseUint(s[2:n], 16, 32)
		return string(rune(v)), n, false
	case 'u', 'U':
		limit := 6
		if s[1] == 'U' {
			limit = 10
		}
		n := 2
		for n < len(s) && n < limit && isHex(s[n]) {
			n++
		}
		if n == 2 {
			return s[:2], 2, false
		}
		v, _ := strconv.ParseUint(s[2:n], 16, 32)
		return string(rune(v)), n, false
	}
	return s[:2], 2, false
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// shellQuote renders s so a shell reads it back as one word — bash's %q.
//
// The output style is bash's, not the prettiest available, because %q
// exists so its result can be pasted back and compared: backslash-escape
// each special character (`a b` becomes `a\ b`), switch to ANSI-C
// quoting only when a control character makes backslashes impossible
// (a newline becomes $'\n'), and spell the empty string `”` since
// nothing at all would simply vanish.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		var sb strings.Builder
		sb.WriteString("$'")
		for _, r := range s {
			switch r {
			case '\n':
				sb.WriteString(`\n`)
			case '\t':
				sb.WriteString(`\t`)
			case '\r':
				sb.WriteString(`\r`)
			case '\a':
				sb.WriteString(`\a`)
			case '\b':
				sb.WriteString(`\b`)
			case '\f':
				sb.WriteString(`\f`)
			case '\v':
				sb.WriteString(`\v`)
			case '\\', '\'':
				sb.WriteByte('\\')
				sb.WriteRune(r)
			default:
				if r < 0x20 || r == 0x7f {
					fmt.Fprintf(&sb, `\%03o`, r)
				} else {
					sb.WriteRune(r)
				}
			}
		}
		sb.WriteString("'")
		return sb.String()
	}

	const special = " \t\n\"'\\$`&;()|<>*?[]{}!#~^=%"
	if !strings.ContainsAny(s, special) {
		return s
	}
	var sb strings.Builder
	for _, r := range s {
		if r < 0x80 && strings.ContainsRune(special, r) {
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}
