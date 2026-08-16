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
this is what Warp does, because Warp *is* the terminal. The cost is
that gish must then own what the terminal owned: forwarding window-size
changes, relaying signals, and interleaving its copy loop with the
existing job-control handoff, which is race-sensitive by design (the
foreground group is set pre-exec, deliberately, to avoid a race).

The decision, stated in advance so the next session does not re-litigate
it: **PTY, opt-in, foreground commands only.** A pipe is disqualified by
the behavior change; capture defaults off until the PTY path has proven
itself against job control; and background jobs keep the current path,
because a background job that loses its terminal semantics to satisfy a
history feature is a bad trade.

## Staged plan

1. **OSC 133 marks** — *done*. Terminal-native block navigation now.
2. **PTY capture path**, behind `config blocks on`: foreground commands
   run through a PTY that gish copies to the real terminal while teeing
   into a size-capped ring buffer. Window size forwarded on SIGWINCH;
   job control unchanged. The success bar is that `vim`, `less`, `git
   log`, and `docker build` behave exactly as they do today.
3. **Block records**: a history entry (already metadata-rich JSONL)
   plus an output reference under `$XDG_DATA_HOME/gish/blocks/`, with
   retention caps. The #10 secret-scrub rules run over captured output
   before anything persists — output is a *new* place secrets can land,
   and the existing rules only cover command lines.
4. **Surfaces**: a `blocks` builtin (list, search, show, re-run) over
   the #100 picker; then output previews in Ctrl-R results, which is
   where the feature stops being a curiosity and starts being the
   reason to keep gish.

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
