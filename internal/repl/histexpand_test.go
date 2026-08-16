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
