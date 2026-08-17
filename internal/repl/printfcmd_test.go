package repl

import (
	"slices"
	"testing"
)

// Option parsing is its own unit because the rules are positional and
// easy to get subtly wrong: -v is an option only *before* the format,
// which is why `printf "%s" -v x` prints "-vx" instead of assigning.
func TestParsePrintfArgs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		args     []string
		ok       bool
		assign   bool
		target   string
		operands []string
	}{
		{
			name: "no options", args: []string{"%s", "hi"},
			ok: true, operands: []string{"%s", "hi"},
		},
		{
			name: "separate name", args: []string{"-v", "x", "%s", "hi"},
			ok: true, assign: true, target: "x", operands: []string{"%s", "hi"},
		},
		{
			name: "clustered name", args: []string{"-vx", "%s", "hi"},
			ok: true, assign: true, target: "x", operands: []string{"%s", "hi"},
		},
		{
			// bash takes the last one.
			name: "repeated -v", args: []string{"-v", "x", "-v", "y", "%s", "hi"},
			ok: true, assign: true, target: "y", operands: []string{"%s", "hi"},
		},
		{
			name: "-- ends the options", args: []string{"-v", "x", "--", "%s", "hi"},
			ok: true, assign: true, target: "x", operands: []string{"%s", "hi"},
		},
		{
			// The format may itself start with a dash, which is what --
			// is for; without it the dash-argument is the format.
			name: "-- then a dashed format", args: []string{"--", "-%s", "a"},
			ok: true, operands: []string{"-%s", "a"},
		},
		{
			// Options stop at the format: these are arguments to it.
			name: "-v after the format", args: []string{"%s", "-v", "x"},
			ok: true, operands: []string{"%s", "-v", "x"},
		},
		{
			name: "-- as an operand", args: []string{"-v", "x", "%s", "--"},
			ok: true, assign: true, target: "x", operands: []string{"%s", "--"},
		},
		{
			// An empty name is a usage-shaped call that bash rejects
			// later, as "not a valid identifier" — parsing accepts it.
			name: "empty clustered name", args: []string{"-v", "", "%s"},
			ok: true, assign: true, target: "", operands: []string{"%s"},
		},
		{name: "-v with nothing after it", args: []string{"-v"}, ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parsePrintfArgs(tc.args)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if got.assign != tc.assign || got.target != tc.target {
				t.Errorf("assign=%v target=%q, want assign=%v target=%q",
					got.assign, got.target, tc.assign, tc.target)
			}
			if !slices.Equal(got.operands, tc.operands) {
				t.Errorf("operands = %q, want %q", got.operands, tc.operands)
			}
		})
	}
}

// The name reaches the interpreter unquoted, because a subscript has to
// keep being evaluated the way bash evaluates it. This is the check
// that makes that safe, so it is worth testing as a boundary rather
// than only through the shell.
func TestValidAssignTarget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"x", true},
		{"_x", true},
		{"X9_a", true},
		{"arr[1]", true},
		{"arr[$i]", true},  // bash evaluates it; so do we
		{"arr[1+1]", true}, // arithmetic, likewise
		{"m[k e y]", true}, // an associative key may contain spaces
		{"a[b[1]]", true},  // nesting closes at the last bracket
		{"", false},        // bash: not a valid identifier
		{"1bad", false},    // may not start with a digit
		{"x.y", false},     // not an identifier character
		{"x y", false},     // ditto
		{"x;echo pwned", false},
		{"x[1]; echo pwned", false},
		// The one that balance alone would let through: it starts with
		// an identifier and ends with a bracket, but the subscript is
		// not the whole tail — so bash rejects it, and so must we, or
		// the middle would reach the interpreter as code.
		{"x[1]$(echo pwned)[2]", false},
		{"x[", false},
		{"x]", false},
		{"x[1]extra", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := validAssignTarget(tc.name); got != tc.want {
				t.Errorf("validAssignTarget(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
