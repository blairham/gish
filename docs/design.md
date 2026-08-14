# swash — architecture

## Goals

1. **Interactive experience of zsh** — completion menus, widgets, rich prompts — with cleaner, discoverable configuration.
2. **Scripting substrate of bash/POSIX** — scripts and muscle memory carry over; `mvdan.cc/sh` gives us a battle-tested parser/interpreter to grow a zsh dialect on top of.
3. **Plugins with a contract** — the headline feature. Two tiers, below.
4. **Cross-platform** — macOS and Linux first-class from day one; Windows a real target (Windows Terminal-native, not just WSL), revisited milestone by milestone.

## Two-tier plugin system

### Tier 1 — script plugins (adoption)

The existing zsh ecosystem: zi/zinit/oh-my-zsh plugins, themes, completions. These are zsh *scripts* that expect `zle`, `zstyle`, `autoload`, `precmd`/`preexec`, compsys. They must run in-process — per-keystroke widgets can't pay IPC costs, and the scripts assume shell state.

Strategy, in order of effort/payoff:

1. **Pattern compat first**: most popular plugins reduce to aliases, exports, PATH edits, precmd/preexec hooks, completions, and a handful of zle widgets. Implement that surface and the top-N plugins run.
2. **zsh-dialect growth**: extend the parser/interpreter toward the zsh constructs plugins actually use, driven by a corpus of real plugins, not by the zsh manual.
3. **Escape hatch**: a `zsh -c` delegation shim for the long tail (where zsh is installed). Never the fast path.

Built-in plugin *management* (the zi rethink) is native: declarative manifest, lazy loading by default, one obvious way to install/pin/update. No `ice` modifiers to memorize.

### Tier 2 — native gRPC plugins (differentiator)

Resident subprocesses over hashicorp/go-plugin, contract in `proto/swash/plugin/v1`:

| Service | Purpose | Shape |
| --- | --- | --- |
| `PluginInfo` | mandatory discovery: name, version, capabilities | unary, once per process |
| `CompletionProvider` | tab completion | request → **streamed** candidate batches |
| `PromptSegmentProvider` | git status, k8s context, cloud account, … | declare segments + budgets, then unary renders |
| `HistoryBackend` | sync/search/scrub history | fire-and-forget append, streamed search |

Rules that make this fast enough to live in a keystroke path:

- **Resident, never spawn-per-call.** Lazy launch on first use, alive for the session. (Future: a shared cross-session daemon, gitstatusd-style.)
- **Deadline on every call.** Prompt segments default to a 50ms budget (self-declarable via `budget_ms`); misses render stale/empty and repaint in place. Completions stream so early candidates show while slow sources work.
- **Versioned contract.** `swash.plugin.v1` is frozen-additive; breaking changes are a `v2` package plus a go-plugin `ProtocolVersion` bump.
- **Filtered environment.** Plugins get an allowlisted env map, not the process environment.
- **Any language.** gRPC + protobuf means plugins in Go, Rust, Python, whatever — an adoption lever the zsh ecosystem can't offer.

Precedent that this architecture wins: powerlevel10k is fast *because* gitstatusd is an external resident daemon; carapace serves completions to five shells from one external engine; LSP proved per-keystroke RPC to a sidecar at scale.

### How the tiers meet

The line editor and prompt engine consume both tiers through one internal interface; tier 1 hooks and tier 2 RPCs are two providers behind it. Long-term, tier 1 is the on-ramp and tier 2 is where the ecosystem settles — the tier with the contract.

## Roadmap (milestone sketch)

1. **Skeleton** *(now)* — bash/POSIX exec via mvdan/sh, plain REPL, tier-2 contract designed, stubs generated, host handshake in place.
2. **Line editor** — raw-mode editor (keymap, kill-ring, undo), history file, incremental search. This is where swash starts feeling like a shell.
3. **Tier-2 dispatch** — plugin discovery/config, resident lifecycle, deadlines; first real plugins: git prompt segment, file/carapace-style completion.
4. **Completion UI** — menu selection, descriptions, multi-provider merge.
5. **Tier-1 compat, wave 1** — precmd/preexec, aliases/exports, simple widget shims; run the top oh-my-zsh plugins unmodified.
6. **zsh dialect** — corpus-driven parser/interpreter extensions.
7. **Windows hardening** — ConPTY line editor path, job-object process groups.

## Open questions

- Prompt styling vocabulary for tier 2 (`RenderResponse.text`): markup subset vs. structured spans. Raw text until decided.
- Line editor: build on an existing Go TTY layer (bubbletea's input stack?) or hand-roll raw-mode like elvish. Decide at milestone 2.
- Tier-1 widget shim depth: which zle APIs are worth emulating vs. declaring out of scope.
