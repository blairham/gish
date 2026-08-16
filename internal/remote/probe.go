package remote

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The probe: everything gish needs to know about the far side, in one
// round trip, bounded by a timeout. If it cannot answer in time the
// caller falls back to plain ssh — the pitch is the 2AM incident box, so
// being slower to get a shell than not having the feature is the one
// unacceptable outcome.

// ProbeTimeout bounds the whole probe. Deliberately short: a working
// connection answers in well under a second, and a connection that does
// not is one the user wants to spend on plain ssh instead.
const ProbeTimeout = 2 * time.Second

// Probe is what the remote told us about itself.
type Probe struct {
	OS   string // uname -s, lowercased: linux, darwin…
	Arch string // GOARCH-normalized: amd64, arm64…
	// Dir is the first candidate directory that is both writable and
	// actually able to execute a file.
	Dir string
	// HashCmd is a shell command that prints the sha256 of "$1", or
	// empty when the remote has neither sha256sum nor shasum.
	HashCmd string
	// Present reports that the content-addressed binary is already there
	// and passed verification — the repeat-visit fast path.
	Present bool
}

// candidateDirs is the fallback chain, in preference order: a real
// cache dir first, then the places that exist when $HOME is read-only or
// full. Each entry is a shell word, expanded on the remote — gish does
// not know the remote's $HOME or uid, and asking would cost a round
// trip.
//
// A var rather than a const so tests can point the chain at a temp
// directory instead of writing to the real /tmp and /dev/shm.
var candidateDirs = []string{
	`"${HOME:-}/.cache/gish"`,
	`"${XDG_RUNTIME_DIR:-}/gish"`,
	`"/dev/shm/gish-$uid"`,
	`"/tmp/gish-$uid"`,
}

// probeScript asks all four questions at once. Written to the POSIX
// subset on purpose: the remote's $SHELL may be anything, and this runs
// under whatever /bin/sh is.
//
// The exec test is the part that cannot be inferred. `/tmp` mounted
// noexec is standard on CIS-benchmarked hosts — precisely the hardened
// box in the pitch — and a directory being writable says nothing about
// whether a binary in it can run. So: write ~20 bytes, chmod +x, run it,
// and check the status it exits with.
const probeScript = `
set -u
os=$(uname -s 2>/dev/null | tr '[:upper:]' '[:lower:]')
arch=$(uname -m 2>/dev/null)
echo "os=$os"
echo "arch=$arch"

hash_cmd=
if command -v sha256sum >/dev/null 2>&1; then hash_cmd=sha256sum
elif command -v shasum >/dev/null 2>&1; then hash_cmd="shasum -a 256"
fi
echo "hashcmd=$hash_cmd"

uid=$(id -u 2>/dev/null || echo 0)
dir=
for cand in %[4]s; do
	case "$cand" in /gish|/gish/*) continue ;; esac
	mkdir -p "$cand" 2>/dev/null || continue
	chmod 700 "$cand" 2>/dev/null || true
	probe="$cand/.gish-exectest"
	printf '#!/bin/sh\nexit 7\n' > "$probe" 2>/dev/null || continue
	chmod 700 "$probe" 2>/dev/null || { rm -f "$probe"; continue; }
	code=0
	"$probe" >/dev/null 2>&1 || code=$?
	rm -f "$probe" 2>/dev/null
	if [ "$code" = 7 ]; then dir="$cand"; break; fi
done
echo "dir=$dir"

present=no
if [ -n "$dir" ] && [ -x "$dir/%[1]s" ]; then
	if [ -n "$hash_cmd" ]; then
		got=$($hash_cmd "$dir/%[1]s" 2>/dev/null | cut -d' ' -f1)
		[ "$got" = "%[2]s" ] && present=yes
	else
		# No hash tool: size is the only cheap integrity signal, and it
		# does catch the failure that actually happens — a dropped
		# connection leaving a truncated binary that gets exec'd forever.
		size=$(wc -c < "$dir/%[1]s" 2>/dev/null | tr -d ' ')
		[ "$size" = "%[3]d" ] && present=yes
	fi
fi
echo "present=$present"
`

// RunProbe asks the remote about itself. name is the content-addressed
// file name to look for, sum its expected sha256, and size its expected
// byte count (the fallback check when the remote has no hash tool).
func RunProbe(ctx context.Context, t Transport, name, sum string, size int64) (Probe, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	out, err := t.Run(ctx, fmt.Sprintf(probeScript, name, sum, size, strings.Join(candidateDirs, " ")), nil)
	if err != nil {
		return Probe{}, fmt.Errorf("probe: %w", err)
	}
	var p Probe
	for line := range strings.Lines(string(out)) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "os":
			p.OS = value
		case "arch":
			p.Arch = normalizeArch(value)
		case "dir":
			p.Dir = value
		case "hashcmd":
			p.HashCmd = value
		case "present":
			p.Present = value == "yes"
		}
	}
	if p.OS == "" || p.Arch == "" {
		return p, fmt.Errorf("probe: remote did not identify itself (uname said %q)", strings.TrimSpace(string(out)))
	}
	if p.Dir == "" {
		return p, errNoExecDir
	}
	return p, nil
}

// normalizeArch maps uname -m onto GOARCH. uname reports the hardware;
// Go names the target, and the two disagree on the most common
// architecture in the world.
func normalizeArch(m string) string {
	switch strings.TrimSpace(m) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armv6l":
		return "arm"
	case "i386", "i686":
		return "386"
	case "riscv64":
		return "riscv64"
	case "ppc64le":
		return "ppc64le"
	case "s390x":
		return "s390x"
	}
	return strings.TrimSpace(m)
}
