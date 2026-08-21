package interp

import (
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/syntax"
)

// The canonical printer's own contract (#386): whatever shape it picks,
// the output has to parse back. Comparing text against bash is what the
// table cases in interp_test.go do; this asserts the property that
// makes the shape usable at all, and it is not hypothetical — printing
// a semicolon after a background statement produced output koi's own
// parser rejected.
func TestPrintedFunctionReparses(t *testing.T) {
	bodies := []string{
		`echo hi`,
		`echo bg >/dev/null & echo after`,
		`if [ 1 ]; then echo a; else echo b; fi`,
		`for i in 1 2; do echo $i; done`,
		`for ((i=0;i<2;i++)); do echo $i; done`,
		`while :; do break; done`,
		`until false; do break; done`,
		`case $x in a|b) :;; *) :;; esac`,
		`select v in a b; do break; done`,
		`g(){ echo inner; }; g`,
		`a=(1 2); x=$((1+2)); echo "${a[@]}$x"`,
		`echo a && echo b | cat`,
		`(subshell); { grp; }`,
		`! false`,
		`echo x >&2 2>&1 < /dev/null`,
		`if a; then b; elif c; then d; else e; fi`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			src := "f(){ " + body + "; }"
			file, err := syntax.NewParser().Parse(strings.NewReader(src), "")
			if err != nil {
				t.Fatalf("the case itself does not parse: %v", err)
			}
			fn, ok := file.Stmts[0].Cmd.(*syntax.FuncDecl)
			if !ok {
				t.Fatalf("case did not parse as a function declaration")
			}
			if err := printFuncCanonicalRoundTrips("f", fn.Body); err != nil {
				t.Errorf("printed function does not re-parse: %v\n%s",
					err, printFuncCanonical("f", fn.Body, false))
			}
		})
	}
}
