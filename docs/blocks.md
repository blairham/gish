# blocks: design

Warp's most-praised feature is blocks — command and output as one
navigable unit. The recurring request is for that *without* a
closed-source, login-gated terminal, and the observation that makes it
gish's to take is that **blocks are a shell feature awkwardly
implemented in a terminal**: the shell already knows command
boundaries, exit codes, durations, and cwd. No local-first shell has
shipped it ([#99](https://github.com/blairham/gish/issues/99)).

This document is the design, and the reason the feature ships in stages
rather than all at once.

## What ships today: OSC 133 semantic marks

gish emits the standard semantic prompt sequences on every command:

| mark | meaning |
| --- | --- |
| `OSC 133;A` | prompt starts |
| `OSC 133;B` | prompt ends, user input starts |
| `OSC 133;C` | command output starts |
| `OSC 133;D;N` | command finished with status N |

That is not a consolation prize. kitty, WezTerm, Ghostty, iTerm2, and
VS Code already implement scroll-to-previous-prompt,
select-command-output, and click-to-rerun on top of these marks — so a
gish user gets block navigation *today*, in the terminal they already
run, without gish becoming a terminal. `doctor` names the terminal and
what it supports. `GISH_SEMANTIC_MARKS=off` disables them for terminals
that render unknown OSC sequences badly.

Implementation note worth keeping: the A/B marks wrap the *prompt
string*, not the render call. The editor's renderer already treats
escape sequences as zero-width and atomic, so the marks survive
redraws, wrapping, and multi-line prompts with no special cases.

## What does not ship yet, and why

The rest of blocks needs **output capture**, and capture in a shell —
as opposed to a terminal — runs into a constraint worth stating
plainly, because it decides the whole design.

Today a foreground child gets the *real terminal file descriptors*, and
its process group is handed the terminal (`SysProcAttr.Foreground`,
`Ctty`) so that Ctrl-C, Ctrl-Z, and window-size signals reach it
correctly. To capture output, gish must sit in the middle. There are
exactly two ways, and both cost something:

**A pipe.** Cheap and simple, and it breaks the world: `isatty` goes
false for the child, so `git log` stops paging, `ls` stops coloring,
`docker` stops drawing progress bars, and every well-behaved program
switches to its dumb output mode. A capture feature that silently
changes program behavior is worse than no capture feature.

**A PTY.** The child still sees a terminal, so behavior is unchanged —
this is what Warp does, because Warp *is* the terminal.

The decision, stated in advance so it is not re-litigated: **PTY,
opt-in, foreground commands only.** A pipe is disqualified by the
behavior change; capture defaults off; and background jobs keep the
current path, because a background job that loses its terminal
semantics to satisfy a history feature is a bad trade.

### What the cost turned out to be — corrected

This document originally predicted that a PTY meant gish "must then own
what the terminal owned: forwarding window-size changes, relaying
signals, and interleaving its copy loop with the existing job-control
handoff." **That is true of one shape and not the one that shipped**, and
the difference is worth recording because it is the reason stage 2 was
tractable at all.

That cost is real when the PTY becomes the child's **controlling
terminal** — what `script(1)` does, and what a terminal emulator does.
Then signals really do stop arriving from the real terminal and gish
has to relay them.

The shipped shape gives the child the PTY for **stdout only**. Its
stdin, its stderr, and its controlling terminal stay exactly as before.
Probed against a real child before building on it:

    isatty_stdout=yes     # color and paging preserved
    cols=100              # size queries answer correctly
    ctty=ttys006          # controlling terminal: still the real one

Because the controlling terminal never changes, Ctrl-C, Ctrl-Z, SIGWINCH
and the pre-exec foreground-group handoff keep working **because nothing
about them changed** — the kernel still delivers them from the real
terminal to the child's process group. Job control is preserved by
construction rather than reimplemented. The race-sensitive handoff is
untouched.

What gish does own is small: keep the PTY's window size in step with the
real terminal (so a program asking its width on stdout gets the truth),
and copy master → screen while teeing into a size-capped ring.

### stderr is not captured, and that was measured

The first implementation captured stdout *and* stderr. Under it, `less`
painted a screenful, never entered raw mode, and every keystroke meant
for the pager was echoed onto the shell's line instead — the shell
appeared to hang. Without capture the same `less` worked. Leaving stderr
alone fixed it, and `vim` then opened, edited and `:wq`-ed normally too.

The reason is that full-screen programs do their terminal control —
`tcgetattr`/`tcsetattr`, raw mode — on **fd 2**, which is standard
practice precisely because stdout is so often redirected. Hand such a
program a pty as stderr and it configures the pty while the real
terminal stays cooked.

So the honest trade is: **a command's errors are not captured.** That is
the price of "programs behave exactly as they do today", which is the
bar this feature has to clear to be worth having. It is also why this
was worth testing with a real pager rather than shipping on the strength
of unit tests — the mechanism was correct and the *policy* was wrong.

Four costs remain, all stated:

- stderr is not captured (above).
- **Builtins are not captured.** Capture substitutes an *external
  child's* stdout, and a builtin never becomes a child — `printf` and
  `echo` are the interpreter writing straight to the terminal, while
  `/bin/echo` goes through the exec path and is captured. For a blocks
  feature this is mostly harmless (the output worth keeping comes from
  real programs) but it is a real hole, and closing it means routing the
  whole line's stdout through the pty rather than each child's, which is
  a larger change than stage 2 should carry.
- A program that writes straight to `/dev/tty` bypasses capture. That is
  correct — writing to `/dev/tty` is precisely how a program says "put
  this on the terminal, not in the output stream."
- Output crosses one extra copy on its way to the screen.

Redirected output is never captured: `cmd > file` and pipeline stages
already have somewhere to go, and routing them through a PTY would both
capture what the user asked to be sent elsewhere and mangle it, since
the line discipline translates `\n` to `\r\n`. Verified by byte count.

## Staged plan

1. **OSC 133 marks** — *done*. Terminal-native block navigation now.
2. **PTY capture path** — *done*, behind `config blocks on`
   (`internal/capture`, wired in `internal/jobs`). Foreground commands
   run through a PTY that gish copies to the real terminal while teeing
   into a size-capped ring that keeps the *tail* — a failed build's error
   is at the end. Window size forwarded on SIGWINCH; job control
   unchanged, by construction (above). `less` and `vim` verified under
   a real pty: both behave as they do uncaptured. `docker build` and
   other long-running progress UIs still want a look under real use.
3. **Block records** — *done*. A history entry gains a `block`
   reference; the output lives under `$XDG_DATA_HOME/gish/blocks/`,
   content-addressed (so running a command twice costs one file, and a
   ref can never escape its directory). Retention is age, then count,
   then total bytes — bytes last, so a size cap never evicts recent
   output to keep old output.

   The #10 rules run over output **as it is written**, so a leaked
   blocks directory cannot leak a token and there is no scrubbing step
   for a later reader to forget. Crucially the *posture* differs from a
   command line: output is **redacted**, not rejected. Dropping a
   200KB build log because one line echoed a token would destroy
   exactly what the user wanted to see, whereas dropping a command line
   is proportionate because there the secret essentially *is* the
   command. Both postures now live in one file (`SecretReason` /
   `RedactOutput`), and which applies is a property of what is being
   written rather than of who is writing it.
4. **Surfaces** — *`blocks` builtin done*. `blocks` lists commands that
   have captured output, `blocks show N` replays one, and `blocks
   search TERM` finds commands **by what they printed** — the thing
   history structurally cannot do, and the reason to keep the output at
   all. A block that shows says so when its output was truncated or
   redacted, rather than presenting a partial or doctored log as whole.

   Still to come: the #100 picker as the front end, and output previews
   in Ctrl-R results.

## Why this order

Stage 1 is free and immediately useful. Stage 2 is the whole risk of
the feature, so it is isolated and opt-in. Stages 3 and 4 are
mechanical once capture is trustworthy, and they are also what
[session restore](https://github.com/blairham/gish/issues/103) and the
"history should capture output" asks from the atuin threads want.

Shipping stage 1 alone is deliberate: a `blocks` command that listed
commands without their output would be `history` wearing a new name,
and this feature only earns its flagship billing when the output is
really there.
