// Package sandbox implements least-privilege execution for commands
// (#21), in the spirit of nono: the shell already owns the exec path,
// so sandbox enforcement is one more step on it — policy resolution →
// platform enforcement → spawn. No wrapper binary on PATH, no shims.
//
// Mechanism: a sandboxed command is rewritten to re-exec koi itself in
// a private mode (`koi __sandbox-exec <policy-json> -- cmd …`). The
// child filters its environment per policy, applies the platform
// enforcement — macOS Seatbelt via /usr/bin/sandbox-exec, Linux
// Landlock — and execs the real command. One wrapper, both platforms,
// and the normal job-control path sees an ordinary child process.
//
// v1 restricts filesystem writes and TCP networking; reads stay open
// (read-restriction profiles can layer on the same schema later).
// Windows is deferred with the #47 milestone.
package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
)

// ExecFlag is the private argv[1] marking the re-exec child. Not part
// of the CLI surface.
const ExecFlag = "__sandbox-exec"

// Policy is the platform-independent sandbox schema. Named profiles
// resolve to one; the same schema later carries AI-tagged command
// defaults (#20) and per-plan agent policies (#34).
type Policy struct {
	Name string `json:"name"`
	// WriteAll leaves filesystem writes unrestricted. Otherwise writes
	// are limited to WritePaths (plus the temp dir and /dev, which are
	// always included — commands must keep their plumbing).
	WriteAll   bool     `json:"write_all"`
	WritePaths []string `json:"write_paths,omitempty"`
	// WriteCwd adds the working directory to WritePaths at wrap time.
	WriteCwd bool `json:"write_cwd"`
	// Network allows outbound/inbound TCP. Enforcement is best-effort
	// on Linux (Landlock ABI 4+ restricts classic TCP only).
	Network bool `json:"network"`
	// EnvAll passes the full environment through. Otherwise the child
	// keeps only the allowlist — the filtered-env invariant applied to
	// sandboxed exec.
	EnvAll bool `json:"env_all"`
}

// Profiles are the built-in policies, strictest last.
var Profiles = map[string]Policy{
	// readonly: the filesystem cannot be changed outside tmp.
	"readonly": {Name: "readonly", Network: true, EnvAll: true},
	// workspace: write the working tree and tmp, nothing else.
	"workspace": {Name: "workspace", WriteCwd: true, Network: true, EnvAll: true},
	// no-network: full filesystem, no TCP.
	"no-network": {Name: "no-network", WriteAll: true, Network: false, EnvAll: true},
	// isolated: readonly + no network + allowlisted env.
	"isolated": {Name: "isolated", Network: false, EnvAll: false},
}

// ProfileNames lists the built-in profile names, sorted.
func ProfileNames() []string {
	names := make([]string, 0, len(Profiles))
	for name := range Profiles {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Resolve returns the named profile with cwd folded in per WriteCwd —
// the concrete policy the child enforces.
func Resolve(name, cwd string) (Policy, error) {
	p, ok := Profiles[name]
	if !ok {
		return Policy{}, fmt.Errorf("unknown sandbox profile %q (profiles: %s)",
			name, strings.Join(ProfileNames(), " | "))
	}
	if p.WriteCwd && cwd != "" {
		p.WritePaths = append(slices.Clone(p.WritePaths), cwd)
	}
	return p, nil
}

// WrapArgv rewrites a command's argv to run under policy via the
// re-exec child. self is the koi binary (os.Executable()).
func WrapArgv(self string, p Policy, argv []string) ([]string, error) {
	blob, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(argv)+3)
	out = append(out, self, ExecFlag, string(blob), "--")
	return append(out, argv...), nil
}

// envAllowlist is what a non-EnvAll child keeps: the plugin-command
// allowlist plus the process-level basics a spawned command needs.
var envAllowlist = []string{"PATH", "HOME", "TERM", "LANG", "LC_ALL", "USER", "TMPDIR", "PWD", "SHLVL"}

// filterEnv applies the policy's env posture to environ-shaped entries.
func filterEnv(environ []string, p Policy) []string {
	if p.EnvAll {
		return environ
	}
	out := make([]string, 0, len(envAllowlist))
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if ok && slices.Contains(envAllowlist, name) {
			out = append(out, kv)
		}
	}
	return out
}

// writeRoots is the effective write-allowed set for a policy: the
// declared paths plus the temp dir and /dev, deduplicated. Deliberately
// the *resolved* temp dir (TMPDIR-aware), not a hardcoded /tmp — the
// carve-out is "the command's scratch space", not "world-shared tmp".
func writeRoots(p Policy) []string {
	roots := []string{os.TempDir(), "/dev"}
	roots = append(roots, p.WritePaths...)
	slices.Sort(roots)
	return slices.Compact(roots)
}

// Exec is the re-exec child entry: parse the policy, filter env, apply
// platform enforcement, and become the command. Only returns on error.
func Exec(policyJSON string, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("sandbox: no command")
	}
	var p Policy
	if err := json.Unmarshal([]byte(policyJSON), &p); err != nil {
		return fmt.Errorf("sandbox: policy: %w", err)
	}
	return enforceAndExec(p, argv, filterEnv(os.Environ(), p))
}
