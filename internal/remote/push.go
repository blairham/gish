package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Sentinel failures. Every one of them means the same thing to the
// caller — fall back to plain ssh with a notice — but they are distinct
// so the notice can say something the user could act on.
var (
	errNoExecDir  = errors.New("no directory on the remote is both writable and executable")
	errNoBinary   = errors.New("no gish binary available for the remote platform")
	errVerifyFail = errors.New("pushed binary failed verification")
)

// Payload is a file destined for the remote, named by its own content.
// Content-addressing is doing three jobs at once: upgrades are free (a
// new build is a new name, so there is nothing to invalidate), a
// truncated transfer can never be mistaken for a good one, and a
// co-tenant on a shared box cannot swap the file under us without
// changing the name we exec.
type Payload struct {
	Name string // e.g. gish-3f2a…  (content-addressed)
	Sum  string // sha256, hex
	Size int64
	Mode string // octal, as chmod takes it
	open func() (io.ReadCloser, error)
}

// LocalBinary describes the gish executable to send. When the remote
// platform matches this process's, that is this very binary — the
// self-copy case, which needs no cache and no download.
func LocalBinary(path string) (Payload, error) {
	f, err := os.Open(path) //nolint:gosec // the path is gish's own executable or the user's cache
	if err != nil {
		return Payload{}, err
	}
	defer f.Close() //nolint:errcheck // read-only

	info, err := f.Stat()
	if err != nil {
		return Payload{}, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return Payload{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	return Payload{
		Name: "gish-" + sum[:16],
		Sum:  sum,
		Size: info.Size(),
		Mode: "0700",
		open: func() (io.ReadCloser, error) { return os.Open(path) }, //nolint:gosec // same path
	}, nil
}

// ConfigPayload wraps an in-memory config bundle as a pushable file.
// Mode 0600: it is not executable, and it is the one thing here that
// could carry anything the user considers private.
func ConfigPayload(prefix string, content []byte) Payload {
	sum := sha256.Sum256(content)
	hexSum := hex.EncodeToString(sum[:])
	return Payload{
		Name: prefix + "-" + hexSum[:16],
		Sum:  hexSum,
		Size: int64(len(content)),
		Mode: "0600",
		open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(string(content))), nil },
	}
}

// Push streams a payload to dir on the remote and verifies it landed
// intact before it is given its real name.
//
// `cat > file` rather than scp or sftp: hardened boxes routinely disable
// sftp-server, and every one of them has cat. ssh's own -C does the
// compression, so there is no dependency on remote gzip or zstd either.
// Nothing is base64'd — that is 33% bloat and a line-length problem on a
// channel that is already binary-clean.
//
// Write-to-.partial-then-rename is the same discipline the rest of gish
// uses for state files: a dropped connection must leave no file that a
// later run would happily exec.
func Push(ctx context.Context, t Transport, dir string, p Payload, hashCmd string) error {
	src, err := p.open()
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck // read-only

	partial := dir + "/" + p.Name + ".partial"
	final := dir + "/" + p.Name

	script := fmt.Sprintf(`set -eu
umask 077
cat > %[1]s
chmod %[2]s %[1]s
`, shellQuote(partial), p.Mode)
	if _, err := t.Run(ctx, script, src); err != nil {
		// Leave nothing behind for a later run to trip over.
		_, _ = t.Run(ctx, "rm -f "+shellQuote(partial), nil) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("push %s: %w", p.Name, err)
	}

	if err := verify(ctx, t, partial, p, hashCmd); err != nil {
		_, _ = t.Run(ctx, "rm -f "+shellQuote(partial), nil) //nolint:errcheck // best-effort cleanup
		return err
	}

	if _, err := t.Run(ctx, fmt.Sprintf("mv -f %s %s", shellQuote(partial), shellQuote(final)), nil); err != nil {
		return fmt.Errorf("install %s: %w", p.Name, err)
	}
	return nil
}

// verify checks the landed file against the payload. The threat model is
// worth stating, because it is narrow: this is not a defense against a
// rooted host — that box already owns the session the moment we exec
// there. It defends against a dropped connection leaving a truncated
// binary, and against a co-tenant on a shared box writing our cache dir.
func verify(ctx context.Context, t Transport, path string, p Payload, hashCmd string) error {
	if hashCmd == "" {
		// Neither sha256sum nor shasum. Size is what is left; combined
		// with content-addressed naming it still catches truncation.
		out, err := t.Run(ctx, fmt.Sprintf("wc -c < %s", shellQuote(path)), nil)
		if err != nil {
			return fmt.Errorf("%w: %w", errVerifyFail, err)
		}
		if strings.TrimSpace(string(out)) != fmt.Sprint(p.Size) {
			return fmt.Errorf("%w: %s is %s bytes, expected %d", errVerifyFail, p.Name, strings.TrimSpace(string(out)), p.Size)
		}
		return nil
	}
	out, err := t.Run(ctx, fmt.Sprintf("%s %s | cut -d' ' -f1", hashCmd, shellQuote(path)), nil)
	if err != nil {
		return fmt.Errorf("%w: %w", errVerifyFail, err)
	}
	if got := strings.TrimSpace(string(out)); got != p.Sum {
		return fmt.Errorf("%w: %s hashed %s, expected %s", errVerifyFail, p.Name, got, p.Sum)
	}
	return nil
}

// shellQuote wraps a value for /bin/sh. Single quotes with the standard
// '\” escape: every path here is ours or the remote's own $HOME, but a
// home directory with a space in it is not exotic.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
