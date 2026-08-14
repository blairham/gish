package repl

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestStarshipRenderLive pins the flag contract against a real starship
// binary; CI without starship skips.
func TestStarshipRenderLive(t *testing.T) {
	if _, err := exec.LookPath("starship"); err != nil {
		t.Skip("starship not installed")
	}
	var s starshipTheme
	info := promptInfo{dir: t.TempDir(), exitCode: 1, duration: 5 * time.Second, jobs: 2, width: 80}
	p, cont, ok := s.render(info, info.width)
	if !ok {
		t.Fatal("render failed against a real starship")
	}
	if !strings.Contains(p, "\x1b[") {
		t.Errorf("prompt has no styling: %q", p)
	}
	if cont == "" {
		t.Error("no continuation prompt")
	}
	// Stale-serve: a second render reuses machinery without error.
	if _, _, ok := s.render(info, info.width); !ok {
		t.Error("second render failed")
	}
}

func TestStarshipMissingBinaryFallsBack(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no starship here
	var s starshipTheme
	if _, _, ok := s.render(promptInfo{width: 80}, 80); ok {
		t.Fatal("render claimed success without a binary")
	}
	// warned once, silent after
	if !s.warned {
		t.Error("missing-binary warning not recorded")
	}
}
