//go:build unix

package builtins

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"syscall"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// umask (#55).
//
// Recognized by the interpreter and answered "unsupported builtin", and
// because a claimed name never reaches the exec seam it also shadowed
// the machine's own umask. Scripts use it for a reason that matters:
// `umask 077` before writing a key or a token is how they keep the file
// from being world-readable, and a shell where that line silently fails
// writes the secret at whatever the inherited mask happens to be.
//
// There is no way to read the mask without setting it — the syscall only
// swaps — so reading is a swap-and-swap-back. The window between the two
// is real but unavoidable, and it is the same thing every other shell
// does.

// bash prefixes its usage line with the builtin's name, which koi's
// ulimit already did and its umask did not.
const umaskUsage = "umask: usage: umask [-p] [-S] [mode]"

// Umask reports or sets the file mode creation mask.
func Umask(_ context.Context, hc interp.HandlerContext, args []string) error {
	var symbolic, prefixed bool
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		switch args[0] {
		case "-S":
			symbolic = true
		case "-p":
			prefixed = true
		case "--":
			args = args[1:]
			goto operand
		default:
			hc.Errf("umask: %s: invalid option\n", args[0])
			hc.RawErrf("%s\n", umaskUsage)
			return interp.ExitStatus(2)
		}
		args = args[1:]
	}

operand:
	if len(args) > 1 {
		// A usage line standing on its own is not a diagnostic and
		// carries no location, the way bare `unalias` prints one (#611).
		hc.RawErrf("%s\n", umaskUsage)
		return interp.ExitStatus(2)
	}

	if len(args) == 1 {
		// A symbolic mode is relative to the mask already in force, so
		// it has to be read before anything is changed.
		current := syscall.Umask(0)
		syscall.Umask(current)
		mode, err := parseUmask(args[0], current)
		if err != nil {
			// The error carries its whole body, because bash words the
			// two families differently: the octal one names the mode
			// (`999: octal number out of range`) and the symbolic one
			// names the byte it stopped at instead.
			hc.Errf("umask: %v\n", err)
			return interp.ExitStatus(1)
		}
		syscall.Umask(mode)
		// -S echoes the new mask back, which is how a script records
		// what it just set. -p does not, measured: its job is to make
		// the *query* re-runnable.
		if symbolic {
			fmt.Fprintln(hc.Stdout, symbolicUmask(mode))
		}
		return nil
	}

	current := syscall.Umask(0)
	syscall.Umask(current)

	switch {
	case symbolic && prefixed:
		// -p -S is the re-runnable form of the symbolic listing, which
		// is what a script saves (#533). koi printed the mode alone,
		// so what it saved could not be replayed.
		fmt.Fprintf(hc.Stdout, "umask -S %s\n", symbolicUmask(current))
	case symbolic:
		fmt.Fprintln(hc.Stdout, symbolicUmask(current))
	case prefixed:
		fmt.Fprintf(hc.Stdout, "umask %04o\n", current)
	default:
		fmt.Fprintf(hc.Stdout, "%04o\n", current)
	}
	return nil
}

// parseUmask reads an octal mode, or a symbolic one against the mask
// already in force (#411) — `umask u=rwx,g=rwx,o=rx` is how a script
// states what it wants to *allow*, and koi refused it outright.
//
// Which of the two it is comes down to the first byte, which is bash's
// rule and not a guess: `9x` is `9x: octal number out of range` there
// rather than a symbolic mode, and koi called it an invalid symbolic
// mode because it only took the octal path when every byte was a digit.
func parseUmask(s string, current int) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("octal number expected")
	}
	if s[0] < '0' || s[0] > '9' {
		return parseSymbolicUmask(s, current)
	}
	v, err := strconv.ParseInt(s, 8, 32)
	// The ceiling is 07777 rather than 0777: bash accepts `umask 1000`
	// and reads it back as `1000`, so the setuid, setgid and sticky bits
	// are part of the mode a script may set. Measured.
	if err != nil || v < 0 || v > 0o7777 {
		return 0, fmt.Errorf("%s: octal number out of range", s)
	}
	return int(v), nil
}

