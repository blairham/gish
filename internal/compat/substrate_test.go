package compat_test

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// The substrate gaps (#119), reduced to minimal reproductions.
//
// All four are `mvdan.cc/sh` behaviors rather than koi code, so the
// fix belongs upstream where it improves every consumer. What this file
// is for is the other half of that: **noticing when one is fixed.** A
// gap that is quietly corrected upstream leaves koi carrying a
// workaround, a doc paragraph and a corpus row that are all now wrong,
// and nothing would say so.
//
// So each case asserts the *current* state and fails when it changes —
// in either direction. A failure here is not a regression; it is a
// prompt to re-check the gap, update docs/compat.md, and drop whatever
// koi carries for it.

// substrateGap is one reproduction, with what bash says and what the
// substrate says today.
type substrateGap struct {
	name     string
	script   string
	koiWant  string // today's behavior, which is what this pins
	upstream string // the issue's disposition
}

var substrateGaps = []substrateGap{
	{
		name:     "associative array element count",
		script:   `declare -A m; m[a]=1; m[b]=2; echo "${#m[@]}"`,
		koiWant:  "1",
		upstream: "the count path is wrong; the keys themselves are present",
	},
	{
		name:     "prefix-anchored substitution",
		script:   `s=a-b-c; echo "${s/#a/A}"`,
		koiWant:  "a-b-c",
		upstream: "silently no-ops, which is the dangerous kind: wrong data, not an error",
	},
	{
		name:     "suffix-anchored substitution",
		script:   `s=a-b-c; echo "${s/%c/C}"`,
		koiWant:  "a-b-c",
		upstream: "same as the prefix form",
	},
	{
		name:     "exec file-descriptor persistence",
		script:   `exec 3>&1; echo via-fd3 >&3; exec 3>&-`,
		koiWant:  "",
		upstream: "output to the duplicated fd is lost; common in logging wrappers",
	},
	{
		name:     "single quote escaped inside an assignment",
		script:   `x='a'\''b'; printf '%s' "$x"`,
		koiWant:  `a\'b`,
		upstream: "the `'\\''` form is correct in a command word and wrong in an assignment",
	},
}

func TestSubstrateGapsAreStillThere(t *testing.T) {
	koiBin := buildKoi(t)
	ctx := context.Background()

	for _, gap := range substrateGaps {
		t.Run(gap.name, func(t *testing.T) {
			out, err := exec.CommandContext(ctx, koiBin, "-c", gap.script).CombinedOutput()
			_ = err // a gap may exit non-zero; the output is the evidence
			got := strings.TrimSpace(string(out))
			if got == gap.koiWant {
				return
			}
			// Both directions are worth knowing about, and "fixed" is
			// the more likely one — it means koi is carrying a
			// workaround and a doc paragraph that are now wrong.
			t.Errorf("substrate behavior changed for %q\n  now:      %q\n  recorded: %q\n  upstream: %s\n"+
				"Re-check the gap, update docs/compat.md, and drop anything koi carries for it.",
				gap.script, got, gap.koiWant, gap.upstream)
		})
	}
}

// A POSIX class inside a bracket expression — the one gap on this list
// fixed by moving the substrate pin rather than by carrying anything.
//
// It belongs here rather than in substrateGaps because the pin is the
// only thing holding it: `go get -u`, a resolver picking the newest
// *tagged* release, or a merge that reverts go.mod all put the crash
// back, and none of them look like a change to pattern handling. So this
// asserts the fixed behavior directly, and a downgrade fails it.
//
// Worth knowing why it was expensive: the translation emitted an
// uncompilable regexp and handed it to MustCompile, so an ordinary
// `${x%%[![:space:]]*}` trim did not misbehave, it *panicked* — and it
// reached koi through a vendor's ~/.profile block, which meant every
// login shell died before its first prompt.
func TestPatternCharacterClassesMatchBash(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash: the differential oracle is unavailable")
	}
	koiBin := buildKoi(t)
	ctx := context.Background()

	for _, script := range []string{
		`x="  hi  "; echo "[${x%%[![:space:]]*}]"`, // the crash, verbatim
		`x="  hi  "; echo "[${x##*[![:space:]]}]"`, // the other trim half
		`x="  hi  "; echo "[${x#[[:space:]]}]"`,    // class as the whole expression
		`x=a1b; echo "[${x##[[:alpha:]0-9]}]"`,     // class mixed with a range
		`x=abc; echo "[${x%[^[:digit:]]}]"`,        // '^' spelling of the negation
		`case 9 in [![:digit:]]) echo no;; *) echo yes;; esac`,
		`case x in [![:digit:]]) echo no-digit;; esac`,
	} {
		t.Run(script, func(t *testing.T) {
			koiOut, _ := exec.CommandContext(ctx, koiBin, "-c", script).CombinedOutput()
			bashOut, _ := exec.CommandContext(ctx, bashBin, "-c", script).CombinedOutput()
			if string(koiOut) != string(bashOut) {
				t.Errorf("koi %q vs bash %q", koiOut, bashOut)
			}
		})
	}
}

