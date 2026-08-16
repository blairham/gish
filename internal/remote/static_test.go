package remote

import (
	"os"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// `gish ssh` rests on one premise: the binary it copies is a pure-Go
// static build. `uname -sm` reports "linux x86_64" and says nothing
// about glibc versus musl, so a cgo-linked binary lands on Alpine and
// fails with an error that *looks like the file is missing* — a support
// case with no obvious cause, on the exact hardened host the feature
// exists for.
//
// The release config already gets this right. This test is the assertion
// that keeps it right: one added build entry without the env line and
// the premise is quietly gone.
func TestReleaseBuildsAreStatic(t *testing.T) {
	data, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatalf("read release config: %v", err)
	}
	var cfg struct {
		Builds []struct {
			ID  string   `yaml:"id"`
			Env []string `yaml:"env"`
		} `yaml:"builds"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse release config: %v", err)
	}
	if len(cfg.Builds) == 0 {
		t.Fatal("no build entries found — the assertion below would pass vacuously")
	}
	for _, b := range cfg.Builds {
		if !slices.Contains(b.Env, "CGO_ENABLED=0") {
			t.Errorf("build %q does not set CGO_ENABLED=0; `gish ssh` copies these binaries to hosts whose libc we cannot know", b.ID)
		}
	}
}
