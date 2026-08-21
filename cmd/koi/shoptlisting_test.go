//go:build unix

package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/compat"
)

// `shopt`'s listings, differentially (#574).
//
// These cases live outside the builtins matrix because the matrix's
// oracle is whatever bash the machine has, and one thing asserted here
// is a bash *version* constant: the column a shopt name is padded to is
// twenty in bash 5.x and fifteen in the 3.2 that macOS still ships as
// /bin/bash. koi claims bash 5 (#120), so an older oracle cannot answer
// these — the skip below asks the oracle to state its own width rather
// than gating on a version string, since the width is the thing under
// test and a parsed version number would be a second claim to maintain.
//
// The rest is not version-sensitive in principle and is measured here
// anyway, because every one of these forms was wrong at once: with no
// names `-s` and `-u` are a *filter* over the listing rather than a
// request, and koi printed nothing at all.
var shoptListingCases = []struct {
	name   string
	script string
}{
	{
		// The filter, in both spellings. `head` keeps the case readable
		// and still covers the boundary: the filtered listing has to
		// start where bash's starts, which is what a wrong filter (or no
		// filter) breaks first.
		name:   "state filter",
		script: `shopt -s -p | head -3; echo --; shopt -u -p | head -3; echo --; shopt -s | head -3; echo --; shopt -u | head -3`,
	},
	{
		// The same filter over the `set -o` table, which shopt reaches
		// through -o and which has its own (narrower) column.
		name:   "state filter over set -o",
		script: `shopt -o -s; echo --; shopt -o -u | head -3; echo --; shopt -o -p -s`,
	},
	{
		// Names turn -s back into a set operation and -p is ignored,
		// which is measured rather than reasoned: `shopt -s -p nullglob`
		// sets nullglob and prints nothing.
		name:   "names make it a request again",
		script: `shopt -s -p nullglob; echo "st=$?"; shopt -p nullglob`,
	},
	{
		// The unfiltered listing, where the column width lives and where
		// koi used to annotate every unsupported option.
		name:   "full listing",
		script: `shopt | head -4; echo --; shopt -p | head -4; echo --; shopt nullglob; echo "st=$?"`,
	},
	{
		// bash refuses the pair rather than letting the last one win.
		name:   "set and unset together",
		script: `shopt -s -u nullglob 2>&1 | sed -e "s|^[^ ]*: line [0-9]*: ||"; echo "st=${PIPESTATUS[0]}"`,
	},
}

func TestShoptListingMatchesBash5(t *testing.T) {
	if testing.Short() {
		t.Skip("differential shopt listings skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)
	if w := oracleShoptColumn(t, bash); w != 20 {
		t.Skipf("this bash pads its shopt listing to %d, koi claims bash 5's 20 (%s)",
			w, bashVersion(t, bash))
	}

	for _, tc := range shoptListingCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := compat.Run(context.Background(), bash, koi, compat.Case{
				Name: tc.name, Script: tc.script,
			})
			if !r.Pass {
				t.Errorf("%s differs from bash (%s)\n  bash: %q (exit %d)\n  koi: %q (exit %d)",
					tc.name, r.Reason, r.BashOut, r.BashCode, r.KoiOut, r.KoiCode)
			}
			// Two shells agreeing on nothing would pass every case here.
			if strings.TrimSpace(r.BashOut) == "" {
				t.Errorf("%s: the oracle printed nothing, so the case proves nothing", tc.name)
			}
		})
	}
}

// oracleShoptColumn reports the width this bash pads a shopt name to,
// read off its own listing: the name field is everything before the tab.
func oracleShoptColumn(t *testing.T, bash string) int {
	t.Helper()
	out, err := exec.Command(bash, "-c", "shopt nullglob").Output()
	if err != nil {
		// A non-zero status is expected here — nullglob is off — so only
		// a missing tab means the listing cannot be read.
		if len(out) == 0 {
			t.Fatalf("asking bash for its shopt column: %v", err)
		}
	}
	tab := strings.IndexByte(string(out), '\t')
	if tab < 0 {
		t.Fatalf("bash's shopt listing has no tab: %q", out)
	}
	return tab
}
