# koi as your coding agent's shell

The 2026 version of "the command I pasted doesn't work in my shell" is
**"my coding agent's subshell doesn't work in my shell."** Coding agents
spawn a shell for every command, and they spawn the one they can
recognize — not the one you chose. With a non-POSIX login shell the
result is silent: your functions, aliases and PATH edits are simply not
there inside the agent's subshell, and with some agents you get a
syntax-error preamble on every command.

The workaround circulating in the wild is the dual-shell split — one
shell for humans, another for agents. That partition is exactly what
koi exists to collapse.

**The claim, and it is two-sided:**

1. An agent pointed at koi gets your **real environment** — functions,
   aliases, PATH — with no syntax errors, through the invocation shapes
   agents actually use.
2. `koi --sandbox` **confines what the agent can touch**. No agent CLI
   can make that claim about itself, because a process cannot meaningfully
   sandbox itself.

The first half is enforced by a CI gate (`internal/compat/agent.go`),
not asserted — see [the gate](#the-gate) below. #11475-class bugs mean
even `SHELL=koi` is not always respected, so this page states what is
tested, what is configured, and what is neither.

## The recipe that works everywhere

Agents select a shell by looking for `bash` or `zsh` — often by grepping
their own `$SHELL`. A binary named `koi` is invisible to them. So give
koi a name they recognize:

```sh
ln -s "$(command -v koi)" ~/.local/bin/koi-agent-bash
export SHELL="$HOME/.local/bin/koi-agent-bash"
```

That is the whole configuration, and it does three things at once:

- **`$SHELL` matches `*bash*`**, so a harness that greps for bash finds
  it and spawns it.
- **The `koi-agent` prefix turns on `--sandbox workspace`** (#215): a
  binary invoked as `koi-agent` or `koi-agent-<suffix>` starts
  sandboxed, because a harness is handed a path to a shell and has
  nowhere to put a flag. An explicit `--sandbox`, including
  `--sandbox none`, still wins.
- **Your rc still loads** for `-i` invocations, so the agent's subshell
  has your aliases and functions.

Verified on this machine: the symlink is recognized by a `$SHELL` grep,
`-lc` runs normally through it, and a write outside the workspace is
refused (`open /etc/…: permission denied`).

## Per-agent notes

| agent | how it picks a shell | state |
| --- | --- | --- |
| **Claude Code** | sources a generated *shell snapshot* before every command, produced by running your shell; spawns `<shell> -c -l '<generator>'` | the snapshot generator's constructs — `>\|`, `declare -F`, `shopt -p` — are all in the gate, captured from the real generator (#215) |
| **Codex CLI** | spawns `$SHELL -lc` | the invocation shape is in the gate; the CLI's own settings are **not verified here** |
| **Gemini CLI** | spawns a shell per tool call | same: shape tested, settings **not verified here** |

The middle column is what the gate covers. **The right column is the
honest part**: koi's side of the contract is tested, and each CLI's own
configuration keys are not, because CI has none of these tools installed
and authenticated. `internal/compat.DetectedAgents` reports which are
present wherever the suite runs, so "we tested against the real thing"
is never claimed on a machine that did not have it.

Known upstream issues worth reading before assuming koi is at fault —
these are agents ignoring the configured shell, not shells failing:
`anthropics/claude-code#11475` (agents ignore the user's default shell),
`#7490` and `#13144` (shell not configurable), `#19983` (installers
emitting bash-style PATH exports at users whose shell never reads them).

## When an agent hardcodes `bash -c` anyway

**It still works. That is the entire point.**

An agent that ignores `$SHELL` and runs `/bin/bash -c '…'` gets real
bash, and nothing about koi interferes. You lose the sandbox for those
commands — koi is not in that path at all — but nothing breaks, no
syntax error appears, and no configuration is required to make that
true. This is the difference between a compatible shell and a
replacement one: the failure mode of "the agent didn't use my shell" is
*nothing happens*, rather than *every command errors*.

The gate pins this too, by running a non-trivial bash script through
both shells and requiring them to agree.

## The gate

`internal/compat/agent.go` runs the invocation shapes harnesses actually
write — the same argv, rc and profile through real bash and through
koi — and requires them to agree. bash is the oracle; nothing encodes
what we think bash ought to do.

Covered: clustered `-lc`; options after the command operand (`-c -l`,
captured verbatim from Claude Code); the three snapshot-generator
constructs; functions, aliases and PATH reaching the subshell; exported
variables surviving the hop; no preamble on a clean run; and the two
places koi deliberately differs.

**Two deliberate divergences**, asserted rather than tolerated, because
they are the ones most likely to be "fixed" by someone making a table
green:

| case | behavior | why |
| --- | --- | --- |
| `echo $0` | `koi`, where bash prints its own path | koi claims bash's *interface*, not its identity (#120). A harness that needs to know can see it. |
| `BASH_VERSION` / `BASH_VERSINFO` | answered | tools branch on these to pick an implementation; fzf picks its Ctrl-T path on `BASH_VERSINFO[0] < 4`, and unset reads as 0 |

### Two bugs this gate found on its first run

Both were in exactly the path an agent uses, and both failed silently —
which is why "we support agents" was not a safe thing to claim until
something ran it:

- **`koi -ic 'll'` did not expand aliases.** The rc loaded, the alias
  was recorded, `alias ll` printed it, and the command still answered
  `127: executable file not found`. `bash -ic 'll'` runs it. This is the
  #163 alias trap on the agent path: everything looks configured and
  nothing works. Expansion stays off for non-interactive runs, which is
  bash's rule and what keeps a script's meaning independent of who runs
  it.
- **`$0` answered `koi -c`.** `$0` is the parse name, and the parse name
  carried the flag, making koi the only shell whose `$0` contains an
  option. bash answers `bash`. It now answers `koi`.

### What a full day of agent use found

The first-run bugs above came from reading a harness's snapshot
generator. The next round came from the other direction: pointing a real
Claude Code session at koi for a working day, then running a bash-vs-koi
differential sweep over the constructs it actually used — 128 snippets,
each run direct **and** through `eval`, because a fix that lives in the
REPL's parse layer is invisible to `eval`, and one of them was.

Two things about the method are worth writing down, because both nearly
cost a correct result:

- **The oracle has to be a bash worth comparing against.** macOS ships
  bash 3.2.57 (2007), which lacks `${v^^}`, associative arrays,
  `local -n`, `mapfile` and `case ;;&`. The first sweep used it and
  reported 100 divergences, 23 of them koi being *more* capable than the
  oracle — and that noise hid three real bugs. Against bash 5.3 the same
  corpus reports 75.
- **The suite cannot be written in the shell under test.** The
  bash-driven first draft wrote itself to disk with
  `cat > probe.sh <<'SH'`, and the heredoc gap below corrupted its own
  source into a file that would not parse. The payloads now live outside
  the shell as data.

What came out, and what each one costs, is generated from the corpus
rather than written here — `make agent-gate` republishes it, and a case
that starts passing while still marked fails the build:

<!-- BEGIN generated agent gaps -->

**28 of 28 cases agree with bash 5.3.15(1)-release.** 0 open gaps, 0 unfiled failures.

No open gaps: every case an agent's shell hits agrees with bash.

<!-- END generated agent gaps -->

## What this does not claim

- Not that every agent respects `$SHELL` — several documented bugs say
  otherwise, and the recipe above works *around* that rather than fixing
  it.
- Not that the sandbox contains a determined adversary. It is
  least-privilege confinement for a tool you are choosing to run
  (`docs/design.md`), on the platform enforcement available (macOS
  Seatbelt, Linux Landlock best-effort by kernel ABI); `doctor` reports
  the ceiling on the machine you are on.
- Not that koi is an agent. koi *hosts* agents — `??` and `explain`
  are the advertised AI surface, and the `agent` builtin is frozen and
  unadvertised (#111).
