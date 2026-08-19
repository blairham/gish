// Copyright (c) 2026, Blair Hamilton. See LICENSE for licensing information.

package interp_test

import (
	"slices"
	"testing"
)

// TestOracleRacingCaseIsRealAndNarrow guards the one exemption in
// TestRunnerRunConfirm (#317).
//
// It is matched on an exact script, so a typo or a reworded case would
// silently retire the exemption and let the flake back in — or, worse,
// leave a retry attached to nothing while the case it was meant for fails
// once and stops the build. The neighbours are named explicitly because
// they were measured as stable, and a retry they do not need would soften
// a check they currently earn.
func TestOracleRacingCaseIsRealAndNarrow(t *testing.T) {
	t.Parallel()

	const racing = `(exit 3) & wait; wait -n; echo $?`
	if !oracleRacesItself(racing) {
		t.Errorf("the racing case is no longer recognized: %q", racing)
	}
	if !slices.ContainsFunc(runTests, func(c runTest) bool { return c.in == racing }) {
		t.Errorf("no case in the table reads %q, so the exemption applies to nothing", racing)
	}
	// Measured stable across 200 runs each under 3x core-count load, so
	// they keep the single-shot check.
	for _, stable := range []string{
		`(sleep 0.1; exit 3) & (exit 4) & wait -n; echo $?`,
		`(exit 3) & (sleep 0.1; exit 4) & wait -n; wait -n; echo $?`,
		`(exit 3) & wait -n; wait -n; echo $?`,
		`(exit 3) & p=$!; wait $p; wait -n; echo $?`,
		`(exit 3) & wait -n -p v; echo "$?/${v:+set}"`,
	} {
		if oracleRacesItself(stable) {
			t.Errorf("case %q does not race and must not be given a retry", stable)
		}
	}
}
