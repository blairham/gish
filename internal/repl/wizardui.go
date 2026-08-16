package repl

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	huh "charm.land/huh/v2"

	"github.com/blairham/gish/internal/ui"
)

// The wizard's question seam (#90). `config theme` on a real terminal
// asks through huh forms; everywhere else — piped stdin, NO_COLOR, a
// dumb terminal, the tests — it asks with the original line Q&A.
//
// Both frontends answer the same questions in the same order and
// produce the same values, so the wizard body below them does not know
// which one is running. That is the point: the walkthrough's logic and
// its persistence path stay in one place, and the styled frontend is
// only a way of asking.
//
// This follows the seam choose.go already established for approvals
// (huh on a terminal, single-rune lines elsewhere), rather than
// inventing a second pattern for the same problem.

// wizardPrompt asks one question at a time. ok=false means the user
// backed out (Ctrl-D, Ctrl-C, Esc) and the wizard aborts, saving
// nothing.
type wizardPrompt interface {
	// selectOne offers a fixed set, defaulting to current.
	selectOne(title, description, current string, options []string) (string, bool)
	// freeText takes an arbitrary value, defaulting to current. validate
	// may reject an answer with a message; the frontend re-asks.
	freeText(title, description, current string, validate func(string) error) (string, bool)
	// confirm is the final save/discard question.
	confirm(title string, def bool) (bool, bool)
	// note prints something informational — a preview, a hint. It is not
	// a question, and it renders identically in both frontends.
	note(text string)
}

// newWizardPrompt picks the frontend. The huh path needs both ends of
// the terminal to be real *and* color-willing, matching every other
// styled surface (ui.Enabled), so NO_COLOR and TERM=dumb keep the plain
// walkthrough rather than a half-styled one.
func newWizardPrompt(in io.Reader, out io.Writer) wizardPrompt {
	f, isFile := in.(*os.File)
	if isFile && ui.Enabled(out) && ui.Enabled(f) {
		return &huhWizard{in: f, out: out}
	}
	return newLineWizard(in, out)
}

// huhWizard is the styled frontend.
type huhWizard struct {
	in  io.Reader
	out io.Writer
}

// run wraps one single-field form. A form error is not distinguished
// from an abort on purpose: every way huh can fail here — Ctrl-C, Esc,
// EOF, a terminal that went away — means the same thing to the wizard,
// which is "do not save".
func (h *huhWizard) run(field huh.Field) bool {
	err := huh.NewForm(huh.NewGroup(field)).WithInput(h.in).WithOutput(h.out).Run()
	return err == nil
}

func (h *huhWizard) selectOne(title, description, current string, options []string) (string, bool) {
	opts := make([]huh.Option[string], len(options))
	for i, o := range options {
		opts[i] = huh.NewOption(o, o)
	}
	// Seeding the bound value is what makes the current setting the
	// highlighted row, so Enter keeps it — the p10k-configure contract
	// the line frontend gets from printing "(default)".
	choice := current
	sel := huh.NewSelect[string]().Title(title).Options(opts...).Value(&choice)
	if description != "" {
		sel = sel.Description(description)
	}
	if !h.run(sel) {
		return "", false
	}
	return choice, true
}

func (h *huhWizard) freeText(title, description, current string, validate func(string) error) (string, bool) {
	value := current
	input := huh.NewInput().Title(title).Value(&value)
	if description != "" {
		input = input.Description(description)
	}
	if validate != nil {
		// huh re-asks in place on a validation error, which is strictly
		// better than the line frontend's print-and-loop.
		input = input.Validate(validate)
	}
	if !h.run(input) {
		return "", false
	}
	if strings.TrimSpace(value) == "" {
		return current, true
	}
	return value, true
}

func (h *huhWizard) confirm(title string, def bool) (bool, bool) {
	value := def
	if !h.run(huh.NewConfirm().Title(title).Value(&value)) {
		return false, false
	}
	return value, true
}

func (h *huhWizard) note(text string) { fmt.Fprintln(h.out, text) }

// lineWizard is the original Q&A, kept as the everywhere-else frontend
// rather than as a deprecated path: scripts, CI, and the tests all read
// it, and it is the only thing that works without a terminal.
type lineWizard struct{ io *wizardIO }

func newLineWizard(in io.Reader, out io.Writer) *lineWizard {
	return &lineWizard{io: newWizardIO(in, out)}
}

func (l *lineWizard) selectOne(title, _, current string, options []string) (string, bool) {
	return l.io.askOneOf(title, current, options)
}

func (l *lineWizard) freeText(title, _, current string, validate func(string) error) (string, bool) {
	for {
		answer, ok := l.io.ask(title, current)
		if !ok {
			return "", false
		}
		if validate == nil {
			return answer, true
		}
		if err := validate(answer); err != nil {
			fmt.Fprintf(l.io.out, "  %v\n", err)
			continue
		}
		return answer, true
	}
}

func (l *lineWizard) confirm(title string, def bool) (bool, bool) {
	defWord := "n"
	if def {
		defWord = "y"
	}
	answer, ok := l.io.askOneOf(title, defWord, []string{"y", "n"})
	if !ok {
		return false, false
	}
	return answer == "y", true
}

func (l *lineWizard) note(text string) { fmt.Fprintln(l.io.out, text) }

// validateSegments is shared by both frontends so a bad segment id is
// rejected the same way in each.
func validateSegments(list string) error {
	ids := strings.Fields(list)
	if len(ids) == 0 {
		return errors.New("give at least one segment id")
	}
	for _, id := range ids {
		if !segmentIDRe.MatchString(id) {
			return fmt.Errorf("bad segment id %q", id)
		}
	}
	return nil
}
