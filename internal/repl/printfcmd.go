package repl

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

// A local fix for one substrate gap (#119): `printf "%05.2f"`.
//
// The interpreter's printf rejects a combined width.precision on a
// float — `invalid format char: .` — and that is not a curiosity. It is
// how every script that formats a duration, a percentage or a size
// writes it, and the failure is loud and immediate.
//
// Of the four gaps the scoreboard found, this is the only one with a
// seam: printf is reached through the CallHandler, so gish can answer
// the cases the interpreter refuses and hand everything else back. The
// other three (`${#assoc[@]}`, `${var/#pat}`, `exec 3>&1`) live inside
// expansion and redirection, where there is nothing to intercept — they
// are documented in docs/compat.md and reproduced in
// internal/compat/substrate_test.go so a fix upstream is noticed.
//
// The rule for a local fix like this: take the case the interpreter
// *errors* on, never the case it handles differently. A second printf
// with its own idea of the spec is how a shell ends up with two
// behaviors that drift.

func printfCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if len(args) < 2 || args[0] != "printf" || !needsLocalPrintf(args[1]) {
			return next(ctx, args)
		}
		hc := interp.HandlerCtx(ctx)
		if err := runPrintf(hc.Stdout, args[1], args[2:]); err != nil {
			fmt.Fprintln(hc.Stderr, "printf:", err)
			return []string{"false"}, nil
		}
		return []string{"true"}, nil
	}
}

// needsLocalPrintf reports whether the format uses a spec the
// interpreter refuses: a precision on a float conversion.
func needsLocalPrintf(format string) bool {
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		j := i + 1
		if j < len(format) && format[j] == '%' {
			i = j
			continue
		}
		sawDot := false
		for ; j < len(format); j++ {
			c := format[j]
			if c == '.' {
				sawDot = true
				continue
			}
			if c >= '0' && c <= '9' || c == '-' || c == '+' || c == ' ' || c == '#' {
				continue
			}
			break
		}
		if j < len(format) && sawDot && strings.ContainsRune("feEgG", rune(format[j])) {
			return true
		}
		i = j
	}
	return false
}

// runPrintf implements the subset needed: the C conversions bash
// supports, with arguments recycled until they run out, as printf does.
func runPrintf(w io.Writer, format string, args []string) error {
	next := func() string {
		if len(args) == 0 {
			return ""
		}
		v := args[0]
		args = args[1:]
		return v
	}

	for round := 0; ; round++ {
		if err := printfOnce(w, format, next); err != nil {
			return err
		}
		// printf reuses the format until the arguments are exhausted —
		// `printf '%s\n' a b c` prints three lines. Stopping after the
		// first round is the bug that makes that print one.
		if len(args) == 0 {
			return nil
		}
		if round > 10000 {
			return nil // a format with no conversions would never consume
		}
	}
}

func printfOnce(w io.Writer, format string, next func() string) error {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c == '\\' && i+1 < len(format) {
			i++
			b.WriteString(unescapeChar(format[i]))
			continue
		}
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		if i+1 < len(format) && format[i+1] == '%' {
			b.WriteByte('%')
			i++
			continue
		}
		spec := "%"
		i++
		for ; i < len(format); i++ {
			c := format[i]
			spec += string(c)
			if strings.ContainsRune("diouxXfeEgGcsq", rune(c)) {
				break
			}
		}
		if i >= len(format) {
			b.WriteString(spec)
			break
		}
		value := next()
		switch spec[len(spec)-1] {
		case 'd', 'i':
			n, _ := strconv.ParseInt(strings.TrimSpace(value), 0, 64)
			fmt.Fprintf(&b, strings.TrimSuffix(spec, "i")+"d", n)
		case 'o', 'u', 'x', 'X':
			n, _ := strconv.ParseInt(strings.TrimSpace(value), 0, 64)
			verb := spec
			if verb[len(verb)-1] == 'u' {
				verb = verb[:len(verb)-1] + "d"
			}
			fmt.Fprintf(&b, verb, n)
		case 'f', 'e', 'E', 'g', 'G':
			f, _ := strconv.ParseFloat(strings.TrimSpace(value), 64)
			fmt.Fprintf(&b, spec, f)
		case 'c':
			if value != "" {
				b.WriteString(value[:1])
			}
		case 'q':
			fmt.Fprintf(&b, "%q", value)
		default: // s
			fmt.Fprintf(&b, spec, value)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func unescapeChar(c byte) string {
	switch c {
	case 'n':
		return "\n"
	case 't':
		return "\t"
	case 'r':
		return "\r"
	case '\\':
		return "\\"
	case 'a':
		return "\a"
	case 'b':
		return "\b"
	case 'f':
		return "\f"
	case 'v':
		return "\v"
	case '0':
		return "\x00"
	default:
		return "\\" + string(c)
	}
}