// `printf -v` writes into a variable instead of stdout (#219).
//
// It is worth a differential of its own because of how it failed: not
// loudly, but by printing a literal "-v" and leaving the variable
// empty. bash-preexec uses it to capture the command line, and
// bash-preexec is what ships inside Kiro's, iTerm2's and Atuin's shell
// integrations — so on a login shell here it produced "-v-v-v" before
// the prompt and handed the integration empty strings, which reads as a
// shell where every command is blank rather than as a broken printf.
//
// These are the success shapes, where stdout is the whole story. The
// statuses and diagnostics are next door, since bash prefixes its
// stderr with its own name and line number.
func TestPrintfDashVMatchesBash(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash: the differential oracle is unavailable")
	}
	koiBin := buildKoi(t)
	ctx := context.Background()

	for _, script := range []string{
		`printf -v x "%s" hello; echo "[$x]"`,
		`printf -vx "%s" hi; echo "[$x]"`,            // clustered
		`printf -v x "%s\n" a b c; echo "[$x]"`,      // the format recycles into the variable
		`printf -v x "%05.2f" 3.14159; echo "[$x]"`,  // the local precision path still applies
		`printf -v x "%s" "with space"; echo "[$x]"`, // quoting survives the round trip
		`printf -v x "%s" "quote'd"; echo "[$x]"`,    // ...including a single quote
		`printf -v x -- "%s" hi; echo "[$x]"`,        // -- ends the options
		`printf -v x "%s" -- ; echo "[$x]"`,          // ...but is an operand after the format
		`printf "%s" -v x; echo`,                     // -v is not an option after the format
		`printf -- "-%s" a; echo`,                    // a format that starts with a dash
		// The scope is the reason this cannot be done in the handler:
		// bash writes the local, not a global of the same name.
		`f(){ local y; printf -v y "%s" in; echo "[$y]"; }; f; echo "[${y-unset}]"`,
	} {
		t.Run(script, func(t *testing.T) {
			koiOut, _ := exec.CommandContext(ctx, koiBin, "-c", script).CombinedOutput()
			bashOut, _ := exec.CommandContext(ctx, bashBin, "-c", script).CombinedOutput()
			if string(koiOut) != string(bashOut) {
				t.Errorf("koi %q vs bash %q", koiOut, bashOut)
			}
		})
	}

	// Assigning through a subscript is a bash 4 feature, and the oracle
	// on a stock macOS is /bin/bash 3.2 — the last GPLv2 release, which
	// Apple has shipped frozen since 2007. It answers `arr[2]': not a
	// valid identifier, so comparing there would assert that koi
	// should refuse something modern bash accepts.
	//
	// The scoreboard already refuses to compare across bash majors for
	// the same reason; this is that rule applied to one case rather
	// than a whole run.
	t.Run("subscript target", func(t *testing.T) {
		const script = `i=2; printf -v "arr[$i]" "%s" hi; echo "[${arr[2]}]"`
		major := bashMajor(t, bashBin)
		// Compared numerically: "10" sorts before "4" as a string, and
		// a version check that breaks on the next major is a trap left
		// for somebody else.
		if n, err := strconv.Atoi(major); err != nil || n < 4 {
			t.Skipf("oracle is bash %s: `printf -v arr[i]' arrived in bash 4", major)
		}
		koiOut, _ := exec.CommandContext(ctx, koiBin, "-c", script).CombinedOutput()
		bashOut, _ := exec.CommandContext(ctx, bashBin, "-c", script).CombinedOutput()
		if string(koiOut) != string(bashOut) {
			t.Errorf("koi %q vs bash %q", koiOut, bashOut)
		}
	})
}

// The statuses, which bash distinguishes more finely than a builtin
// returning true/false can: a bad identifier is 2, a bad conversion is
// 1, and a call with no format at all is 2.
func TestPrintfDashVStatusesMatchBash(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash: the differential oracle is unavailable")
	}
	koiBin := buildKoi(t)
	ctx := context.Background()

	for _, script := range []string{
		`printf -v 1bad "%s" q`, // not a valid identifier
		`printf -v "" "%s" q`,   // ditto, empty
		`printf -v x.y "%s" hi`, // ditto
		`printf -v "x;echo pwned" "%s" hi`,
		`printf -v "x[1]$(echo p)[2]" "%s" hi`, // the subscript is not the whole tail
		`printf -v x`,                          // no format
		`printf`,                               // no format, no -v
		`printf -v x "%s" ok`,                  // the success case, for contrast
	} {
		t.Run(script, func(t *testing.T) {
			koiCode := runStatus(ctx, t, koiBin, script)
			bashCode := runStatus(ctx, t, bashBin, script)
			if koiCode != bashCode {
				t.Errorf("exit status: koi %d, bash %d", koiCode, bashCode)
			}
		})
	}
}

// runStatus reports the exit status of running script through sh.
func runStatus(ctx context.Context, t *testing.T, sh, script string) int {
	t.Helper()
	cmd := exec.CommandContext(ctx, sh, "-c", script)
	_ = cmd.Run()
	return cmd.ProcessState.ExitCode()
}

// The one gap with a seam is fixed locally, and this is the case that
// broke: a precision on a float, which is how every script that formats
// a duration, a percentage or a size writes it.
func TestPrintfPrecisionIsFixedLocally(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash: the differential oracle is unavailable")
	}
	koiBin := buildKoi(t)
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
			koiOut, _ := exec.CommandContext(ctx, koiBin, "-c", script).CombinedOutput()
			bashOut, _ := exec.CommandContext(ctx, bashBin, "-c", script).CombinedOutput()
			if string(koiOut) != string(bashOut) {
				t.Errorf("koi %q vs bash %q", koiOut, bashOut)
			}
		})
	}
}
