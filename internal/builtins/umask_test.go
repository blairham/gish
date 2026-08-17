//go:build unix

package builtins

import "testing"

// Unit coverage for the parts of umask that a differential test cannot
// reach cheaply. The bash-differential suite is the oracle for output;
// these pin the two functions with real logic in them.

func TestSymbolicUmaskShowsWhatRemains(t *testing.T) {
	t.Parallel()

	// -S prints the permissions that survive the mask, not the mask, so
	// the common 022 reads as rwx / rx / rx rather than as ---/-w-/-w-.
	for _, tc := range []struct {
		mask int
		want string
	}{
		{0o022, "u=rwx,g=rx,o=rx"},
		{0o077, "u=rwx,g=,o="},
		{0o000, "u=rwx,g=rwx,o=rwx"},
		{0o777, "u=,g=,o="},
	} {
		if got := symbolicUmask(tc.mask); got != tc.want {
			t.Errorf("symbolicUmask(%04o) = %q, want %q", tc.mask, got, tc.want)
		}
	}
}

func TestParseUmaskRejectsWhatItCannotDo(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{"077", "0022", "0", "777"} {
		if _, err := parseUmask(ok); err != nil {
			t.Errorf("parseUmask(%q) failed: %v", ok, err)
		}
	}
	// Symbolic modes are refused rather than half-supported: a mode that
	// silently does not apply is the exact failure umask is used to
	// prevent.
	for _, bad := range []string{"u=rwx", "999", "", "0x20", "1000"} {
		if _, err := parseUmask(bad); err == nil {
			t.Errorf("parseUmask(%q) accepted a mode it cannot apply", bad)
		}
	}
}
