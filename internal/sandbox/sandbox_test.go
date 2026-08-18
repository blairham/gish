package sandbox

import (
	"slices"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	t.Parallel()

	p, err := Resolve("workspace", "/work/repo")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(p.WritePaths, "/work/repo") {
		t.Errorf("WriteCwd did not fold cwd in: %+v", p)
	}

	if p, err = Resolve("readonly", "/work/repo"); err != nil || len(p.WritePaths) != 0 {
		t.Errorf("readonly must not gain cwd: %+v, %v", p, err)
	}

	if _, err = Resolve("yolo", "/"); err == nil || !strings.Contains(err.Error(), "unknown sandbox profile") {
		t.Errorf("unknown profile error = %v", err)
	}
}

func TestWrapArgv(t *testing.T) {
	t.Parallel()

	p, _ := Resolve("readonly", "")
	got, err := WrapArgv("/bin/koi", p, []string{"make", "test"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "/bin/koi" || got[1] != ExecFlag || got[3] != "--" ||
		got[4] != "make" || got[5] != "test" {
		t.Fatalf("wrapped = %q", got)
	}
	if !strings.Contains(got[2], `"name":"readonly"`) {
		t.Errorf("policy blob = %q", got[2])
	}
}

func TestFilterEnv(t *testing.T) {
	t.Parallel()

	environ := []string{"PATH=/bin", "AWS_SECRET_ACCESS_KEY=hunter2", "HOME=/home/u", "MALFORMED"}
	all := filterEnv(environ, Policy{EnvAll: true})
	if len(all) != 4 {
		t.Errorf("EnvAll should pass everything: %q", all)
	}
	kept := filterEnv(environ, Policy{EnvAll: false})
	if slices.Contains(kept, "AWS_SECRET_ACCESS_KEY=hunter2") {
		t.Errorf("secret survived the allowlist: %q", kept)
	}
	if !slices.Contains(kept, "PATH=/bin") || !slices.Contains(kept, "HOME=/home/u") {
		t.Errorf("allowlisted vars dropped: %q", kept)
	}
}

func TestWriteRoots(t *testing.T) {
	t.Parallel()

	roots := writeRoots(Policy{WritePaths: []string{"/dev", "/work"}})
	if !slices.Contains(roots, "/dev") || !slices.Contains(roots, "/work") {
		t.Errorf("roots = %q", roots)
	}
	if len(roots) != len(slices.Compact(slices.Clone(roots))) {
		t.Errorf("roots not deduplicated: %q", roots)
	}
}

func TestProfileNamesSorted(t *testing.T) {
	t.Parallel()

	names := ProfileNames()
	if !slices.IsSorted(names) || len(names) != len(Profiles) {
		t.Errorf("names = %q", names)
	}
}
