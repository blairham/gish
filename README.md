# koi

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
koi                        5.6 ms      theme + highlighting + suggestions + lint, all on
bash (no rc)               5.5 ms      empty rc
zsh (no rc)                8.4 ms      empty rc
zsh + powerlevel10k       86.3 ms      the prompt koi's p10k theme is a port of
zsh (real config)        302.7 ms      a real .zshrc: plugin manager, theme, tool hooks
```

Keystroke latency, measured end to end from byte-in to repaint-out:
**p50 0.2 ms, p99 0.3 ms** — with highlighting, suggestions, and the
footgun linter all running. Full methodology and numbers in
[docs/bench.md](docs/bench.md); the bash-compatibility scoreboard, gaps
included, is in [docs/compat.md](docs/compat.md). That corpus is one we
curated, so there is a second scoreboard with a denominator nobody can
accuse us of choosing — **bash's own test suite**, run through koi and
published with its parse gaps in
[docs/bash-suite.md](docs/bash-suite.md). It is a much lower number, and
it is meant to be: quote whichever one the claim is actually about.

What happens when you *paste* a bash one-liner at the prompt, and when you source nvm, conda
or an activate script unmodified, is measured separately in
[docs/interactive-compat.md](docs/interactive-compat.md) — pasting and
sourcing is the most-cited reason people go back. The same page carries
the ecosystem matrix: **starship, direnv, fzf, zoxide, atuin and mise
run in koi through their own bash init lines**, unmodified and with no
koi-specific support on either side.

**A plugin can never block a keystroke, and never needs a rebuild.**
Every host→plugin call carries a deadline, so a hung plugin costs its
own segment and nothing else; `plugin/v1` is frozen-additive and CI
fails on any change that would break a binary compiled against it. Both
are tested rather than asserted — see
[docs/plugins.md](docs/plugins.md#the-compatibility-promise-to-plugin-authors-168).

**Your config will not break.** What is frozen — rc syntax, `KOI_*`
variables, `config` keys, `plugins.toml`, the prompt escape set, the
theme knobs, and bash's own hook surface — is written down in
[docs/stability.md](docs/stability.md), along with what is explicitly
not covered and how a deprecation works. A shell is something people
build on for years; the contract is what makes that reasonable.

**No counterparty.** koi is MIT, open source, and there is no account,
no telemetry, no CLA assigning copyright to a company, and no hosted
service anywhere in the path — nothing here can be switched off by
someone else's business decision. The shells and terminals that asked
people to trust a company have a consistent history: Fig was acquired
and shut down about six months later, and Warp required a login until
late 2024 and open-sourced in 2026 to an audience that had already
left. That is not a risk you can evaluate after adopting something,
which is why it is stated here rather than conceded later.

Even the update notice keeps that promise: koi tells you when a newer
release exists, and it learns that by **reading what your package
manager already downloaded** — never by asking anyone. There is no
request to opt into, no cadence to configure, and nothing leaves the
machine. The flip side is stated where it matters: koi can tell you
something newer exists, but it will never claim you are up to date,
because local metadata is only as fresh as your last `brew update`.

**Try it without commitment.** Run `koi` in one tab — no `chsh`
required, nothing to undo but two directories. On its first run it
tells you how to uninstall it, once, before you have invested anything. Coming from bash or zsh,
run `koi migrate` to import your aliases, functions, exports, PATH,
prompt and history in one command — it parses your rc rather than
running it, and lists everything that did not translate
([docs/coming-from-zsh.md](docs/coming-from-zsh.md)). Muscle memory is
in [docs/porting.md](docs/porting.md).

## The idea

Most of what people install plugins *for* — a fast git prompt, autosuggestions,
highlighting, completions, env and version switching — koi ships natively, so a
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

*(A koi is a fish — the lineage is the point, since fish is the shell
whose out-of-box experience this one is chasing. The project was called
`gish` until the name was retired along with its backronym: a plugin
architecture is not why anyone switches shells. See
[docs/strategy.md](docs/strategy.md).)*

## Try it

```bash
make build
./build/koi                 # syntax highlighting, autosuggestions, Tab completion
./build/koi -c 'echo hi'    # run a command
./build/koi script.sh       # run a script
```

## Configuration

koi reads one rc file at interactive startup — `$KOI_RC`, else `$XDG_CONFIG_HOME/koi/koirc`, else `~/.koirc` — as ordinary shell script: functions, variables, and `cd` persist into your session.

koi starts **naked**: the prompt is the stock zsh/bash shape (`user@host dir %`), so day one looks like the shell you came from. Themes are opt-in — one line in your rc file:

```sh
# ~/.koirc
KOI_THEME=p10k                # a native port of powerlevel10k: its presets, its
                               # ~50 segments, its config vocabulary — in Go, and
                               # 15x faster to first prompt (see docs/prompt.md)
