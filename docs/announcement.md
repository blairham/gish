# announcement playbook

The launch plan for gish (#106), derived from how shell announcements
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

*Naming tension, stated openly:* the project's name expands to "gRPC
interactive shell". That expansion belongs in the docs, not the
headline. If the name itself becomes the objection, the honest answer
is that the acronym describes the plumbing, not the pitch.

**State the trust stance up front, in bold.** Open source. Local-first.
No account. No telemetry. AI is opt-in (`??` only), bring-your-own
provider including local models, preview-before-execute — output lands
in the editor buffer, never auto-runs. The delta between ShellGPT's
117-point reception and Warp's login backlash was entirely trust.

**Never require `chsh`.** Document terminal-emulator launch as the happy
path. Reversibility is what makes people willing to *recommend* it —
"try it in one tab" converts, "make it your login shell" does not.

**Concede the ssh objection, then flip it.** Yes, gish isn't on the
remote box. It's a local daily driver whose bash compatibility means
zero context-switch when you land on a server. #98 is the real answer,
and saying "not yet, here's the plan" beats deflecting.

**Host agents; don't claim to be one.** The AI line is `??` and
`explain` — opt-in, preview-before-execute, bring-your-own-provider. The
`agent` builtin exists but is experimental and frozen (#111) and stays
out of the pitch entirely: "shell with a built-in AI agent" is the
Warp-shaped red flag. The *strong* AI claim is the inverse — run your
coding agent under `gish --sandbox workspace` and the shell confines what
it can touch. That is a claim no agent CLI can make about itself.

**No "future of the terminal" tone.** Warp poisoned that well. Say what
it does; let the numbers carry the ambition.

**Be in the thread.** Author responsiveness is the single strongest
correlate of thread success (nushell's playbook). Answer every
objection, including the hostile ones, including "why not just zsh".

## Launch-gating artifacts

| artifact | state |
| --- | --- |
| `docs/compat.md` — the bash scoreboard (#101) | **done** — 87% against bash 5.3, gaps published |
| `docs/bench.md` — startup + keystroke p50/p99 (#102) | **done** — 5.6ms all-features start, p50 0.2ms keystroke |
| `docs/porting.md` — muscle-memory porting guide (#96) | **done** |
| README leading with the one idea | **done** |
| 3 asciinema/GIF demos | **open** — needs a human at a terminal |
| Packaging: brew | done (tap ships today) |
| Packaging: AUR, nixpkgs | **open** — needs accounts and a maintainer |
| Packaging: winget/scoop | deferred with #89 per the ratified #110 |

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
- **terminal geeks** — the plugin contract demo and the sandbox. They
  will ask about the keystroke path within one comment.

## Success metric

Fish-sized, not Warp-sized: the roughly 7% of developers who customize
their shell at all, won a thousand enthusiasts at a time. A 90-point
thread with the author answering every reply is the shape of success;
a viral spike with a trust backlash is not.

## What we will not claim

- Not "Windows support" — the native interactive port is sequenced to
  v1.x (#110), and WSL2 is the documented story until then.
- Not "your zsh plugins all work" — tier-1 is a scoped escape hatch
  (#105), and the honest line is that gish ships the top plugins'
  *behaviors* natively.
- Not a bash-compat percentage without the corpus behind it —
  `docs/compat.md` states its own ceiling, and the number moves when the
  corpus grows.
