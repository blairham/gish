package repl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A second rc file that exists and is never read (#232).
//
// Nothing in rc resolution misbehaves — first hit wins, and it is
// documented. The bug was that the information was invisible from the one
// surface built to make configuration legible, so doctor reported two true
// and useless lines and a ✔ argued the setup was fine.

func TestShadowedRCsNamesTheSkippedFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // UserHomeDir reads this on Windows
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("KOI_RC", "")

	xdg := filepath.Join(home, ".config", "koi", "koirc")
	classic := filepath.Join(home, ".koirc")

	// Only the classic file: it is the active one, nothing is shadowed.
	writeFile(t, classic, "KOI_THEME=p10k\n")
	if got := rcPath(); got != classic {
		t.Fatalf("rcPath = %q, want %q", got, classic)
	}
	if s := shadowedRCs(); len(s) != 0 {
		t.Errorf("one rc file reported %v as shadowed", s)
	}

	// Add the higher-precedence file: now the classic one is dead, and
	// that is exactly what nobody could see.
	writeFile(t, xdg, "KOI_WELCOME=off\n")
	if got := rcPath(); got != xdg {
		t.Fatalf("rcPath = %q, want the XDG file %q", got, xdg)
	}
	s := shadowedRCs()
	if len(s) != 1 || s[0] != classic {
		t.Errorf("shadowedRCs = %v, want [%s]", s, classic)
	}
}

// KOI_RC outranks both defaults, so a user who sets it *and* keeps a
// ~/.koirc is in the same trap — and doctor should say so rather than
// only naming the file it read.
func TestKoiRCShadowsBothDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // UserHomeDir reads this on Windows
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	explicit := filepath.Join(home, "explicit-rc")
	writeFile(t, explicit, "KOI_LINT=off\n")
	t.Setenv("KOI_RC", explicit)

	writeFile(t, filepath.Join(home, ".config", "koi", "koirc"), "x=1\n")
	writeFile(t, filepath.Join(home, ".koirc"), "y=2\n")

	if got := rcPath(); got != explicit {
		t.Fatalf("rcPath = %q, want %q", got, explicit)
	}
	if s := shadowedRCs(); len(s) != 2 {
		t.Errorf("shadowedRCs = %v, want both defaults", s)
	}
}

// Nothing existing must not report a phantom shadow, and must not be an
// error: no rc file at all is the normal first-run state.
func TestNoRCFilesShadowNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // UserHomeDir reads this on Windows
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("KOI_RC", "")

	if got := rcPath(); got != "" {
		t.Errorf("rcPath = %q with no rc files, want empty", got)
	}
	if s := shadowedRCs(); len(s) != 0 {
		t.Errorf("shadowedRCs = %v with no rc files, want none", s)
	}
}

// The doctor line is the deliverable, so assert on it rather than only on
// the helper: a ✔ here is the actual bug.
func TestDoctorWarnsAboutAShadowedRC(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // UserHomeDir reads this on Windows
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("KOI_RC", "")
	writeFile(t, filepath.Join(home, ".config", "koi", "koirc"), "KOI_WELCOME=off\n")
	writeFile(t, filepath.Join(home, ".koirc"), "KOI_THEME=p10k\n")

	got := checkRC()
	if got.status != checkWarn {
		t.Errorf("status = %v, want a warning — a tick argues the config is fine", got.status)
	}
	for _, want := range []string{".koirc", "never read"} {
		if !strings.Contains(got.detail, want) {
			t.Errorf("detail %q does not mention %q", got.detail, want)
		}
	}
	if got.fix == "" {
		t.Error("no fix offered; the point is to turn a diagnosis into a five-second read")
	}

	// And one rc file stays a clean tick: a warning on every session
	// would train people to ignore it.
	if err := os.Remove(filepath.Join(home, ".koirc")); err != nil {
		t.Fatal(err)
	}
	if got := checkRC(); got.status != checkOK {
		t.Errorf("status = %v with a single rc file, want OK", got.status)
	}
}
