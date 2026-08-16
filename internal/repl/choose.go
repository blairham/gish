package repl

import (
	"io"
	"os"

	huh "charm.land/huh/v2"

	"github.com/blairham/gish/internal/ui"
)

// The shared approval seam (#90): agent gates and the trust review ask
// through a chooser — a huh select on an interactive terminal, plain
// line input elsewhere. Both frontends carry the same options and the
// same single-rune keys, so muscle memory and scripts agree.

// chooseOption is one answer: the stable single-rune key (the line
// protocol and the tests speak it) and the human label the select
// shows.
type chooseOption struct {
	key, label string
}

// chooser resolves one prompt to an option key; ok=false means the
// user backed out (EOF, Ctrl-C).
type chooser func(prompt string, options []chooseOption) (string, bool)

// huhChooser builds the interactive frontend over the given terminal.
func huhChooser(in io.Reader, out io.Writer) chooser {
	return func(prompt string, options []chooseOption) (string, bool) {
		opts := make([]huh.Option[string], len(options))
		for i, o := range options {
			opts[i] = huh.NewOption(o.label, o.key)
		}
		var choice string
		form := huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().Title(prompt).Options(opts...).Value(&choice),
		)).WithInput(in).WithOutput(out)
		if err := form.Run(); err != nil {
			return "", false
		}
		return choice, true
	}
}

// interactiveChooser returns the huh frontend when both sides of the
// terminal are real, nil otherwise (callers fall back to line input).
func interactiveChooser(in io.Reader, out io.Writer) chooser {
	fin, finOK := in.(*os.File)
	if !finOK || !ui.Enabled(out) || !ui.Enabled(fin) {
		return nil
	}
	return huhChooser(in, out)
}
