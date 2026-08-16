# gish — architecture

## Goals

1. **Interactive experience of zsh** — completion menus, widgets, rich prompts — with cleaner, discoverable configuration.
2. **Scripting substrate of bash/POSIX** — scripts and muscle memory carry over; `mvdan.cc/sh` gives us a battle-tested parser/interpreter to grow a zsh dialect on top of.
3. **Plugins with a contract** — the headline feature. Two tiers, below.
4. **Cross-platform** — macOS and Linux first-class from day one; Windows a real target, *sequenced* (#110): the portability invariants and the windows CI gate hold now, native interactive (Windows Terminal, not just WSL) lands in v1.x after the unix beachhead. WSL2 is the supported Windows story until then.

## Two-tier plugin system

### Tier 1 — script plugins (migration escape hatch)

*Demoted from "the adoption story" by [#105](https://github.com/blairham/gish/issues/105) — see Decisions.*
The adoption story is fish-grade defaults + bash paste-compat + the
native stack; tier 1 exists so a switcher's handful of
must-have zsh plugins is not a blocker, not so the .zshrc pile moves in.

Scope — **pattern compat only**: aliases, exports, PATH edits,
`precmd`/`preexec` hooks, and completion registration — the surface the
zi manager already feeds. No zle-widget shim beyond what maps
trivially; compsys emulation is explicitly out of scope. Corpus-driven
zsh-dialect growth is off the critical path, revisited only on
demonstrated demand for named plugins pattern-compat cannot carry.

There is no delegation fallback: the long tail keeps running in zsh,
where it already works — see the `zsh -c` decision below.

Built-in plugin *management* (the zi rethink) is native and **shipped**:
a declarative manifest (`plugins.toml`) with four knobs — source, kind,
pin, lazy — edited by `plugin add|remove|pin|enable|disable|update`. No
`ice` modifiers to memorize; see the decision below.

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

The line editor and prompt engine consume both tiers through one internal interface; tier 1 hooks and tier 2 RPCs are two providers behind it. Tier 2 is where the ecosystem settles — the tier with the contract; tier 1 is the escape hatch that keeps a migration from stalling on one beloved script.

## Roadmap (milestone sketch)

1. **Skeleton** *(now)* — bash/POSIX exec via mvdan/sh, plain REPL, tier-2 contract designed, stubs generated, host handshake in place.
2. **Line editor** — raw-mode editor (keymap, kill-ring, undo), history file, incremental search. This is where gish starts feeling like a shell.
3. **Tier-2 dispatch** — plugin discovery/config, resident lifecycle, deadlines; first real plugins: git prompt segment, file/carapace-style completion.
4. **Completion UI** — menu selection, descriptions, multi-provider merge.
5. **Tier-1 pattern compat** — precmd/preexec, aliases/exports, completion registration; the top plugins' *behaviors*, natively or by pattern.
6. ~~**zsh dialect** — corpus-driven parser/interpreter extensions.~~ *Off the critical path per #105; revisit on demonstrated demand.*
7. **Windows hardening** *(paused per #110 — resumes post-v1)* — ConPTY line editor path, job-object process groups.

## Decisions

- **A manifest, not a modifier language** (2026-08,
  [#108](https://github.com/blairham/gish/issues/108)): plugin
  configuration is data — `source`, `kind`, `pin`, `lazy` in
  `$XDG_CONFIG_HOME/gish/plugins.toml` — not a vocabulary of ice
  modifiers (`from"gh-r" as"program" pick"bin/fzf" wait"1" lucid`) to
  learn before installing one plugin. The #23 port carried zi's full
  surface in because it was the engine's native idiom; reproducing that
  idiom faithfully pointed the wrong way for a shell whose pitch is
  escaping the framework tax, and pre-1.0 was the free window to shrink
  it (our frozen-additive discipline would otherwise keep it forever).

  The zi engine stays as the implementation and existing installs carry
  over; `zi migrate` converts them, and the `zi` command keeps working
  with a one-time notice. Ice syntax is documented only on the migration
  path.

- **The native/delegate line** (2026-08,
  [#112](https://github.com/blairham/gish/issues/112)): gish ships
  natively what lives in the **keystroke, prompt, and cd path** —
  highlighting, suggestions, completion, prompt segments, env diffs,
  version *switching*, directory jumping — because those are the places
  a shell can be uniquely fast and where an external tool costs a hook,
  a subprocess, or a stall. Everything else is delegated to the tool
  whose full-time job it is.

  The first feature on the wrong side of that line was `tool install
  --from`, a GitHub-release downloader: it carried package-manager
  obligations (archive formats, binary provenance, rate limits,
  platform matrices, "it didn't work on my machine" threads) that mise,
  ubi, and asdf already own. It now prints the equivalent one-liner for
  those tools instead. The `ghr` code stays — plugin installation is
  gish's own artifact class, which it legitimately owns.

  The guardrail matters more than the flag: "ship the eight-tool stack
  natively" is a strategy that needs a stopping rule, and this is it.

- **Host agents; do not be one** (2026-08,
  [#111](https://github.com/blairham/gish/issues/111)): the `??` compose
  and `explain` surfaces match the researched demand for AI in a shell
  point for point; *plan orchestration* was nowhere in that signal, and
  competing with funded agentic CLIs that already run inside the shell
  is a losing trade. The stronger position is that gish is the best
  **host** for other people's agents — sandbox profiles on the exec path
  (#21), permission-gated env (#12), secret-free history (#10) — which
  is a claim no agent CLI can make about itself.

  The #34 `agent` builtin is **frozen, not deleted**: experimental,
  unadvertised, and receiving no further investment. It is not cut,
  because the proposed alternative does not survive contact with the
  architecture — moving orchestration behind the plugin boundary would
  require giving a plugin an exec channel, which the tier-2 contract
  forbids precisely so a plugin can never run commands on the user's
  behalf. A plugin-side agent could only *propose*, which is `??` with
  extra ceremony. The existing implementation already holds execution
  where it belongs (the shell), so freezing costs nothing and deleting
  would trade a working safety property for a smaller diff.

  It stays out of the announcement regardless: "shell with a built-in AI
  agent" is the Warp-shaped red flag the launch playbook exists to avoid.

- **Tier-1 zsh compat is an escape hatch, not the adoption story**
  (2026-08, [#105](https://github.com/blairham/gish/issues/105)):
  the Aug 2026 research is unambiguous — config fatigue is the #1
  switching trigger (people flee the .zshrc pile, they don't pack it);
  fish built the largest post-zsh user base with zero compat while
  Oils' 8-year maximal-bash-compat effort bought ~272 brew installs a
  year; and gish already ships the top zsh plugins' behaviors natively
  (autosuggestions, highlighting, p10k-class prompt, completions,
  direnv/asdf). Acquisition-critical compat is bash *paste* compat
  (mvdan/sh, proven by the #101 scoreboard). Tier 1 is therefore
  scoped to pattern compat, the zsh dialect milestone leaves the
  critical path, and the freed effort goes to the differentiators that
  win threads (#99 blocks, #98 ssh, #101/#102 published numbers). The
  escape hatch stays — elvish/nushell show an ecosystem of zero is
  worse than an ecosystem of sourced scripts — it just stops being
  the bet.

- **No `zsh -c` delegation shim** (2026-08, [#107](https://github.com/blairham/gish/issues/107)):
  **the compat boundary is honest, not smoothed over with a chimera.**
  The once-planned escape hatch — delegating unparsed zsh constructs to
  an installed zsh — is struck before any code existed. It contradicted
  every story gish tells: an out-of-process zsh dependency inside a
  shell that markets "no sourced-script soup"; a latency wildcard on
  deadline-bounded surfaces; a hidden runtime dependency reintroducing
  "works differently on different boxes", which the single static
  binary exists to kill; and a support surface where every delegated
  plugin bug becomes an unfixable gish bug report. The answer for a
  plugin that pattern-compat cannot run is the truthful one: it keeps
  running in zsh, where the user already had it working. Do not
  re-propose without new facts.

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