KOI_THEME=koi                # or koi's own segment-knob theme (KOI_THEME_*)
KOI_THEME=starship            # or your exact starship prompt, unchanged
KOI_PROMPT='%~ %p{git} %?$ '  # or take full manual control (always wins)
KOI_PROMPT_CONT='... '        # zsh spellings work: %n user, %m host, %~ cwd,
                               # %# prompt char — plus %W basename, %d full cwd,
                               # %? exit status, %p{id} plugin segment, %% literal
```

Or skip the file editing: `config theme starship` sets it **live and** writes it to your rc file in one step. `config` lists everything tunable (`theme`, `lint`, `prompt`).

Coming from powerlevel10k? `prompt configure` is the wizard and `prompt import` brings your existing `~/.p10k.zsh` across — [docs/prompt.md](docs/prompt.md) covers the presets, what imports cleanly, and what deliberately does not.

Plugins are executables in `$XDG_DATA_HOME/koi/plugins` — `plugins` lists them. `koi-git` (in this repo) serves the `%p{git}` segment: branch, ahead/behind, staged/dirty/untracked, cached per-repo with fsnotify invalidation.

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

Those commands write `$XDG_CONFIG_HOME/koi/plugins.toml`, and hand-editing
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

History lives at `$XDG_DATA_HOME/koi/history.jsonl` — up/down are prefix-aware, **Ctrl-R opens a full-screen fuzzy picker** showing where each command ran, how long ago, how long it took, and whether it failed (red), a leading space keeps a command out, secrets never reach disk, and **commands from concurrent sessions appear live**. Typos get `did you mean` suggestions — for the whole line, not just the word, and a distro's own `command_not_found_handle` runs first and receives the full command line. Homebrew's environment is set up natively (no `shellenv` boilerplate); and if you already use **starship**, `KOI_THEME=starship` renders your exact prompt unchanged.

`set -o vi` works — a real modal editor with counts, text objects and
operator composition (`d2w`, `ciw`, `ci"`, `f`/`;`), not a handful of
hardcoded commands — and the cursor shape tells you which mode you are
in. See [docs/porting.md](docs/porting.md#vi-mode) for the full set and
the two deliberate limits.

Every affordance has its own switch, because "turn the whole shell monochrome" is not an answer to one distracting color: `config highlight quiet` keeps syntax color but drops the red-on-unknown-command verdict, `config highlight off` drops highlighting entirely, and `config suggest off` turns off the history ghost text. The built-in colors are the terminal's own 16 — koi does not impose a palette over the scheme you chose. Nor does it clutter your home directory: starting a shell creates nothing, and each file appears when there is something to put in it.

koi also warns **before Enter**: it holds a real parse tree of the line, so the classic footguns — `rm $dir/*` unquoted, `cd /tmp; rm -rf *` unchained, `[ $x = y ]`, useless `cat`, `sort f > f` — draw a dim caution line under the prompt as you type. Multi-line buffers get a `shellcheck` pass on Enter when it's installed (budget-bounded, findings with codes). Warnings never block execution; `KOI_LINT=native` skips shellcheck, `KOI_LINT=off` silences everything.

And because the shell owns the scheduler, parallelism is a builtin, not a package: `parallel -j 4 -- gzip -9 {} ::: *.log` runs a goroutine pool of process children with output discipline GNU parallel never had by default — per-task prefixed streaming, or `--collect` for whole outputs in input order, never interleaved garbage. `--fail-fast` cancels the rest on first failure, Ctrl-C stops the lot, and the exit status is the worst task's.

## What's built in

The things you would otherwise install and wire up:

```sh
z api                  # zoxide-class jumping — frecency-ranked, seeded from your
                       # history, with a picker built in. No shell hook to install.
tool pin golang 1.26.6 # .tool-versions switching without shims: PATH is rebuilt on
                       # cd, your asdf/mise installs are reused as-is. koi switches
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
agents already live *inside* your shell, and koi is built to be the
place they run:

```sh
koi --sandbox workspace            # every command the agent runs, in or out of
                                    # its own tooling, is confined to this tree

ln -s "$(which koi)" ~/.local/bin/koi-agent-bash   # same thing, as a name
```

Least-privilege exec on the shell's own exec path (macOS Seatbelt, Linux
Landlock), a permission-gated environment where nothing applies to a
directory until you allow it, and history that never records a secret.
Point Claude Code, aider, or your own script at a sandboxed koi session
and the confinement is the shell's, not the agent's promise.

**The symlink is the whole install.** A harness is handed a *path* to a
shell and has nowhere to put a flag, so the invocation name carries the
posture instead — the way argv[0] already carries login. Anything named
`koi-agent` (or `koi-agent-<suffix>`) starts with `--sandbox workspace`
already on; an explicit `--sandbox`, including `--sandbox none`, still
wins. The `-bash` suffix is not decoration: harnesses pick a shell by
grepping their own `$SHELL` for `bash` or `zsh`, and a binary called
`koi` is invisible to them.

## Making it your daily driver

**Start with your terminal emulator, not `chsh`.** Point your profile's
"command" setting at the koi binary and you get it in new tabs while
every existing habit — including "open a normal shell" — keeps working.
Nothing to undo but two directories under `$XDG_CONFIG_HOME/koi` and
`$XDG_DATA_HOME/koi`.

The non-interactive contract is POSIX-clean — tools that spawn `$SHELL -c` (editors, IDEs, cron, ssh, scp) get a fast, silent, script-compatible shell with no prompt machinery loaded. That includes the spawn *forms* they use, not just the flags: short options cluster (`$SHELL -lc 'cmd'`), options may follow `-c` because the command string is an operand (`$SHELL -c -l 'cmd'`), `--` ends the options, and `-i` sources your rc file the way bash does. Login invocations (`-l`, or argv[0] beginning with `-`) source `/etc/profile`, then `~/.koi_profile` or `~/.profile`.

When you're sure, the login-shell route is the usual one. Both lines
matter: a login shell that is **not** listed in `/etc/shells` is the
documented way people lock themselves out, because some systems refuse
to log in with an unlisted shell.

```sh
which koi | sudo tee -a /etc/shells   # do this first, not second
chsh -s "$(which koi)"
```

**The way back, before you need it:**

```sh
chsh -s /bin/zsh                       # or whatever you ran before
```

`doctor` knows this state and reports it: whether koi is your login
shell, whether it is listed in `/etc/shells`, and the exact `chsh`
command that undoes it. If you never run `chsh` at all, `doctor` says
so and says there is nothing to revert — which is the supported path.

## Your shell follows you over ssh

```sh
koi ssh prod-web-3
```

The most-cited reason people give for not switching shells is the box
they only have ssh to — the 2AM incident host they did not provision and
cannot change. `koi ssh` probes it, copies one static binary plus your
prompt settings into a cache directory under your own home there, and
opens an interactive koi. Repeat visits copy nothing.

It never installs anything: no `chsh`, no remote dotfile edits, no
daemon, and it does not shadow your `ssh`. It asks once per host before
touching it, `koi ssh --uninstall host` undoes it, and **every** failure
falls back to plain `ssh` with one line on stderr — during an incident, a
feature that delays your shell is worse than no feature.

The details that make it work on hardened hosts — the `noexec` exec test,
`cat` instead of `sftp`, content-addressed verification, why terminfo is
not pushed — are in [docs/ssh.md](docs/ssh.md).

## Blocks and terminal integration

koi emits OSC 133 semantic marks, so terminals that speak them —
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
make proto   # regenerate pkg/pluginapi/v1 from proto/
```

See [AGENTS.md](AGENTS.md) for the full development guide.
