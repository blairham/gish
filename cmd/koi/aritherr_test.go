//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What an arithmetic error costs, which is a question about control flow
// rather than about wording (#597).
//
// bash decides whether an assignment's target is a variable while
// *evaluating*, so `$((5 += 2))` is a runtime refusal that abandons the
// line and lets the script carry on, while `(( 5 += 2 ))` and `let 5+=2`
// are commands whose evaluation failed — they report under their own name
// and answer 1, and the line keeps going. koi refused all three at parse
// time and, parsing ahead, lost the rest of the file.
//
// The diagnostics themselves are known to diverge: bash names the
// expression as it was *written* and the token it stopped at
// (`5 += 2 : … (error token is "+= 2 ")`) while koi prints the expression
// back from the parse tree, so the spacing is normalized and the tail is
// absent. That residue is filed rather than encoded here, which is why
// this test compares the *behavior* — which commands ran, what `$?`
// said — with each shell's diagnostic lines dropped, and asserts koi's
// own prefixes separately. A wording fix must not be able to pass this
// while breaking the control flow, and vice versa.
func TestArithmeticAssignmentIsARuntimeVerdict(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	for _, tc := range []struct {
		name   string
		script string
		// diag is the prefix koi puts the message under: empty for a
		// word, the command's own name for a command.
		diag string
	}{
		{
			name:   "in a word the line is abandoned",
			script: "echo pre; echo $((5 += 2)); echo lost\necho next=$?\necho tail\n",
		},
		{
			name:   "the ternary shape bash's own arith.tests stops on",
			script: "echo pre; echo $((1 ? 20 : x+=2)); echo lost\necho next=$?\necho tail\n",
		},
		{
			name:   "division by zero is the same category",
			script: "echo pre; echo $((1/0)); echo lost\necho next=$?\necho tail\n",
		},
		{
			name:   "an arithmetic command only fails",
			script: "echo pre; (( 5 += 2 )); echo cmd=$?\necho next=$?\necho tail\n",
			diag:   "((: ",
		},
		{
			name:   "let reports under its own name",
			script: "echo pre; let 5+=2; echo let=$?\necho next=$?\necho tail\n",
			diag:   "let: ",
		},
		{
			name:   "a C-style loop header is an arithmetic command too",
			script: "echo pre; for ((i=0; 5+=2; i++)); do echo body; done; echo loop=$?\necho next=$?\necho tail\n",
			diag:   "((: ",
		},
		{
			name:   "a target that is a variable still assigns",
			script: "x=3; echo $((x += 2)); echo x=$x\necho next=$?\necho tail\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "s.sh"), []byte(tc.script), 0o600); err != nil {
				t.Fatal(err)
			}
			bashOut, bashCode := runInDir(t, dir, bash, "./s.sh")
			koiOut, koiCode := runInDir(t, dir, koi, "./s.sh")

			if got, want := plainLines(koiOut), plainLines(bashOut); got != want {
				t.Errorf("what ran differs from bash\n  bash: %q\n  koi:  %q", want, got)
			}
			if koiCode != bashCode {
				t.Errorf("exit = %d, bash exits %d", koiCode, bashCode)
			}
			// Both shells printing nothing would agree while proving
			// nothing, so the oracle has to have said something.
			if tc.diag != "" || strings.Contains(tc.script, "lost") {
				if !strings.Contains(bashOut, "line 1: ") {
					t.Fatalf("the oracle reported nothing, so this case cannot detect a missing diagnostic: %q", bashOut)
				}
				if want := "./s.sh: line 1: " + tc.diag; !strings.Contains(koiOut, want) {
					t.Errorf("koi's diagnostic does not start with %q: %q", want, koiOut)
				}
			}
		})
	}
}

// plainLines is the output with each shell's diagnostic lines dropped, so
// what is left is what the script itself printed. A diagnostic is the
// line naming the script and a line number, which is the shape both
// shells use (#571).
func plainLines(out string) string {
	var kept []string
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "s.sh: line ") || strings.Contains(line, "s.sh: ") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
