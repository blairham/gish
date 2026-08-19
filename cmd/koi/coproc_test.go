//go:build unix

package main

import (
	"strings"
	"testing"
)

// The two halves of writing a bounded parallel job loop (#287).
//
// `wait -n` blocks until *some* job finishes; `coproc` is the other
// shape of the same need, a long-lived helper the script talks to over a
// pair of descriptors. Neither worked, so the thing a build script or a
// test runner reaches for could not be written at all.
//
// bash is the oracle. These run the same script through both and compare.
func TestParallelJobIdiomsMatchBash(t *testing.T) {
	bashBin := requireBash(t)
	koiBin := buildKoi(t)

	cases := []struct {
		name   string
		script string
		// needs is the feature the *oracle* must have for the case to
		// mean anything. macOS still ships bash 3.2 as /bin/bash, which
		// predates coproc by a major release and `wait -n` by two, so
		// there the oracle answers "invalid option" and a syntax error
		// for constructs koi implements correctly — comparing against
		// that would report koi as broken for being newer.
		needs string
	}{
		{
			name:  "wait -n returns when the first job finishes",
			needs: featWaitN,
			// Ordered by sleep rather than by luck: the point being
			// checked is that -n returns on the *first* completion, and
			// two jobs racing would not show that.
			script: `(sleep 0.3; exit 3) & (exit 4) & wait -n; echo "first=$?"`,
		},
		{
			name:   "wait -n hands back each job once, then 127",
			needs:  featWaitN,
			script: `(exit 3) & (sleep 0.2; exit 4) & wait -n; echo "a=$?"; wait -n; echo "b=$?"; wait -n; echo "drained=$?"`,
		},
		{
			name:  "a bounded parallel loop keeps N in flight",
			needs: featWaitN,
			// Each job writes to its own file rather than to stdout, and
			// not for tidiness: koi's background jobs are goroutines
			// sharing one writer, and `echo` is several writes, so two
			// jobs printing at once fuse into `done:2done:3` (#301).
			// That predates this change and reproduces on main without
			// any `wait -n`, so letting it flake this test would be
			// testing the scheduler rather than the loop.
			script: `MAX=3; n=0; d=$(mktemp -d)
for i in 1 2 3 4 5 6 7 8; do
  if (( n >= MAX )); then wait -n; n=$((n-1)); fi
  { sleep 0.05; echo "done:$i" > "$d/$i"; } &
  n=$((n+1))
done
wait
cat "$d"/*
rm -rf "$d"
echo all-finished`,
		},
		{
			name:   "a coprocess round-trips over its two descriptors",
			needs:  featCoproc,
			script: `coproc C { read -r l; echo "got:$l"; }; echo hi >&"${C[1]}"; read -r r <&"${C[0]}"; echo "$r"`,
		},
		{
			name:   "an unnamed coprocess is COPROC",
			needs:  featCoproc,
			script: `coproc { echo from-default; }; read -r r <&"${COPROC[0]}"; echo "$r"`,
		},
		{
			name:  "a coprocess is a job, so its status is waitable",
			needs: featCoproc,
			// C_PID has to spell the job the way $! does or `wait` cannot
			// find what `coproc` just started.
			script: `coproc C { exit 4; }; wait "$C_PID"; echo "status=$?"`,
		},
		{
			name:  "several lines through one coprocess",
			needs: featCoproc,
			// The case a coprocess exists for: a helper kept alive across
			// many exchanges rather than re-spawned per line.
			script: `coproc C { while read -r l; do echo "echo:$l"; done; }
for w in alpha beta gamma; do
  echo "$w" >&"${C[1]}"
  read -r r <&"${C[0]}"
  echo "$r"
done`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !oracleHas(t, bashBin, tc.needs) {
				t.Skipf("bash on this machine has no %s (%s) — no oracle for this case",
					tc.needs, bashVersion(t, bashBin))
			}
			wantOut, wantCode := runArgv(t, bashBin, []string{"-c", tc.script})
			gotOut, gotCode := runArgv(t, koiBin, []string{"-c", tc.script})
			if gotOut != wantOut {
				t.Errorf("output = %q, bash = %q", gotOut, wantOut)
			}
			if gotCode != wantCode {
				t.Errorf("exit status = %d, bash = %d", gotCode, wantCode)
			}
		})
	}
}

// The two features the oracle needs, named so a case declares what it
// depends on rather than repeating a probe script.
const (
	featCoproc = "coproc"
	featWaitN  = "wait -n"
)

// oracleHas reports whether this machine's bash implements feat.
//
// Asked of the oracle rather than derived from its version string, which
// is the same choice builtins_matrix_test.go made and for the same
// reason: a version-gated list is another claim that needs its own
// maintenance, and it would be wrong the moment a distro backports
// something. Running the construct and looking at whether bash complained
// cannot drift.
func oracleHas(t *testing.T, bashBin, feat string) bool {
	t.Helper()
	var probe string
	switch feat {
	case featCoproc:
		probe = "coproc C { :; }"
	case featWaitN:
		probe = "wait -n"
	default:
		t.Fatalf("unknown oracle feature %q", feat)
	}
	out, _ := runArgv(t, bashBin, []string{"-c", probe})
	// bash 3.2 has no `coproc` keyword, so the brace group is a syntax
	// error; it has no `wait -n`, so the flag is an invalid option.
	return !strings.Contains(out, "syntax error") && !strings.Contains(out, "invalid option")
}
