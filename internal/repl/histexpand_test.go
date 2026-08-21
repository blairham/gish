package repl

import (
	"strings"
	"testing"
)

// fakeHistory serves canned entries: index 0 is the most recent, and
// a prefix filters like the real store's Match.
func fakeHistory(entries ...string) func(string, int) (string, bool) {
	return func(prefix string, n int) (string, bool) {
		for _, e := range entries {
			if strings.HasPrefix(e, prefix) {
				if n == 0 {
					return e, true
				}
				n--
			}
		}
		return "", false
	}
}

func TestExpandHistoryDesignators(t *testing.T) {
	t.Parallel()

	hist := fakeHistory("git commit -m draft ./notes.md", "make test ./pkg")
	tests := []struct{ name, in, want string }{
		{"bang-bang", "!!", "git commit -m draft ./notes.md"},
		{"bang-bang mid-line", "sudo !!", "sudo git commit -m draft ./notes.md"},
		{"last arg", "vim !$", "vim ./notes.md"},
		{"first arg", "which !^", "which commit"},
		{"word index", "echo !:2", "echo -m"},
		{"word range", "echo !:1-2", "echo commit -m"},
		{"prefix match", "!make", "make test ./pkg"},
		{"prefix mid-line", "time !make", "time make test ./pkg"},
		{"bang-bang word designator", "echo !!:0", "echo git"},
	}
	for _, tt := range tests {
		got, changed, err := expandHistory(tt.in, hist)
		if err != nil || !changed || got != tt.want {
			t.Errorf("%s: expandHistory(%q) = %q, %v, %v — want %q", tt.name, tt.in, got, changed, err, tt.want)
		}
	}
}

func TestExpandHistoryLeavesNonEventsAlone(t *testing.T) {
	t.Parallel()

	hist := fakeHistory("make test")
	// Negation, !=, !(, single-quoted text, and escapes are not events.
	for _, in := range []string{
		"if ! make test; then echo no; fi",
		`[ "$x" != y ] && echo differ`,
		`echo 'literal !! here'`,
		`echo "hi\!"`,
		"echo no bangs at all",
	} {
		got, changed, err := expandHistory(in, hist)
		if err != nil || changed || got != in {
			t.Errorf("expandHistory(%q) = %q, %v, %v — want untouched", in, got, changed, err)
		}
	}
}

func TestExpandHistoryCaretSubstitution(t *testing.T) {
	t.Parallel()

	hist := fakeHistory("make tset")
	got, changed, err := expandHistory("^tset^test", hist)
	if err != nil || !changed || got != "make test" {
		t.Errorf("caret = %q, %v, %v", got, changed, err)
	}
	// A miss is an error, not a silent pass-through.
	if _, _, err = expandHistory("^nope^x", hist); err == nil {
		t.Error("failed substitution should error")
	}
}

func TestExpandHistoryEventNotFound(t *testing.T) {
	t.Parallel()

	empty := fakeHistory()
	if _, _, err := expandHistory("!!", empty); err == nil {
		t.Error("!! with empty history should error")
	}
	hist := fakeHistory("make test")
	if _, _, err := expandHistory("!nosuchprefix", hist); err == nil {
		t.Error("unmatched prefix should error")
	}
	if _, _, err := expandHistory("echo !:9", hist); err == nil {
		t.Error("out-of-range word designator should error")
	}
}

// The word designators and modifiers, every expectation measured
// against bash 5.3 with the same event (#277). The history here is one
// entry so that `!!` and the designators read off a known command.
func TestExpandHistorySelectors(t *testing.T) {
	t.Parallel()

	hist := fakeHistory("echo /a/b.txt one two")
	for _, tt := range []struct{ name, in, want string }{
		{"command word", "!!:0", "echo"},
		{"first argument", "!!:^", "/a/b.txt"},
		{"last word", "!!:$", "two"},
		{"every argument", "!!:*", "/a/b.txt one two"},
		{"range", "!!:1-2", "/a/b.txt one"},
		{"from n to last", "!!:2*", "one two"},
		{"from n to second last", "!!:1-", "/a/b.txt one"},
		// Modifiers apply to the whole event when no word is selected,
		// which is why :h here trims the last /component of the *line*.
		{"head", "!!:h", "echo /a"},
		{"tail", "!!:t", "b.txt one two"},
		{"head of a word", "!!:1:h", "/a"},
		{"tail of a word", "!!:1:t", "b.txt"},
		{"root of a word", "!!:1:r", "/a/b"},
		{"extension of a word", "!!:1:e", ".txt"},
		{"chained", "!!:1:h:t", "a"},
		{"quote", "!!:q", "'echo /a/b.txt one two'"},
		{"substitute", "!!:s/one/ONE/", "echo /a/b.txt ONE two"},
		{"substitute everywhere", "!!:gs/o/0/", "ech0 /a/b.txt 0ne tw0"},
		// The modifiers reach every event form, not just `!!`.
		{"after last arg", "!$:r", "two"},
		{"after a prefix match", "!echo:$", "two"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, changed, err := expandHistory(tt.in, hist)
			switch {
			case err != nil:
				t.Fatalf("%q: %v", tt.in, err)
			case !changed:
				t.Fatalf("%q was not expanded", tt.in)
			case got != tt.want:
				t.Errorf("%q expanded to %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// A designator bash cannot read is a *failed line*, not text to pass
// through: `!!:z` used to run a command with a literal `:z` on the end,
// which is a different command than the one asked for.
func TestExpandHistoryRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	hist := fakeHistory("echo one two")
	for _, in := range []string{
		"!!:z",     // not a modifier
		"!!:",      // nothing after the colon
		"!!:9",     // no such word
		"!!:1-5",   // range past the end
		"!!:3-",    // no word 3
		"!!:3*",    // same, in the other spelling
		"!!:1:zzz", // a bad modifier after a good designator
	} {
		if got, _, err := expandHistory(in, hist); err == nil {
			t.Errorf("%q expanded to %q, want a failed expansion", in, got)
		}
	}

	// `:n-` stops one short of the last word, so naming the last word
	// is empty rather than an error — measured, and the boundary the
	// error cases above sit either side of.
	if got, _, err := expandHistory("x !!:2-", hist); err != nil || got != "x " {
		t.Errorf(`"x !!:2-" gave %q, %v; want "x ", nil`, got, err)
	}

	// `:*` on a command with no arguments is the one empty answer that
	// is not an error, and `:$` on the same command is its only word.
	single := fakeHistory("echo")
	if got, _, err := expandHistory("x !!:*", single); err != nil || got != "x " {
		t.Errorf(`"x !!:*" gave %q, %v; want "x ", nil`, got, err)
	}
	if got, _, err := expandHistory("!!:$", single); err != nil || got != "echo" {
		t.Errorf(`"!!:$" gave %q, %v; want "echo", nil`, got, err)
	}
}

// `:p` asks for the expansion to be shown rather than run, which is the
// only modifier that is not about the text.
func TestExpandHistoryPrintOnly(t *testing.T) {
	t.Parallel()

	hist := fakeHistory("rm -rf ./build")
	got, changed, printOnly, err := expandHistoryLine("!!:p", hist)
	switch {
	case err != nil:
		t.Fatal(err)
	case !changed || got != "rm -rf ./build":
		t.Fatalf("expanded to %q (changed=%t)", got, changed)
	case !printOnly:
		t.Error("`:p` did not ask for the line to be printed rather than run")
	}
	if _, _, printOnly, _ := expandHistoryLine("!!", hist); printOnly {
		t.Error("a plain !! asked for print-only")
	}
}
