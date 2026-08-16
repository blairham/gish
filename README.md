# gish

**fish-quality interactive UX, and your bash muscle memory and pasted one-liners still work.**

One static Go binary. Syntax highlighting, autosuggestions, a p10k-class
prompt, completions, directory jumping, and version switching are *built
in* — not eight `eval "$(tool init)"` hooks you assembled over five
years.

**Open source, local-first, no account, no telemetry.** The AI features
are opt-in (`??` only), bring-your-own-provider including local models,
and never auto-execute — a composed command lands in your editor buffer
for you to read, edit, and run.

```
                          startup      what it includes
gish                       5.9 ms      theme + highlighting + suggestions + lint, all on
bash (no rc)               5.6 ms      empty rc
zsh (no rc)                9.0 ms      empty rc
zsh + powerlevel10k       87.3 ms      the prompt gish's p10k theme is a port of
zsh (real config)        304.4 ms      a real .zshrc: plugin manager, theme, tool hooks
```

Keystroke latency, measured end to end from byte-in to repaint-out:
**p50 0.2 ms, p99 0.3 ms** — with highlighting, suggestions, and the
footgun linter all running. Full methodology and numbers in
[docs/bench.md](docs/bench.md); the bash-compatibility scoreboard, gaps
included, is in [docs/compat.md](docs/compat.md).

**Try it without commitment.** Run `gish` in one tab — no `chsh`
required, nothing to undo but two directories. Coming from bash or zsh,
start with [docs/porting.md](docs/porting.md).

## The idea

Most of what people install plugins *for* — a fast git prompt, autosuggestions,
highlighting, completions, env and version switching — gish ships natively, so a
fresh install already behaves like a tuned setup.

What's left gets a real contract. **Plugins run out of process, in any
language, crash-isolated and deadline-bounded: a slow or broken plugin
degrades its own feature and can never block a keystroke or take down
your shell.** Kill one mid-session and the prompt doesn't flinch. (This
is how the fast parts of the shell world already work — gitstatusd,
carapace — promoted from workaround to architecture.)

The existing zsh ecosystem is an escape hatch, not the pitch: a
pattern-compat layer runs the common plugin shapes so one beloved script
can't block your migration, but the .zshrc pile is the thing most people
are trying to leave. See [docs/design.md](docs/design.md) for the
architecture, roadmap, and the decisions behind both.

