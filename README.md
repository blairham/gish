# gish

> **gish** — the **g**RPC **i**nteractive **sh**ell.

A new interactive shell: **zsh's interactive experience, bash's ubiquity, and a native plugin system with an actual contract.** Cross-platform (macOS, Linux, Windows), one static Go binary.

**Status: early scaffold.** The interactive loop runs real POSIX/bash today (via [`mvdan.cc/sh`](https://github.com/mvdan/sh)); everything that makes it *gish* is in front of us.

## The idea

Shell plugins today are zsh scripts poking at shell internals with no contract, glued together by plugin managers of wildly varying ergonomics. Gish's design bet is a **two-tier plugin system**:

- **Tier 1 — your existing zsh plugins keep working.** A zsh-compat layer runs the zi/zinit/oh-my-zsh ecosystem in-process, so switching shells doesn't mean abandoning your setup.
- **Tier 2 — native plugins get a real API.** Resident subprocesses over gRPC ([hashicorp/go-plugin](https://github.com/hashicorp/go-plugin)) with a versioned protobuf contract: completion providers, prompt segments, history backends. Written in any language. Deadline on every call — a slow plugin can never block a keystroke. (This is how the fast parts of the shell world already work — gitstatusd, carapace — promoted from workaround to architecture.)

See [docs/design.md](docs/design.md) for the full architecture and roadmap.

## Try the skeleton

```bash
make build
./build/gish                 # interactive: raw-mode editor, emacs keys, multi-line
./build/gish -c 'echo hi'    # run a command
./build/gish script.sh       # run a script
```

## Configuration

gish reads one rc file at interactive startup — `$GISH_RC`, else `$XDG_CONFIG_HOME/gish/gishrc`, else `~/.gishrc` — as ordinary shell script: functions, variables, and `cd` persist into your session.

```sh
# ~/.gishrc
GISH_PROMPT='%u@%h %W %?$ '   # %u user  %h host  %w cwd(~)  %W basename  %? exit  %% literal
GISH_PROMPT_CONT='... '
```

History lives at `$XDG_DATA_HOME/gish/history.jsonl` — up/down are prefix-aware, Ctrl-R is incremental search, and a leading space keeps a command out of history.

## Development

```bash
make check   # fmt + vet + test
make lint    # golangci-lint
make proto   # regenerate pkg/pluginapi from proto/
```

See [AGENTS.md](AGENTS.md) for the full development guide.
