# gish

> **gish** — the **g**RPC **i**nteractive **sh**ell.

A new interactive shell: **zsh's interactive experience, bash's ubiquity, and a native plugin system with an actual contract.** Cross-platform (macOS, Linux, Windows), one static Go binary.

**Fast is a guarantee, not an accident**: ~6ms to first prompt warm (10ms with a 10,000-entry history), enforced by a CI regression gate. Plugin discovery never blocks the first paint.

**Status: early scaffold.** The interactive loop runs real POSIX/bash today (via [`mvdan.cc/sh`](https://github.com/mvdan/sh)); everything that makes it *gish* is in front of us.

## The idea

Shell plugins today are zsh scripts poking at shell internals with no contract, glued together by plugin managers of wildly varying ergonomics. Gish's design bet is a **two-tier plugin system**:

- **Tier 1 — your existing zsh plugins keep working.** A zsh-compat layer runs the zi/zinit/oh-my-zsh ecosystem in-process, so switching shells doesn't mean abandoning your setup.
- **Tier 2 — native plugins get a real API.** Resident subprocesses over gRPC ([hashicorp/go-plugin](https://github.com/hashicorp/go-plugin)) with a versioned protobuf contract: completion providers, prompt segments, history backends. Written in any language. Deadline on every call — a slow plugin can never block a keystroke. (This is how the fast parts of the shell world already work — gitstatusd, carapace — promoted from workaround to architecture.)

See [docs/design.md](docs/design.md) for the full architecture and roadmap.

## Try the skeleton

```bash
make build
./build/gish                 # interactive: raw-mode editor, emacs keys, Tab completion
./build/gish -c 'echo hi'    # run a command
./build/gish script.sh       # run a script
```

## Configuration

gish reads one rc file at interactive startup — `$GISH_RC`, else `$XDG_CONFIG_HOME/gish/gishrc`, else `~/.gishrc` — as ordinary shell script: functions, variables, and `cd` persist into your session.

gish ships a p10k-class two-line prompt **by default** — smart-truncated path, git (via the gish-git plugin), runtime pins, job count, duration, exit status — all async and budget-bounded, so the prompt never waits on anything.

```sh
# ~/.gishrc
GISH_THEME=plain               # the off switch, for the bare-metal crowd
GISH_PROMPT='%W %p{git} %?$ '  # or take full manual control (always wins):
GISH_PROMPT_CONT='... '        # %u %h %w %W %? %% and %p{id} plugin segments
```

Plugins are executables in `$XDG_DATA_HOME/gish/plugins` — `plugins` lists them. `gish-git` (in this repo) serves the `%p{git}` segment: branch, ahead/behind, staged/dirty/untracked, cached per-repo with fsnotify invalidation.

## The zi plugin manager

gish ships [Zi](https://github.com/z-shell/zi) natively — the Go engine from [zi-go](https://github.com/blairham/zi-go) built in, no shim, existing syntax and `~/.zi-go` installs carry over:

```sh
# ~/.gishrc
zi ice from"gh-r" as"program"
zi load junegunn/fzf              # release binary onto PATH

zi snippet OMZP::git              # oh-my-zsh snippets
zi update                         # refresh everything
zi list
```

A load is installed by the engine and `source`d directly in your live session — functions, variables, and PATH changes persist. (Plugins written in heavy zsh dialect await the tier-1 compat layer; snippets, POSIX-style plugins, and `gh-r` binaries work today. `wait` turbo ices are accepted but load immediately for now.) The manager itself sits behind a contract, so an alternative manager can replace it.

History lives at `$XDG_DATA_HOME/gish/history.jsonl` — up/down are prefix-aware, Ctrl-R is incremental search, and a leading space keeps a command out of history.

## Development

```bash
make check   # fmt + vet + test
make lint    # golangci-lint
make proto   # regenerate pkg/pluginapi from proto/
```

See [AGENTS.md](AGENTS.md) for the full development guide.
