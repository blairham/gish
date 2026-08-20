# announcement playbook

The launch plan for koi, derived from how shell announcements
actually land: fish, nushell, and ShellGPT threads that worked; murex's
2-point silence; Warp's trust backlash. Every rule below is someone
else's scar tissue.

## The one idea

> **fish-quality interactive UX, and your bash muscle memory and pasted
> one-liners still work.**

One idea in the title. Everything else is body copy. Feature-list
titles read as unfocused and die — the threads that worked led with a
single sentence a tired developer could evaluate in two seconds.

## Rules

**Never headline "gRPC".** It reads as enterprise-speak and invites the
question "so there's RPC in my keystroke path?" Frame the plugin system
by what it buys: *plugins in any language, out of process,
crash-isolated, deadline-bounded — they can never block a keystroke or
take down your shell.* Keep `docs/bench.md`'s p50/p99 in the body, ready
before anyone asks.

*The name carries no backronym to defend.* A koi is a fish, which says
the positioning without a sentence of explanation: fish's out-of-box
experience is the thing being chased. There is no naming tension to
defend because the plugin architecture is not the claim — see
docs/strategy.md, which records why the earlier name and its expansion
were dropped.

**State the trust stance up front, in bold.** Open source. Local-first.
No account. No telemetry. AI is opt-in (`??` only), bring-your-own
provider including local models, preview-before-execute — output lands
in the editor buffer, never auto-runs. The delta between ShellGPT's
117-point reception and Warp's login backlash was entirely trust.

**Never require `chsh`.** Document terminal-emulator launch as the happy
path. Reversibility is what makes people willing to *recommend* it —
"try it in one tab" converts, "make it your login shell" does not.

**Market the rollback as loudly as the install.** Exit cost is
the forgotten half of every fast-adoption story: uv's skeptics never
blocked adoption because rollback was free, and ripgrep could refuse
grep compatibility outright because quitting cost nothing. koi says
this itself — the first interactive run prints one dim line stating
that nothing was changed and naming the uninstall command — and the
`chsh` instructions lead with `/etc/shells` and carry the `chsh -s
/bin/zsh` that undoes them. `doctor` reports login-shell state and knows
the way back. Say all of this in the post; it is a conversion asset, not
a disclaimer.

**State the counterparty risk, before anyone asks.** MIT, no account, no
telemetry, no CLA assigning copyright to a company, no hosted service in
the core path. Fig was acqui-killed roughly six months after
acquisition; Warp required a login until Nov 2024 and its 2026
open-sourcing scored 3 points on HN because the audience had already
left. Trust decays irreversibly (docs/adoption.md), so this paragraph
ships *before* launch rather than being conceded after.

**Concede the ssh objection, then flip it.** Yes, koi isn't on the
remote box. It's a local daily driver whose bash compatibility means
zero context-switch when you land on a server. `koi ssh` is the real
answer, and saying "not yet, here's the plan" beats deflecting.

**Host agents; don't claim to be one.** The AI line is `??` and
`explain` — opt-in, preview-before-execute, bring-your-own-provider. The
`agent` builtin exists but is experimental and frozen, and stays
out of the pitch entirely: "shell with a built-in AI agent" is the
Warp-shaped red flag. The *strong* AI claim is the inverse — run your
coding agent under `koi --sandbox workspace` and the shell confines what
it can touch. That is a claim no agent CLI can make about itself.

**No "future of the terminal" tone.** Warp poisoned that well. Say what
it does; let the numbers carry the ambition.

**Be in the thread.** Author responsiveness is the single strongest
correlate of thread success (nushell's playbook). Answer every
objection, including the hostile ones, including "why not just zsh".

## Launch-gating artifacts

| artifact | state |
| --- | --- |
| `docs/compat.md` — the bash scoreboard | **done** — 87% against bash 5.3, gaps published |
| `docs/bench.md` — startup + keystroke p50/p99 | **done** — 5.6ms all-features start, p50 0.2ms keystroke |
| `docs/porting.md` — muscle-memory porting guide | **done** |
| README leading with the one idea | **done** |
| 3 asciinema/GIF demos | **open** — needs a human at a terminal |
| Packaging: brew | done (tap ships today) |
| Packaging: AUR, nixpkgs | **open** — needs accounts and a maintainer |
| Packaging: winget/scoop | deferred with the native Windows interactive port, sequenced to v1.x |

The three demos, when someone records them:

1. **The daily driver** — autosuggestions and syntax highlighting while
   typing, then paste a gnarly bash one-liner and watch it just run.
2. **The safety story** — kill a plugin mid-session and show the prompt
   not flinching; then let the sandbox catch an `rm -rf` in an
   AI-composed command.
3. **The config story** — `config theme` wizard start to finish, ending
   with a prompt the viewer wants.

## Audience notes

- **zsh/oh-my-zsh refugees** — lead with the benchmark table and "delete
  eight `eval "$(tool init)"` hooks". Their pain is a 266ms prompt and a
  .zshrc they're afraid to touch.
- **fish users** — "everything you love about fish, and bash still
  pastes". They already believe in good defaults; the compat is the
  news.
- **people whose coding agent fights their shell** — the 2026 version of
  the paste problem, and the one audience with a claim no other modern
  shell can match: *the only shell that works as an AI agent's `SHELL`,
  and the one place that can sandbox what the agent runs*. Their pain is
  documented in someone else's tracker (anthropics/claude-code issues
  11475, 7490, 13144, 19983) and their current workaround is the
  dual-shell split — fish for humans, zsh for agents — which is the
  partition koi collapses. Point at `docs/agents.md`: the recipe is one
  symlink, the contract is CI-gated, and it found two real bugs on its
  first run. Do not overstate it — several agents ignore `$SHELL`
  entirely, the recipe works around that rather than fixing it, and
  saying so is what makes the rest credible.
- **terminal geeks** — the plugin contract demo and the sandbox. They
  will ask about the keystroke path within one comment.

## Success metric

Fish-sized, not Warp-sized, won a thousand enthusiasts at a time. The
only per-tool census that exists is Homebrew's, and it puts the ceiling
at fish's ~193k installs/yr — that is the target shape, over about ten
years (docs/adoption.md). A 90-point thread with the author answering
every reply is the shape of success; a viral spike with a trust
backlash is not.

*The "roughly 7% of developers customize their shell" figure is
deliberately absent from this section* — it is unsourced, and
docs/strategy.md records why it and two other numbers were dropped.

## What we will not claim

- Not "Windows support" — the native interactive port is sequenced to
  v1.x, and WSL2 is the documented story until then.
- Not "your zsh plugins all work" — tier-1 is a scoped escape hatch,
  and the honest line is that koi ships the top plugins' *behaviors*
  natively.
- Not a bash-compat percentage without the corpus behind it —
  `docs/compat.md` states its own ceiling, and the number moves when the
  corpus grows.
