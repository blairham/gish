package compat_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// The substrate gaps (#119), reduced to minimal reproductions.
//
// All four are `mvdan.cc/sh` behaviors rather than gish code, so the
// fix belongs upstream where it improves every consumer. What this file
// is for is the other half of that: **noticing when one is fixed.** A
// gap that is quietly corrected upstream leaves gish carrying a
// workaround, a doc paragraph and a corpus row that are all now wrong,
// and nothing would say so.
//
// So each case asserts the *current* state and fails when it changes —
// in either direction. A failure here is not a regression; it is a
// prompt to re-check the gap, update docs/compat.md, and drop whatever
// gish carries for it.

// substrateGap is one reproduction, with what bash says and what the
// substrate says today.
type substrateGap struct {
	name     string
	script   string
	gishWant string // today's behavior, which is what this pins
	upstream string // the issue's disposition
}

var substrateGaps = []substrateGap{
	{
		name:     "associative array element count",
		script:   `declare -A m; m[a]=1; m[b]=2; echo "${#m[@]}"`,
		gishWant: "1",
		upstream: "the count path is wrong; the keys themselves are present",
	},
	{
		name:     "prefix-anchored substitution",
		script:   `s=a-b-c; echo "${s/#a/A}"`,
		gishWant: "a-b-c",
		upstream: "silently no-ops, which is the dangerous kind: wrong data, not an error",
	},
	{
		name:     "suffix-anchored substitution",
		script:   `s=a-b-c; echo "${s/%c/C}"`,
		gishWant: "a-b-c",
		upstream: "same as the prefix form",
	},
	{
		name:   "negated POSIX class in pattern removal",
		script: `x="  hi  "; echo "[${x%%[![:space:]]*}]"`,
		gishWant: "gish: internal error running gish -c: regexp: " +
			"Compile(`((?s)[^[:space:]\\].*)$`): error parsing regexp: missing closing ]: " +
			"`[^[:space:]\\].*)$` (this is a gish bug: https://github.com/blairham/gish/issues)\n" +
			"run with GISH_DEBUG=1 to include the stack",
		upstream: "the pattern→regexp translation emits an invalid class and MustCompiles it, " +
			"so this *panicked* until #217 put a recover boundary around the interpreter — the " +
			"recorded output is that boundary reporting the failure instead of the process dying. " +
			"Trimming with ${x%%[![:space:]]*} is ordinary, and this reached gish through a " +
			"vendor's ~/.profile block, so every login invocation crashed",
	},
	{
		name:     "exec file-descriptor persistence",
		script:   `exec 3>&1; echo via-fd3 >&3; exec 3>&-`,
		gishWant: "",
		upstream: "output to the duplicated fd is lost; common in logging wrappers",
	},
	{
		name:     "single quote escaped inside an assignment",
		script:   `x='a'\''b'; printf '%s' "$x"`,
		gishWant: `a\'b`,
		upstream: "the `'\\''` form is correct in a command word and wrong in an assignment",
	},
}

func TestSubstrateGapsAreStillThere(t *testing.T) {
	gishBin := buildGish(t)
	ctx := context.Background()

	for _, gap := range substrateGaps {
		t.Run(gap.name, func(t *testing.T) {
			out, err := exec.CommandContext(ctx, gishBin, "-c", gap.script).CombinedOutput()
			_ = err // a gap may exit non-zero; the output is the evidence
			got := strings.TrimSpace(string(out))
			if got == gap.gishWant {
				return
			}
			// Both directions are worth knowing about, and "fixed" is
			// the more likely one — it means gish is carrying a
			// workaround and a doc paragraph that are now wrong.
			t.Errorf("substrate behavior changed for %q\n  now:      %q\n  recorded: %q\n  upstream: %s\n"+
				"Re-check the gap, update docs/compat.md, and drop anything gish carries for it.",
				gap.script, got, gap.gishWant, gap.upstream)
		})
	}
}

// The one gap with a seam is fixed locally, and this is the case that
// broke: a precision on a float, which is how every script that formats
// a duration, a percentage or a size writes it.
func TestPrintfPrecisionIsFixedLocally(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash: the differential oracle is unavailable")
	}
	gishBin := buildGish(t)
	ctx := context.Background()

	for _, script := range []string{
		`printf "%05.2f\n" 3.14159`,
		`printf "%s=%.1f%%\n" cpu 42.317`,
		`printf "%.3e\n" 1234.5678`,
		`printf "%s\n" a b c`, // the recycling rule the local path must keep
		`printf "%d items\n" 42`,
		`printf "%-8s|\n" left`,
	} {
		t.Run(script, func(t *testing.T) {
			gishOut, _ := exec.CommandContext(ctx, gishBin, "-c", script).CombinedOutput()
			bashOut, _ := exec.CommandContext(ctx, bashBin, "-c", script).CombinedOutput()
			if string(gishOut) != string(bashOut) {
				t.Errorf("gish %q vs bash %q", gishOut, bashOut)
			}
		})
	}
}
