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
		if _, err := parseUmask(ok, 0o022); err != nil {
			t.Errorf("parseUmask(%q) failed: %v", ok, err)
		}
	}
	// Symbolic modes are supported now (#411), and describe the
	// permissions to *allow* — so they are read against the mask
	// already in force and inverted.
	for _, tc := range []struct {
		mode    string
		current int
		want    int
	}{
		{"u=rwx,g=rwx,o=rx", 0o022, 0o002},
		{"a=rwx", 0o022, 0},
		{"u+w", 0o022, 0o022},
		{"a-w", 0o022, 0o222},
		{"g-rwx", 0o022, 0o072},
		{"=", 0o022, 0o777},
	} {
		got, err := parseUmask(tc.mode, tc.current)
		if err != nil {
			t.Errorf("parseUmask(%q) failed: %v", tc.mode, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseUmask(%q, %04o) = %04o, want %04o", tc.mode, tc.current, got, tc.want)
		}
	}
	for _, bad := range []string{"999", "", "0x20", "1000", "u=q", "z+r"} {
		if _, err := parseUmask(bad, 0o022); err == nil {
			t.Errorf("parseUmask(%q) accepted a mode it cannot apply", bad)
		}
	}
}
