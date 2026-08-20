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

const umaskUsage = "usage: umask [-p] [-S] [mode]"

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
			fmt.Fprintf(hc.Stderr, "umask: %s: invalid option\n%s\n", args[0], umaskUsage)
			return interp.ExitStatus(2)
		}
		args = args[1:]
	}

operand:
	if len(args) > 1 {
		fmt.Fprintln(hc.Stderr, umaskUsage)
		return interp.ExitStatus(2)
	}

	if len(args) == 1 {
		// A symbolic mode is relative to the mask already in force, so
		// it has to be read before anything is changed.
		current := syscall.Umask(0)
		syscall.Umask(current)
		mode, err := parseUmask(args[0], current)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "umask: %s: %v\n", args[0], err)
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
func parseUmask(s string, current int) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("octal number expected")
	}
	for _, r := range s {
		if r < '0' || r > '7' {
			return parseSymbolicUmask(s, current)
		}
	}
	v, err := strconv.ParseInt(s, 8, 32)
	if err != nil || v < 0 || v > 0o777 {
		return 0, fmt.Errorf("octal number out of range")
	}
	return int(v), nil
}

// parseSymbolicUmask applies a comma-separated symbolic mode. The
// clauses describe the permissions that are *allowed*, so the work
// happens on the complement of the mask and is inverted at the end —
// which is why `umask =` answers 0777 rather than 0.
func parseSymbolicUmask(s string, current int) (int, error) {
	perm := ^current & 0o777
	for _, clause := range strings.Split(s, ",") {
		if clause == "" {
			return 0, fmt.Errorf("invalid symbolic mode")
		}
		i := strings.IndexAny(clause, "+-=")
		if i < 0 {
			return 0, fmt.Errorf("invalid symbolic mode")
		}
		whoSpec, op, permSpec := clause[:i], clause[i], clause[i+1:]
		mask := 0
		if whoSpec == "" {
			whoSpec = "a"
		}
		for _, w := range whoSpec {
			switch w {
			case 'u':
				mask |= 0o700
			case 'g':
				mask |= 0o070
			case 'o':
				mask |= 0o007
			case 'a':
				mask |= 0o777
			default:
				return 0, fmt.Errorf("invalid symbolic mode")
			}
		}
		bits := 0
		for _, c := range permSpec {
			switch c {
			case 'r':
				bits |= 0o444
			case 'w':
				bits |= 0o222
			case 'x', 'X':
				bits |= 0o111
			default:
				return 0, fmt.Errorf("invalid symbolic mode")
			}
		}
		bits &= mask
		switch op {
		case '+':
			perm |= bits
		case '-':
			perm &^= bits
		case '=':
			perm = perm&^mask | bits
		}
	}
	return ^perm & 0o777, nil
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
