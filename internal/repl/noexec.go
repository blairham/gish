package repl

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/syntax"
)

// `-n`: read commands but do not execute them (#233).
//
// Every shell has this and koi did not, which is the #217 class again — a
// shell's argv is a contract other *programs* write, and `sh -n` is how
// scripts get lint-checked: pre-commit hooks, CI steps, `make lint` in a
// great many repositories, editor integrations. koi answered `unknown
// option "n"` plus a usage dump, so it could not stand in for `sh` in any
// of those places, and it failed in the most confusing available way — a
// usage error for the *shell* reads as the tool being broken.
//
// The capability already existed; only the option was missing. The
// substrate parses without running, which is exactly the check, and
// `set -n` inside a session already worked.
//
// What is matched here is bash's *observable* contract, not its prose:
// silent and 0 when the input parses, a message on stderr and **exit 2**
// when it does not, and nothing executed either way. The message text
// stays koi's own (`koi: file:line:col: …`) per #120 — koi claims bash's
// interface, not its identity — because what a caller acts on is the exit
// status, and inventing bash's exact wording would be a lie that also has
// to be maintained.

// NoExecStatus is what a failed syntax check exits with. bash uses 2 for
// this, and a tool that branches on the status rather than the text is the
// normal case, so the number is part of the contract.
const NoExecStatus = 2

// CheckCommand parses -c's operand without running it. name is what
// appears in an error, as it is for a real run.
func CheckCommand(src, name string) error {
	return checkSyntax(strings.NewReader(src), name)
}

// CheckFile parses a script without running it.
func CheckFile(path string) error {
	f, err := os.Open(path) //nolint:gosec // the script the caller named
	if err != nil {
		return err
	}
	defer f.Close()
	return checkSyntax(f, path)
}

// CheckStdin parses piped input without running it, which is what
// `cat script | koi -n` and `koi -n < script` mean.
func CheckStdin() error {
	return checkSyntax(os.Stdin, "stdin")
}

// checkSyntax is the whole check: parse, discard the tree.
//
// Deliberately the *same* parser the run path uses rather than a
// second-guess at what koi accepts. A syntax check that disagrees with
// the shell it checks for is worse than none — it would pass scripts that
// then fail to run, which is the failure mode CI uses `-n` to prevent.
func checkSyntax(r io.Reader, name string) error {
	_, err := syntax.NewParser().Parse(r, name)
	return err
}

// ReportSyntaxError prints a check failure the way the run path reports
// one, so the two never diverge in appearance.
func ReportSyntaxError(w io.Writer, err error) {
	fmt.Fprintln(w, "koi:", err)
}
