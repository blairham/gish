package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"slices"
	"testing"
	"time"
)

func TestSplitLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		line   string
		cursor int
		want   []string
	}{
		{"git checkout ma", 15, []string{"git", "checkout", "ma"}},
		{"git checkout ", 13, []string{"git", "checkout", ""}},
		{"git", 3, []string{"git"}},
		{"", 0, nil},
		{"git checkout main extra", 12, []string{"git", "checkout"}},
	}
	for _, tt := range tests {
		got := splitLine(tt.line, tt.cursor)
		if !slices.Equal(got, tt.want) {
			t.Errorf("splitLine(%q,%d) = %q, want %q", tt.line, tt.cursor, got, tt.want)
		}
	}
}

func TestExportParse(t *testing.T) {
	t.Parallel()

	// Shape observed from carapace-bin 1.7.3 (export v1.13).
	sample := `{"version":"v1.13.0","messages":[],"noprefix":"","nospace":"","usage":"",
	 "values":[{"value":"main","display":"main","description":"last commit","style":"bold","tag":"heads"},
	           {"value":"HEAD","display":"HEAD","description":"","style":"bold","tag":"heads"}]}`
	var ex export
	if err := json.Unmarshal([]byte(sample), &ex); err != nil {
		t.Fatal(err)
	}
	if len(ex.Values) != 2 || ex.Values[0].Value != "main" || ex.Values[0].Description != "last commit" {
		t.Errorf("parsed = %+v", ex.Values)
	}
}

func TestBridgeWithoutBinary(t *testing.T) {
	t.Parallel()

	b := &bridge{} // no path: carapace not installed
	if got := b.complete(context.Background(), []string{"git", "checkout", ""}); got != nil {
		t.Errorf("missing binary should yield nothing, got %v", got)
	}
}

func TestBridgeCommandPositionIsNotBridged(t *testing.T) {
	t.Parallel()

	b := newBridge()
	// A single word is the command itself — core completion territory.
	if got := b.complete(context.Background(), []string{"gi"}); got != nil {
		t.Errorf("command-position completion bridged: %v", got)
	}
}

// TestBridgeLive exercises the real carapace binary when present
// (developer machines); CI without carapace skips.
func TestBridgeLive(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("carapace"); err != nil {
		t.Skip("carapace not installed")
	}
	b := newBridge()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	values := b.complete(ctx, []string{"git", "checkout", ""})
	if len(values) == 0 {
		t.Fatal("live carapace returned no candidates for git checkout")
	}

	if got := b.complete(ctx, []string{"definitely-not-a-real-cli", "x"}); got != nil {
		t.Errorf("unsupported command bridged: %v", got)
	}
}
