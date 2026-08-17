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

- **ACP on the agent edge, in two places** (2026-08,
  [#166](https://github.com/blairham/gish/issues/166),
  [#167](https://github.com/blairham/gish/issues/167), docs/acp.md): the
  inbound role (an ACP agent answers `??` and `explain`) is a plugin;
  the outbound role (an agent's commands execute inside gish) is core,
  because a plugin may never hold an exec channel (#34).

  The spike's load-bearing question — does the terminal capability have
  real adopters — resolved yes, and better than expected: the capability
  is **client-side and optional**, so an agent that does not see it
  advertised must not call it. Implementing it is purely additive. Wire
  v1 is stable under a vendor-neutral org with a public RFD process; v2
  is a draft whose own announcement says adding it must not mean
  dropping v1.

  What makes it worth doing is what ACP omits: no permission model, no
  sandboxing, no timeout. Those are correct omissions for a protocol and
  a real gap for whoever hosts one — every ACP client today runs an
  agent's commands the way `bash -c` would. gish has sandbox profiles
  and a deadline on every call already.

  Recorded caveat, so it is not re-litigated into a slogan: the
  *user-facing* claim ("people are leaving zsh because agents assume
  bash") is **not evidenced** — 2 HN accounts, 0 of 827 in a Reddit
  corpus. Build it for the substrate, not the story (#169).

- **gish claims bash's interface, not bash's identity** (2026-08,
  [#120](https://github.com/blairham/gish/issues/120)): `BASH_VERSION`
  and `BASH_VERSINFO` report a modern bash; `$0` stays `gish`, and
  `GISH_VERSION` says exactly what is running.

  The issue's own recommendation was the opposite — claim nothing, and
  shim per tool — with one stated condition for revisiting: evidence
  that specific popular tools are unusable *and* that their bash hook
  passes. The #159 matrix produced exactly that. fzf chooses between two
  Ctrl-T implementations on `((BASH_VERSINFO[0] < 4))`: a readline
  *macro* built from editing commands including `shell-expand-line`,
  which gish does not emulate and will not, or `bind -x`, which gish
  implements. Unset, that arithmetic reads 0 — so refusing to claim a
  version handed gish the one path it cannot run, in order to avoid
  claiming a capability it has.

  The distinction that makes this honest rather than impersonation:
  these variables are used as **feature probes**, and the features they
  gate are ones gish implements (`PROMPT_COMMAND`, `PS0`, the DEBUG
  trap, `bind -x`, `complete`/`compgen`). The **identity** question is
  answered truthfully — `$0` is what a script re-execs and what a user
  sees, and lying there would be a lie a program could act on. Where the
  claim does outrun the substrate, docs/compat.md already lists the gaps
  by name and `doctor` reports them.

- **One theme engine, two config dialects** (2026-08,
  [#134](https://github.com/blairham/gish/issues/134)): `p10k` is the
  engine; the `gish` theme's knobs (`GISH_THEME_SEGMENTS`,
  `GISH_THEME_COLOR_*`, `_LINES`, `_FRAME`, `_SEP`, `_RPROMPT`) are a
  second, smaller dialect over the same idea, and they stay — they are a
  documented surface and [docs/stability.md](stability.md) says
  documented surfaces do not move.

  What does not stay is the pretence that they are two unrelated themes.
  The p10k engine is a strict superset in capability: six presets, ~50
  segments, a parameter namespace with a real fallback chain. `gish` is
  six segments and five knobs, which is the right size for someone who
  wants a prompt configured the way the rest of gish is configured, and
  the wrong size for someone arriving with a 1720-line `.p10k.zsh`.

  So: `config theme.*` and `p10k configure` both keep working and are
  described as what they are — the small dialect and the compatibility
  one. The renderer behind `gish` is not deleted, because deleting it
  buys one file and costs every rc that sets those knobs a behavior
  change, and "your config will not break" outranks "there is one of
  these". The third option — treating `POWERLEVEL9K_*` as an import
  format only — was rejected outright: "paste a line from your old
  config and it works" is a real part of why the port is attractive, and
  it is exactly the property an import-only surface destroys.

- **Structured data uses `test`'s operator vocabulary, or not at all**
  (2026-08, [#104](https://github.com/blairham/gish/issues/104),
  docs/structured.md): the exploration turned up that the shape everyone
  writes for this feature — `... | where %cpu > 50` — parses today as
  `where %cpu` with stdout redirected, silently **creating a file named
  `50`**. "Zero new grammar" and "nushell's comparison syntax" cannot
  both hold, because `>`/`<`/`|` are the most load-bearing characters in
  shell grammar, and the bash parser being the contract is the entire
  differentiator over nushell.

  POSIX already solved this for `test` in the 1970s for the same reason,
  so the verbs speak the operators shell users already have in their
  fingers: `where %cpu -gt 50`. Plain words, no quoting, no redirect, no
  new grammar. It does not look like nushell, which is the correct trade
  — looking like nushell is worth nothing while having nushell's
  adoption problem. Sequenced after v1; if cut, the honest reason is
  that the native eight-tool stack already covers the jq/awk slot.

- **One prompt pipeline; a small, frozen escape set** (2026-08,
  [#109](https://github.com/blairham/gish/issues/109)): the #6
  `GISH_PROMPT` expander was explicitly a stopgap until the prompt
  engine existed. It does now, so the manual override became the
  **"literal" theme** — every prompt (plain, p10k, starship, plugin,
  manual) renders through one dispatch, and renderer fixes land once
  instead of twice or drifting.

  The escape set was decided rather than accreted, because v1 freezes
  it: `%n`/`%u` user, `%m`/`%h` host, `%~`/`%w` cwd, `%W` basename,
  `%d` full cwd, `%?` exit status, `%#` prompt char, `%p{id}` plugin
  segment, `%%` literal. zsh's spellings are first-class aliases — the
  person writing `GISH_PROMPT` is usually porting a zsh `PROMPT` and
  their fingers know `%n`/`%m`/`%~` (the #96 lesson). Unknown escapes
  pass through untouched; gish does not grow zsh's full `PROMPT_SUBST`
  surface by accident.

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

- **The ssh story is a pushed binary, not a presence on the box**
  (2026-08, [#98](https://github.com/blairham/gish/issues/98)):
  **`gish ssh` copies a binary, execs it for the interactive session,
  and leaves nothing else behind.** The remote `$SHELL` is never
  changed and remote dotfiles are never written — the POSIX-clean
  non-interactive contract (#41) exists precisely so `ssh host cmd`,
  `scp`, `rsync`, and git-over-ssh keep working, and a shell that
  announces itself from a remote rc file breaks all four. Every failure
  path — no writable directory, `noexec` on every candidate, unsupported
  platform, probe timeout — lands in plain `ssh` with one line on
  stderr: the scenario being sold is the 2AM incident box, so
  bring-along machinery that delays a shell is negative value. Ruled
  out: **a persistent remote daemon** (mosh/Eternal Terminal), which
  contradicts "nothing persists beyond the dropped binary" and makes
  every remote box a support and security surface — the whole point of
  the single static binary is that there is nothing to run; **shipping
  commands from a local shell to a remote executor**, which buys a fully
  local UX by reimplementing `cd` persistence, job control, Ctrl-C, and
  every full-screen program over a command channel (a one-shot
  `gish exec host -- cmd` is fine; a session is not); and
  **auto-launching from the remote `~/.bashrc`**, the obvious shortcut
  to "gish on every login", which is the rc-file rule above restated as
  a footgun. Do not re-propose without new facts.

## Open questions

- Prompt styling vocabulary for tier 2 (`RenderResponse.text`): markup subset vs. structured spans. Raw text until decided.
- Tier-1 widget shim depth: which zle APIs are worth emulating vs. declaring out of scope.
- ~~**`EnvProvider`** (direnv-class)~~ — resolved (#12): both mechanisms, layered. Trust is per-(plugin, directory, diff-hash) — nothing applies without an explicit `trust allow`, and a changed diff re-pends (direnv's edit-reprompts semantics). A deny-list (loader hooks, `IFS`, startup-file vars, `GISH_*`) is stripped host-side before a proposal exists; `PATH` is settable but only ever through the visible allow flow. Requests carry the allowlisted env subset. Applied diffs revert when the shell leaves the proposal's subtree.

The plugin roadmap itself — which plugins, in what order, under which latency budgets — lives in [plugins.md](plugins.md).