// parseSymbolicUmask applies a symbolic mode. The clauses describe the
// permissions that are *allowed*, so the work happens on the complement
// of the mask and is inverted at the end — which is why `umask =`
// answers 0777 rather than 0.
//
// The grammar is `[ugoa]* (op [rwxXst ugo]*)+ (, ...)*`, and the two
// pieces koi was missing are exactly the ones bash's own builtins8.sub
// is made of. **A clause may carry more than one action**: `u=r+w` is
// `u`, then `=r`, then `+w`, and koi cut the clause at the first
// operator and read `r+w` as a permission list, so seven of that file's
// eighteen cases came back "invalid symbolic mode". **A permission may
// be a who letter**, meaning "whatever that one has right now" — `o=u`,
// `g+u`, `o+ru` — and it mixes freely with `rwx`, all measured.
//
// The scan is one pass over the whole string rather than a split on
// commas, because that is the only way to report the byte bash reports:
// a leading comma is `,': invalid symbolic mode operator` there, which a
// split would have turned into an empty first clause.
func parseSymbolicUmask(s string, current int) (int, error) {
	perm := ^current & 0o777
	// copyOf answers "what this who is allowed right now", spread across
	// all three positions so the caller's who-mask can pick out the ones
	// it wants — chmod's `g+u` rule.
	copyOf := func(shift uint) int {
		v := (perm >> shift) & 7
		return v | v<<3 | v<<6
	}
	for i := 0; i < len(s); {
		who := 0
		for ; i < len(s); i++ {
			switch s[i] {
			case 'u':
				who |= 0o700
			case 'g':
				who |= 0o070
			case 'o':
				who |= 0o007
			case 'a':
				who |= 0o777
			default:
				goto actions
			}
		}
	actions:
		if who == 0 {
			// No who at all means all of them, so `+x` is `a+x`.
			who = 0o777
		}
		for first := true; ; first = false {
			if i >= len(s) {
				if first {
					return 0, badUmaskOperator(s, i)
				}
				return ^perm & 0o777, nil
			}
			op := s[i]
			if op != '+' && op != '-' && op != '=' {
				return 0, badUmaskOperator(s, i)
			}
			i++
			bits := 0
		permList:
			for ; i < len(s); i++ {
				switch c := s[i]; c {
				case 'r':
					bits |= 0o444
				case 'w':
					bits |= 0o222
				case 'x':
					bits |= 0o111
				case 'X':
					// Conditional execute: only when something is
					// already executable, since there is no file here to
					// ask about. `umask 777; umask a+X` is a no-op.
					if perm&0o111 != 0 {
						bits |= 0o111
					}
				case 's', 't':
					// setuid, setgid and sticky are accepted and do not
					// reach the nine bits `umask -S` prints; koi refused
					// them outright.
				case 'u':
					bits |= copyOf(6)
				case 'g':
					bits |= copyOf(3)
				case 'o':
					bits |= copyOf(0)
				case '+', '-', '=', ',':
					break permList
				default:
					return 0, fmt.Errorf("`%c': invalid symbolic mode character", c)
				}
			}
			bits &= who
			switch op {
			case '+':
				perm |= bits
			case '-':
				perm &^= bits
			case '=':
				perm = perm&^who | bits
			}
			if i < len(s) && s[i] == ',' {
				i++
				if i >= len(s) {
					// A trailing comma promises another clause, so bash
					// reports the byte after it — the terminator.
					return 0, badUmaskOperator(s, i)
				}
				break
			}
		}
	}
	return ^perm & 0o777, nil
}

// badUmaskOperator words bash's complaint about the byte where an
// operator should have been. Running off the end of the string is the
// odd one: bash reads the terminator and prints it, so the message
// really does carry a NUL between its backquotes — matched rather than
// tidied, because this text is what a caller diffs.
func badUmaskOperator(s string, i int) error {
	c := byte(0)
	if i < len(s) {
		c = s[i]
	}
	return fmt.Errorf("`%c': invalid symbolic mode operator", c)
}

// symbolicUmask renders the mask the way `umask -S` does: the
// permissions that remain, not the ones masked off.
func symbolicUmask(mask int) string {
	who := []struct {
		name  string
		shift uint
	}{{"u", 6}, {"g", 3}, {"o", 0}}
	parts := make([]string, 0, 3)
	for _, w := range who {
		bits := (^mask >> w.shift) & 7
		var perm strings.Builder
		if bits&4 != 0 {
			perm.WriteByte('r')
		}
		if bits&2 != 0 {
			perm.WriteByte('w')
		}
		if bits&1 != 0 {
			perm.WriteByte('x')
		}
		parts = append(parts, w.name+"="+perm.String())
	}
	return strings.Join(parts, ",")
}
