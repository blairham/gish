# gish — architecture

## Goals

1. **Interactive experience of zsh** — completion menus, widgets, rich prompts — with cleaner, discoverable configuration.
2. **Scripting substrate of bash/POSIX** — scripts and muscle memory carry over; `mvdan.cc/sh` gives us a battle-tested parser/interpreter to grow a zsh dialect on top of.
3. **Plugins with a contract** — the headline feature. Two tiers, below.
4. **Cross-platform** — macOS and Linux first-class from day one; Windows a real target, *sequenced* (#110): the portability invariants and the windows CI gate hold now, native interactive (Windows Terminal, not just WSL) lands in v1.x after the unix beachhead. WSL2 is the supported Windows story until then.

## Two-tier plugin system

### Tier 1 — script plugins (adoption)

The existing zsh ecosystem: zi/zinit/oh-my-zsh plugins, themes, completions. These are zsh *scripts* that expect `zle`, `zstyle`, `autoload`, `precmd`/`preexec`, compsys. They must run in-process — per-keystroke widgets can't pay IPC costs, and the scripts assume shell state.

Strategy, in order of effort/payoff:

1. **Pattern compat first**: most popular plugins reduce to aliases, exports, PATH edits, precmd/preexec hooks, completions, and a handful of zle widgets. Implement that surface and the top-N plugins run.
2. **zsh-dialect growth**: extend the parser/interpreter toward the zsh constructs plugins actually use, driven by a corpus of real plugins, not by the zsh manual.
3. **Escape hatch**: a `zsh -c` delegation shim for the long tail (where zsh is installed). Never the fast path.

Built-in plugin *management* (the zi rethink) is native: declarative manifest, lazy loading by default, one obvious way to install/pin/update. No `ice` modifiers to memorize.

### Tier 2 — native gRPC plugins (differentiator)

Resident subprocesses over hashicorp/go-plugin, contract in `proto/gish/plugin/v1`:

| Service | Purpose | Shape |
| --- | --- | --- |
| `PluginInfo` | mandatory discovery: name, version, capabilities | unary, once per process |
| `CompletionProvider` | tab completion | request → **streamed** candidate batches |
| `PromptSegmentProvider` | git status, k8s context, cloud account, … | declare segments + budgets, then unary renders |
| `HistoryBackend` | sync/search/scrub history | fire-and-forget append, streamed search |

Rules that make this fast enough to live in a keystroke path:

- **Resident, never spawn-per-call.** Lazy launch on first use, alive for the session. (Future: a shared cross-session daemon, gitstatusd-style.)
- **Deadline on every call.** Prompt segments default to a 50ms budget (self-declarable via `budget_ms`); misses render stale/empty and repaint in place. Completions stream so early candidates show while slow sources work.
- **Versioned contract.** `gish.plugin.v1` is frozen-additive; breaking changes are a `v2` package plus a go-plugin `ProtocolVersion` bump.
- **Filtered environment.** Plugins get an allowlisted env map, not the process environment.
- **Any language.** gRPC + protobuf means plugins in Go, Rust, Python, whatever — an adoption lever the zsh ecosystem can't offer.

Precedent that this architecture wins: powerlevel10k is fast *because* gitstatusd is an external resident daemon; carapace serves completions to five shells from one external engine; LSP proved per-keystroke RPC to a sidecar at scale.

### How the tiers meet

The line editor and prompt engine consume both tiers through one internal interface; tier 1 hooks and tier 2 RPCs are two providers behind it. Long-term, tier 1 is the on-ramp and tier 2 is where the ecosystem settles — the tier with the contract.

## Roadmap (milestone sketch)

1. **Skeleton** *(now)* — bash/POSIX exec via mvdan/sh, plain REPL, tier-2 contract designed, stubs generated, host handshake in place.
2. **Line editor** — raw-mode editor (keymap, kill-ring, undo), history file, incremental search. This is where gish starts feeling like a shell.
3. **Tier-2 dispatch** — plugin discovery/config, resident lifecycle, deadlines; first real plugins: git prompt segment, file/carapace-style completion.
4. **Completion UI** — menu selection, descriptions, multi-provider merge.
5. **Tier-1 compat, wave 1** — precmd/preexec, aliases/exports, simple widget shims; run the top oh-my-zsh plugins unmodified.
6. **zsh dialect** — corpus-driven parser/interpreter extensions.
7. **Windows hardening** *(paused per #110 — resumes post-v1)* — ConPTY line editor path, job-object process groups.

## Decisions

- **TTY input approach** (2026-08, [#1](https://github.com/blairham/gish/issues/1)):
  **own the editor core; borrow the terminal plumbing.** gish writes the
  buffer/cursor model, keymap engine, kill ring, undo, multi-line
  continuation, and the diff-based inline renderer — the render loop is
  where the differentiators (plugin ghost text, streamed completion menus,
  deadline repaints) live, and every serious shell ends up owning its
  editor (fish, elvish `pkg/edit`, nushell/reedline). The plumbing is
  imported behind `internal/term`, gish's own interface: raw-mode
  entry/restore via `golang.org/x/term` now; key/event decoding (ANSI,
  kitty protocol, bracketed paste, Windows ConPTY) via charmbracelet's
  `ultraviolet`/`x/ansi` layer when the editor lands, with a hand-rolled
  decoder as the documented fallback. Nothing outside `internal/term`
  imports a terminal library. Ruled out: bubbletea as framework (Elm
  control inversion; terminal ownership fights shell handover and job
  control), go-prompt and the readline family (framework-shaped, stale,
  or single-maintainer risk at the heart of the shell, with render
  pipelines that can't host plugin hooks).

- **CommandProvider** (2026-08, [#11](https://github.com/blairham/gish/issues/11)):
  plugins register commands over gRPC. Precedence: interpreter/gish
  builtin names are reserved (claims rejected with a warning); shell
  functions shadow plugin commands (dispatched before the exec seam);
  plugin commands shadow PATH; contested names go to the
  lexicographically first plugin. I/O is streamed over the RPC — no raw
  terminal handover in v1 (full-screen plugin commands wait for a
  PTY-passing design). Execution starts at Enter, so Run carries no
  budget; Ctrl-C rides context cancellation. Discovery uses an
  mtime-keyed command-index cache so warm sessions route names without
  launching plugins.

## Open questions

- Prompt styling vocabulary for tier 2 (`RenderResponse.text`): markup subset vs. structured spans. Raw text until decided.
- Tier-1 widget shim depth: which zle APIs are worth emulating vs. declaring out of scope.
- ~~**`EnvProvider`** (direnv-class)~~ — resolved (#12): both mechanisms, layered. Trust is per-(plugin, directory, diff-hash) — nothing applies without an explicit `trust allow`, and a changed diff re-pends (direnv's edit-reprompts semantics). A deny-list (loader hooks, `IFS`, startup-file vars, `GISH_*`) is stripped host-side before a proposal exists; `PATH` is settable but only ever through the visible allow flow. Requests carry the allowlisted env subset. Applied diffs revert when the shell leaves the proposal's subtree.

The plugin roadmap itself — which plugins, in what order, under which latency budgets — lives in [plugins.md](plugins.md).