*(The name expands to "gRPC interactive shell" — that's the plumbing the
plugin contract runs on. It's an implementation detail you should never
have to think about, which is why it isn't the pitch.)*

## Try it

```bash
make build
./build/gish                 # syntax highlighting, autosuggestions, Tab completion
./build/gish -c 'echo hi'    # run a command
./build/gish script.sh       # run a script
```

## Configuration

gish reads one rc file at interactive startup — `$GISH_RC`, else `$XDG_CONFIG_HOME/gish/gishrc`, else `~/.gishrc` — as ordinary shell script: functions, variables, and `cd` persist into your session.

gish starts **naked**: the prompt is the stock zsh/bash shape (`user@host dir %`), so day one looks like the shell you came from. Themes are opt-in — one line in your rc file:

```sh
# ~/.gishrc
GISH_THEME=p10k                # a native port of powerlevel10k: its presets, its
                               # ~50 segments, its config vocabulary — in Go, and
                               # 15x faster to first prompt (see docs/p10k.md)
GISH_THEME=gish                # or gish's own segment-knob theme (GISH_THEME_*)
GISH_THEME=starship            # or your exact starship prompt, unchanged
GISH_PROMPT='%~ %p{git} %?$ '  # or take full manual control (always wins)
GISH_PROMPT_CONT='... '        # zsh spellings work: %n user, %m host, %~ cwd,
                               # %# prompt char — plus %W basename, %d full cwd,
                               # %? exit status, %p{id} plugin segment, %% literal
```

Or skip the file editing: `config theme starship` sets it **live and** writes it to your rc file in one step. `config` lists everything tunable (`theme`, `lint`, `prompt`).

Coming from powerlevel10k? `p10k configure` is the wizard and `p10k import` brings your existing `~/.p10k.zsh` across — [docs/p10k.md](docs/p10k.md) covers the presets, what imports cleanly, and what deliberately does not.

Plugins are executables in `$XDG_DATA_HOME/gish/plugins` — `plugins` lists them. `gish-git` (in this repo) serves the `%p{git}` segment: branch, ahead/behind, staged/dirty/untracked, cached per-repo with fsnotify invalidation.

## Plugins

One file, four knobs, no modifier language to memorize:

```sh
plugin add zsh-users/zsh-autosuggestions
plugin add junegunn/fzf --kind release --pin 0.55.0
plugin add ohmyzsh/ohmyzsh --lazy command:git   # loads the first time you run git
plugin                                          # what's configured, and its state
plugin disable fzf                              # keep the entry, stop loading it
plugin update
```

Those commands write `$XDG_CONFIG_HOME/gish/plugins.toml`, and hand-editing
it is equally supported:

```toml
[[plugin]]
source = "junegunn/fzf"
kind   = "release"      # plugin (default) | release | snippet
pin    = "0.55.0"       # omit for latest
lazy   = "command:fzf"  # omit to load at startup
```

The [Zi](https://github.com/z-shell/zi) engine does the installing
underneath — existing `~/.zi-go` trees and installs carry over, and `zi
migrate` converts what you already have into the manifest. The `zi`
command with its ice modifiers still works for compatibility, but the
manifest is the supported surface; see [the
decision](docs/design.md#decisions) for why a modifier language was the
wrong thing to reproduce.

History lives at `$XDG_DATA_HOME/gish/history.jsonl` — up/down are prefix-aware, **Ctrl-R opens a full-screen fuzzy picker** showing where each command ran, how long ago, how long it took, and whether it failed (red), a leading space keeps a command out, secrets never reach disk, and **commands from concurrent sessions appear live**. Typos get `did you mean` suggestions; Homebrew's environment is set up natively (no `shellenv` boilerplate); and if you already use **starship**, `GISH_THEME=starship` renders your exact prompt unchanged.

gish also warns **before Enter**: it holds a real parse tree of the line, so the classic footguns — `rm $dir/*` unquoted, `cd /tmp; rm -rf *` unchained, `[ $x = y ]`, useless `cat`, `sort f > f` — draw a dim caution line under the prompt as you type. Multi-line buffers get a `shellcheck` pass on Enter when it's installed (budget-bounded, findings with codes). Warnings never block execution; `GISH_LINT=native` skips shellcheck, `GISH_LINT=off` silences everything.

And because the shell owns the scheduler, parallelism is a builtin, not a package: `parallel -j 4 -- gzip -9 {} ::: *.log` runs a goroutine pool of process children with output discipline GNU parallel never had by default — per-task prefixed streaming, or `--collect` for whole outputs in input order, never interleaved garbage. `--fail-fast` cancels the rest on first failure, Ctrl-C stops the lot, and the exit status is the worst task's.

## What's built in

The things you would otherwise install and wire up:

```sh
z api                  # zoxide-class jumping — frecency-ranked, seeded from your
                       # history, with a picker built in. No shell hook to install.
tool pin golang 1.26.6 # .tool-versions switching without shims: PATH is rebuilt on
                       # cd, your asdf/mise installs are reused as-is. gish switches
                       # versions; installing stays your package manager's job
trust                  # direnv-class per-directory env, with a real trust model:
                       # nothing applies until you allow it, and edits re-prompt
sandbox --profile readonly -- make test
                       # least-privilege exec: macOS Seatbelt, Linux Landlock
doctor                 # what's configured, what's broken, and the fix for each
ls | pick -m           # fzf's selection primitive, built in: Tab marks, Enter prints
```

And the AI surface, which is opt-in and never runs anything on its own:

```sh
?? find the biggest files here      # composes a command into your editor buffer,
                                    # sandbox-wrapped, for you to read and run
explain                             # why did that last command fail
```

Bring your own provider — the reference plugin drives the `claude` CLI you
already have, and any local model behind the same contract works identically.

### Hosting other people's agents

The more useful thing a shell can do for AI is not to be one. Coding
agents already live *inside* your shell, and gish is built to be the
place they run:

```sh
gish --sandbox workspace            # every command the agent runs, in or out of
                                    # its own tooling, is confined to this tree
```

Least-privilege exec on the shell's own exec path (macOS Seatbelt, Linux
Landlock), a permission-gated environment where nothing applies to a
directory until you allow it, and history that never records a secret.
Point Claude Code, aider, or your own script at a sandboxed gish session
and the confinement is the shell's, not the agent's promise.

## Making it your daily driver

**Start with your terminal emulator, not `chsh`.** Point your profile's
"command" setting at the gish binary and you get it in new tabs while
every existing habit — including "open a normal shell" — keeps working.
Nothing to undo but two directories under `$XDG_CONFIG_HOME/gish` and
`$XDG_DATA_HOME/gish`.

The non-interactive contract is POSIX-clean — tools that spawn `$SHELL -c` (editors, IDEs, cron, ssh, scp) get a fast, silent, script-compatible shell with no prompt machinery loaded. Login invocations (`-l`, or argv[0] beginning with `-`) source `/etc/profile`, then `~/.gish_profile` or `~/.profile`.

When you're sure, the login-shell route is the usual one:

```sh
which gish | sudo tee -a /etc/shells
chsh -s "$(which gish)"
```

## Blocks and terminal integration

gish emits OSC 133 semantic marks, so terminals that speak them —
kitty, WezTerm, Ghostty, iTerm2, VS Code — give you
scroll-to-previous-prompt, select-command-output, and click-to-rerun
with no configuration. `doctor` tells you what your terminal supports.
Full command+output blocks (capture, search, re-run) are designed in
[docs/blocks.md](docs/blocks.md) and staged behind that.

## Platforms

macOS and Linux are first-class today. Windows builds and its portable
test suite run in CI on every change, but the native interactive port
(ConPTY, job objects, installer) is deliberately sequenced after v1 —
WSL2 is the supported Windows story until then. The reasoning is in
[docs/design.md](docs/design.md#decisions).

## Development

```bash
make check   # fmt + vet + test
make lint    # golangci-lint
make proto   # regenerate pkg/pluginapi from proto/
```

See [AGENTS.md](AGENTS.md) for the full development guide.
