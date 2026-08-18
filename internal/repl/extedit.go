package repl

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// Ctrl-X Ctrl-E (#96): edit the current line in $EDITOR. The editor
// has already ceded the terminal (cooked mode, decoder stopped), so
// the child owns stdin/stdout for real — vim and friends work.

// externalEditFn returns the editor's ExternalEdit hook, resolving the
// editor from the session's VISUAL/EDITOR, then the environment.
func externalEditFn(runner *interp.Runner) func(string) (string, bool) {
	return func(text string) (string, bool) {
		editor := firstNonEmpty(
			shellVar(runner, "VISUAL", ""), shellVar(runner, "EDITOR", ""),
			os.Getenv("VISUAL"), os.Getenv("EDITOR"),
		)
		if editor == "" {
			fmt.Fprintln(os.Stderr, "koi: set $EDITOR to edit the command line")
			return "", false
		}
		f, err := os.CreateTemp("", "koi-edit-*.sh")
		if err != nil {
			fmt.Fprintln(os.Stderr, "koi:", err)
			return "", false
		}
		path := f.Name()
		defer os.Remove(path) //nolint:errcheck // temp file
		if _, err := f.WriteString(text); err != nil {
			f.Close()
			fmt.Fprintln(os.Stderr, "koi:", err)
			return "", false
		}
		f.Close()

		// The editor command may carry flags ("code -w"); split it.
		fields := strings.Fields(editor)
		cmd := exec.Command(fields[0], append(fields[1:], path)...) //nolint:gosec // the user's own $EDITOR
		cmd.Dir = runner.Dir
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "koi: %s: %v\n", filepath.Base(fields[0]), err)
			return "", false
		}
		edited, err := os.ReadFile(path) //nolint:gosec // our own temp file
		if err != nil {
			fmt.Fprintln(os.Stderr, "koi:", err)
			return "", false
		}
		return strings.TrimRight(string(edited), "\n"), true
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
