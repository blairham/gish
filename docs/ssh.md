# `gish ssh`: your shell follows you

"My shell has to be everywhere I ssh" is the single most-cited blocker
to adopting any new shell. It comes up in every new-shell thread, always
with the same scenario — the 2AM incident box, bash-only, that you did
not provision and cannot change — and no incumbent alternative shell has
an answer to it.

gish's answer is not a syntax feature. It is that a single static Go
binary is `scp`-able, and nobody will do that by hand, so gish does it:

```
gish ssh prod-web-3
```

Probe the host, copy one file into a cache directory under your own home
there, open an interactive gish. Repeat visits copy nothing.

## What it does, exactly

1. **One connection, one authentication.** ControlMaster multiplexing,
   with the socket under `$XDG_RUNTIME_DIR`. Probe, push, and session all
   ride it. Without this, anyone behind a bastion or holding a hardware
   key gets three prompts and uninstalls gish that afternoon.
2. **One probe round trip** — `uname -sm`, a writable *and executable*
   directory, whether a hash tool exists, and whether the binary is
   already there. Bounded at two seconds.
3. **Push, verify, rename.** `ssh -C host 'cat > file.partial'`, verify
   the sha256, then `mv` into place.
4. **Exec.** `ssh -t host 'exec <path> --remote-session --rc <path>'`.

## The decisions worth knowing

**The exec test is not optional.** A directory being writable says
nothing about whether a binary in it can run. `/tmp` mounted `noexec` is
standard on CIS-benchmarked hosts — precisely the hardened box in the
pitch. So the probe writes a 20-byte script, `chmod +x`, runs it, and
checks the status. The fallback chain is:

    ~/.cache/gish → $XDG_RUNTIME_DIR/gish → /dev/shm/gish-$UID → /tmp/gish-$UID

Every candidate failing means plain ssh, not a broken session.

**`cat`, not `scp` or `sftp`.** Hardened boxes routinely disable
`sftp-server`; every box has `cat`. ssh's own `-C` does the compression,
so there is no dependency on remote `gzip` or `zstd` either. Nothing is
base64'd — that is 33% bloat and a line-length problem on a channel that
is already binary-clean.

**Content-addressing does three jobs.** The remote file is named by its
own hash, so upgrades are free (a new build is a new name, nothing to
invalidate), a truncated transfer can never be mistaken for a good one,
and a co-tenant on a shared box cannot swap the file under the name we
are about to exec. The threat model is deliberately narrow: this is not
a defense against a rooted host — that box already owns the session — it
is a defense against a dropped connection and a shared `/tmp`.

**Killing on deadline is not returning on deadline.** `exec.CommandContext`
kills the process when the context ends, but `Wait` still drains the
command's stdout and stderr pipes — and a grandchild that outlived its
parent keeps the write end open, so `Wait` blocks long past the timeout.
Every command here therefore sets `WaitDelay`. Without it the 2s probe
budget is advisory, and "never slower than plain ssh" stops being true
in exactly the case the deadline exists for: a wedged remote. The
regression test backgrounds a sleep so the grandchild really does hold
the pipe — a plain `sleep 30` misses the bug wherever `/bin/sh` execs
its last command instead of forking it, which is why this reproduced on
Linux CI and not on macOS.

**Static linking is the premise.** `uname -sm` reports `linux x86_64`
and says nothing about glibc versus musl. A cgo-linked binary lands on
Alpine and fails with an error that *looks like the file is missing*.
The release build sets `CGO_ENABLED=0` everywhere; `doctor` warns if the
gish you are running was not built that way.

**Terminfo is not pushed, on purpose.** The usual cause of "my shell
looks broken over ssh" is a RHEL box with no ghostty/kitty/wezterm
terminfo entry. gish's terminal layer (`internal/term`) is
escape-sequence based end to end — `charmbracelet/ultraviolet` and
`golang.org/x/term`, no terminfo lookup anywhere — so there is nothing to
push. kitty ships a compiled terminfo entry beside its binary precisely
to solve this problem; gish does not have it.

**Cross-platform builds are not downloaded.** Going from an arm64 laptop
to an amd64 server needs a build for the far side, and gish does not
fetch one: [#112](https://github.com/blairham/gish/issues/112) settled
the scope line at *native for the keystroke, prompt, and cd path;
delegate everything else*, and a release downloader carries a package
manager's obligations. Drop a build in
`$XDG_CACHE_HOME/gish/remote-bin/<os>-<arch>/gish` and the error message
tells you the exact `go build` line if you have not.

## What it will never do

- **Never install.** No `chsh`, no remote dotfile edits, no daemon.
  A `~/.bashrc` hook that auto-launches gish is the obvious shortcut and
  it is forbidden: it breaks `rsync` and `scp` the day the remote rc
  prints one byte. The POSIX-clean non-interactive contract is exactly
  what that protects.
- **Never shadow `ssh`.** `gish ssh` is explicit. Auto-hijacking `ssh`
  would drop executables on servers people do not own — that trips
  file-integrity monitoring (Tripwire, Wazuh) and violates change control
  at plenty of shops.
- **Never carry secrets.** What travels is a generated rc file of
  `GISH_*` display settings, mode 0600, named by its hash and passed to
  the remote gish *as a path* — argv is world-readable through `/proc`,
  and `SendEnv` is restricted server-side by `AcceptEnv`. History does
  not travel. Credentials do not travel. Plugins do not travel in v1;
  the deadline-bounded degradation already makes their absence safe.
- **Never be slower than plain ssh.** Every failure lands in
  `ssh host` with one line on stderr. The pitch is the 2AM incident box;
  a feature that delays a shell during an incident is negative value.

## Controls

```
config ssh.bring ask        # default: ask once per host, then remember
config ssh.bring always     # trust yourself
config ssh.bring never      # off

gish ssh --ephemeral host   # wipe the dropped files when the session ends
gish ssh --forget host      # forget the remembered answer, ask again
gish ssh --uninstall host   # remove everything gish left there
```

Remembered per-host answers live in `$XDG_DATA_HOME/gish/ssh-hosts.json`.
It is preference state, not security state: a corrupt file resets to
"ask again", which is the direction that touches fewer hosts.

A `README` is dropped beside the binary explaining what the directory is
and how to remove it. That sounds like a nicety and is not — it is the
difference between a sysadmin shrugging and a sysadmin filing a "what is
this binary on prod" ticket.

## Testing

The transport is an interface (`Run`, `Interactive`, `Close`) with ssh as
implementation #1, and that is load-bearing in two directions. `kubectl
exec` and `docker exec` are the same problem with a different verb. And a
local `sh` transport makes the whole matrix — no writable directory,
`noexec` everywhere, truncated transfer, no `sha256sum`, unsupported
platform, probe timeout, repeat visit, uninstall — testable against a
real POSIX shell under `t.TempDir()`, with no ssh, no network, and no
remote host. See `internal/remote/remote_test.go`.

## Not yet

- ~~OSC-52 clipboard~~ **landed** (#140): `… | clip` copies to the
  clipboard of the terminal you are sitting at, not the machine the shell
  runs on — zero forwarding, zero remote support. `doctor` names your
  terminal and, when it ships OSC 52 switched off, which setting to flip.
- **Local-identity plugins over a reverse-forwarded socket**, so a remote
  session can reach local plugins while AWS SSO tokens, the history
  store, and the credential store never land on the server.
- **A persistent remote daemon** is struck, not deferred. It contradicts
  "nothing persists beyond the dropped binary", and the whole value of
  the single static binary is that there is nothing to run.
