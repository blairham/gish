package remote

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// Options configure one `koi ssh` invocation.
type Options struct {
	// Host is the ssh destination, [user@]host.
	Host string
	// SSHArgs are the user's own ssh flags, passed through untouched.
	SSHArgs []string
	// Mode is KOI_SSH_BRING: ask, always, or never.
	Mode string
	// Interactive reports whether there is a human to ask.
	Interactive bool
	// Ephemeral wipes the dropped files when the session ends.
	Ephemeral bool
	// Stderr receives the one-line notices. Never stdout: `koi ssh` is
	// used in pipelines, and a notice on stdout would corrupt them.
	Stderr io.Writer
	// Stdin is where the bring question is read from.
	Stdin io.Reader
	// Prompt overrides the bring question (tests supply their own).
	Prompt func(string) (bool, error)
}

// Session is one prepared remote koi: what was pushed and where.
type Session struct {
	Probe   Probe
	Binary  Payload
	Config  Payload
	Command string // the exact remote command line
	Pushed  bool   // false on the repeat-visit fast path
}

// Bring probes the remote, drops koi if policy and platform allow, and
// returns the session to exec. Every failure it can meet is returned as
// an error whose text is one line fit for stderr — the caller's job is
// then to run plain ssh, not to interpret this.
func Bring(ctx context.Context, t Transport, opts Options) (*Session, error) {
	prompt := opts.Prompt
	if prompt == nil && opts.Interactive {
		prompt = AskOnTerminal(opts.Stdin, opts.Stderr)
	}
	bring, _, err := Decide(opts.Host, opts.Mode, opts.Interactive, prompt)
	if err != nil {
		return nil, err
	}
	if !bring {
		return nil, fmt.Errorf("not bringing koi to %s (KOI_SSH_BRING=%s)", opts.Host, orDefault(opts.Mode, BringAsk))
	}

	// The probe needs the payload name to look for, and the payload
	// needs the probe's platform — so the first probe asks only about the
	// platform, and the presence check rides the second one. To keep the
	// promise of one round trip, the common case is resolved in one call:
	// ask about the *local* binary's identity, which is the right answer
	// whenever the platforms match, and that is the overwhelming
	// majority of visits.
	self, selfErr := LocalBinary(selfPath())
	p, err := RunProbe(ctx, t, self.Name, self.Sum, self.Size)
	if err != nil {
		return nil, err
	}

	bin := self
	if p.OS != goos() || p.Arch != goarch() || selfErr != nil {
		path, err := BinaryFor(p)
		if err != nil {
			return nil, err
		}
		if bin, err = LocalBinary(path); err != nil {
			return nil, err
		}
		// Cross-platform: the presence answer above was about the wrong
		// file, so re-ask. Rare enough to be worth the extra trip.
		if p, err = RunProbe(ctx, t, bin.Name, bin.Sum, bin.Size); err != nil {
			return nil, err
		}
	}

	cfg := ConfigPayload("rc", Bundle(os.Getenv, EnvNames()))
	sess := &Session{Probe: p, Binary: bin, Config: cfg}

	if !p.Present {
		if err := Push(ctx, t, p.Dir, bin, p.HashCmd); err != nil {
			return nil, err
		}
		sess.Pushed = true
	}
	// The config is small and changes whenever the user retunes their
	// prompt, so it is always pushed; content-addressing makes a repeat
	// push of an unchanged bundle a no-op in practice.
	if err := Push(ctx, t, p.Dir, cfg, p.HashCmd); err != nil {
		return nil, err
	}
	if err := dropREADME(ctx, t, p.Dir); err != nil {
		return nil, err
	}

	sess.Command = remoteCommand(p.Dir, bin.Name, cfg.Name, opts.Ephemeral)
	return sess, nil
}

// remoteCommand is what ssh -t runs. `exec` so no shell parent lingers
// under the session, and --remote-session so the far side knows it was
// brought rather than installed.
func remoteCommand(dir, bin, cfg string, ephemeral bool) string {
	run := fmt.Sprintf("%s --remote-session --rc %s",
		shellQuote(dir+"/"+bin), shellQuote(dir+"/"+cfg))
	if ephemeral {
		// Ephemeral cannot `exec`: something has to outlive the shell to
		// do the cleanup. The trap covers a session that ends by signal
		// rather than by exit.
		return fmt.Sprintf("trap 'rm -rf %s' EXIT INT TERM HUP; %s", shellQuote(dir), run)
	}
	return "exec " + run
}

// dropREADME writes the explain-yourself file. Named plainly rather
// than by hash: its whole purpose is to be found and read by someone who
// is not us.
func dropREADME(ctx context.Context, t Transport, dir string) error {
	script := fmt.Sprintf("cat > %s", shellQuote(dir+"/README"))
	if _, err := t.Run(ctx, script, strings.NewReader(readmeText)); err != nil {
		return fmt.Errorf("drop README: %w", err)
	}
	return nil
}

// Uninstall removes everything koi left on a host. One command, because
// "nothing persists beyond the dropped binary + config" is only a real
// promise if undoing it is trivial.
func Uninstall(ctx context.Context, t Transport) (removed []string, err error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout*3)
	defer cancel()

	// Sweep every candidate directory, not just the one this platform
	// would pick today: the chain can resolve differently across visits
	// (a /tmp that used to be noexec, an $XDG_RUNTIME_DIR that appeared).
	script := fmt.Sprintf(`
set -u
uid=$(id -u 2>/dev/null || echo 0)
for cand in %s; do
	case "$cand" in /koi|/koi/*) continue ;; esac
	if [ -d "$cand" ]; then
		rm -rf "$cand" 2>/dev/null && echo "$cand"
	fi
done
`, strings.Join(candidateDirs, " "))
	out, err := t.Run(ctx, script, nil)
	if err != nil {
		return nil, fmt.Errorf("uninstall: %w", err)
	}
	for line := range strings.Lines(string(out)) {
		if s := strings.TrimSpace(line); s != "" {
			removed = append(removed, s)
		}
	}
	return removed, nil
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
