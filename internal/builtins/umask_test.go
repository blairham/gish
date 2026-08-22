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
		// A clause may carry more than one action, and a permission may
		// be a who letter meaning "whatever that one has now" — the two
		// halves of #604's umask fix, both confirmed against bash 5.3.
		{"u=r+w", 0o022, 0o122},
		{"u=r-w", 0o022, 0o322},
		{"g+u,o+rwx-u", 0o022, 0o007},
		{"u=r+w,g=wx,o+xr", 0o022, 0o142},
		{"u+w=r+x", 0o022, 0o222},
		{"o=u", 0o022, 0o020},
		{"g=u", 0o022, 0o002},
		{"u+g,g+o,o-rw", 0o022, 0o026},
		{"o+ru", 0o077, 0o070},
		{"u=g=o", 0o022, 0o222},
		// setuid, setgid and sticky are accepted and reach none of the
		// nine bits; `X` adds execute only when something already has it.
		{"u+s", 0o022, 0o022},
		{"u+t", 0o022, 0o022},
		{"a+X", 0o777, 0o777},
		{"g+wX", 0o022, 0o002},
		// bash accepts a four-digit mode and reads it back whole.
		{"1000", 0o022, 0o1000},
		{"7777", 0o022, 0o7777},
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
	// `1000` left this list because bash accepts it, which is the reason
	// the ceiling moved to 07777; `9x` and `08` joined it because the
	// octal path is chosen by the first byte, so they are out-of-range
	// numbers rather than symbolic modes.
	for _, bad := range []string{"999", "", "0x20", "9x", "08", "10000", "u=q", "z+r", "u", "u=r,", ",u=r"} {
		if _, err := parseUmask(bad, 0o022); err == nil {
			t.Errorf("parseUmask(%q) accepted a mode it cannot apply", bad)
		}
	}
	// The wording is bash's, per family: the octal complaint names the
	// mode and the symbolic one names the byte it stopped at.
	for _, tc := range []struct {
		mode string
		want string
	}{
		{"999", "999: octal number out of range"},
		{"u=q", "`q': invalid symbolic mode character"},
		{"q=r", "`q': invalid symbolic mode operator"},
	} {
		_, err := parseUmask(tc.mode, 0o022)
		if err == nil || err.Error() != tc.want {
			t.Errorf("parseUmask(%q) error = %v, want %q", tc.mode, err, tc.want)
		}
	}
}
