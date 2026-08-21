//go:build unix

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/compat"
)

// Diagnostics for a command that cannot be started (#569).
//
// Differential, because the whole point is bash's wording and bash's
// status: 127 means "no such command" and 126 means "found it, cannot
// run it", and koi answered 127 in Go's words for every failure. A
// hand-written expectation here would just be this file's author
// remembering bash, which is the mistake that let `stat /abs/path: no
// such file or directory` stand in for `./deploy: No such file or
// directory`.
//
// Each script normalizes two things in the script itself rather than in
// Go, so the comparison is over what is left:
//
//   - bash prefixes its runtime diagnostics with `$0: line N:` and koi
//     prints no location at all. That difference is real and tracked in
//     #569; it is not what these cases are about.
//   - the temp directory's path differs per run.
//
// The two shells share one temp directory per case, so a script that
// leaves a file behind has to tolerate finding it: the first run's
// chmod 000 is what the second run's redirection would trip over.
var execErrorCases = []struct {
	name   string
	script string
}{
	{
		// The status is the load-bearing half: a script that tests for
		// 126 to mean "installed but not runnable" never saw it.
		name: "lookup failures",
		script: `{ nosuchcmd; echo "notfound=$?"
./nosuchfile; echo "nofile=$?"
mkdir -p "$TMPD/adir"; "$TMPD/adir"; echo "isdir=$?"
echo x > "$TMPD/noexec"; chmod 644 "$TMPD/noexec"; "$TMPD/noexec"; echo "noexec=$?"
} 2>&1 | sed -e "s|^[^ ]*: line [0-9]*: ||" -e "s|$TMPD|TMPD|g"`,
	},
	{
		// A PATH search that finds a file it cannot run reports *that*
		// file, by full path and with 126, rather than claiming the
		// command does not exist. A directory or a dangling link in PATH
		// is not that case: those are still "command not found".
		name: "PATH candidates",
		script: `mkdir -p "$TMPD/bin" "$TMPD/bin/adir"
echo x > "$TMPD/bin/noexec"; chmod 644 "$TMPD/bin/noexec"
ln -sf /nonexistent "$TMPD/bin/dangling"
{ PATH="$TMPD/bin" noexec; echo "noexec=$?"
PATH="$TMPD/bin" adir; echo "adir=$?"
PATH="$TMPD/bin" dangling; echo "dangling=$?"
} 2>&1 | sed -e "s|^[^ ]*: line [0-9]*: ||" -e "s|$TMPD|TMPD|g"`,
	},
	{
		// A PATH entry that cannot run is remembered, not preferred: a
		// later directory with a working copy still wins.
		name: "PATH keeps searching",
		script: `mkdir -p "$TMPD/first" "$TMPD/second"
echo 'echo second-wins' > "$TMPD/first/prog"; chmod 644 "$TMPD/first/prog"
echo 'echo second-wins' > "$TMPD/second/prog"; chmod 755 "$TMPD/second/prog"
PATH="$TMPD/first:$TMPD/second" prog; echo "st=$?"`,
	},
	{
		// source's own diagnostics, which had the same shape: Go's
		// error, an absolute path nobody typed, and status 2 for a
		// directory where bash answers 1. bash names the builtin as it
		// was called for the directory case and not for the others,
		// which is odd and is bash's.
		name: "source",
		script: `{ . "$TMPD/nosuchfile"; echo "nofile=$?"
source "$TMPD"; echo "isdir=$?"
. "$TMPD"; echo "isdir_dot=$?"
rm -f "$TMPD/unread"; echo x > "$TMPD/unread"; chmod 000 "$TMPD/unread"; source "$TMPD/unread"; echo "unread=$?"
} 2>&1 | sed -e "s|^[^ ]*: line [0-9]*: ||" -e "s|$TMPD|TMPD|g"`,
	},
}

func TestCommandCannotBeExecuted(t *testing.T) {
	if testing.Short() {
		t.Skip("differential exec diagnostics skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	for _, tc := range execErrorCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			r := compat.Run(context.Background(), bash, koi, compat.Case{
				Name:   tc.name,
				Script: "TMPD=" + tmp + "\n" + tc.script,
			})
			if !r.Pass {
				t.Errorf("%s differs from bash (%s)\n  bash: %q (exit %d)\n  koi: %q (exit %d)",
					tc.name, r.Reason, r.BashOut, r.BashCode, r.KoiOut, r.KoiCode)
			}
			// A pair of shells agreeing on silence would pass every case
			// here while proving none of them.
			if !strings.Contains(r.BashOut, "=") {
				t.Errorf("%s: case reported no statuses, so it cannot detect a wrong one", tc.name)
			}
		})
	}
}
