# strategy

What the research actually supports, what it does not, and what that
means for what koi claims. This page exists because three numbers we
were repeating are wrong and one positioning premise was overturned —
and because the corrections are more useful than the claims
were.

## Three numbers to stop using

**1. "~7% of developers customize their shell."** Unsourced. JetBrains'
DevEco survey contains no shell-usage question at all. Delete it; do
not replace it with an estimate.

**2. "Add-on tools out-install new shells 10:1."** Overstated.
Measured on Homebrew installs: fzf/fish is **3:1**, starship/fish is
**1.3:1**, aggregate ≈ **6.7:1** — and the aggregate is carried by mise
and fzf, not by prompt or UX tools. fish ≈ zsh on Homebrew. The honest
version of the claim is narrower and still useful: *people install
add-ons far more readily than they change shells*, which is an argument
for inheriting the ecosystem, not for a 10:1 headline.

**3. Stack Overflow has never surveyed shells.** "zsh" appears zero
times in the 2021–2025 surveys; everything is lumped into "Bash/Shell
(all shells)", and the 33.9% → 48.7% jump between 2024 and 2025 is a
methodology change, not adoption. **Nothing should be built on that
survey**, in either direction.

## The premise that was wrong

An earlier framing held that the tier-2 plugin system is the
differentiator — "so it's in the name". The research falsifies that:

- **nushell already ships out-of-process plugins in any language.** The
  architecture is not unique. What is unique is the *distribution
  discipline* around it: a version-independent ABI and a
  deadline nobody can block — and even those are advantages, not moats.
- **Every fish-parity UX feature is replicable as a zsh or bash
  add-on.** `ble.sh` does fish-grade syntax highlighting and
  autosuggestions **in pure bash**. IRIS delivers IntelliSense-grade
  completion *into* existing shells as a single Go binary. A UX feature
  that an add-on can deliver into the shell someone already uses is not
  a reason to switch shells.

  The tempting inversion — ship koi's own UX as an add-on, meet people
  in the shell they run, convert later — is **decided against** and
  recorded in docs/design.md. Briefly: it is ble.sh's and IRIS's
  game rather than ours, every add-on removes a reason to switch, fish's
  plateau is partly its features leaking out as zsh plugins, and N shells
  × M integrations spends the maintainer count that docs/adoption.md
  identifies as koi's zeroed factor.

What is actually defensible is narrower and more durable:

1. **Bash compatibility** — the historical *gate*, not a feature
   alongside the UX work. See below.
2. **The exec path** — sandbox profiles, deadlines, parse-tree
   footgun detection, secret-free history. This is the one layer an
   add-on provably cannot own, because an add-on lives inside the shell
   it is extending and does not control execution.

Everything else is table stakes we happen to have.

## What actually decides shell adoption

The lifecycle evidence behind this section — the composite law, the
churn funnel stage by stage, and the growth mechanics ranked by whether
they transfer — is recorded in [docs/adoption.md](adoption.md).
This page says what koi should *claim*; that one says what determines
whether anyone ever runs it.

**Distribution, not merit.** zsh's rise was caused by Apple making it
the default in Catalina, and the evidence is causal rather than
correlational: Wikipedia pageviews for `Z_shell` sat flat at ~6,500/mo
for seventeen months, then stepped to 20,009 at WWDC (June 2019) and
21,449 at ship (October 2019). Within Stack Overflow's `macos` tag, the
zsh:bash ratio went 8.9% (2018) → 75.5% (2022).

**Oh My Zsh did not cause it.** Its star-acquisition rate was flat at
40–55/day for a decade with no step change at Catalina, and its SO tag
*lagged*, doubling only in 2020. The framework rode the default; it did
not create it.

**fish's disqualifier for the Apple slot was sh-incompatibility, not
licensing.** fish is GPLv2, and Apple still ships GPLv2 bash 3.2.57
today. Compatibility was the gate.

**The risk is the csh ending.** tcsh won on interactive merit, and then
bash absorbed its features onto a compatible base. The threat to koi is
not nushell; it is **zsh or fish absorbing bash-paste compatibility
before koi has users**, or an add-on (ble.sh, IRIS) delivering the UX
into the shell people already run.

**The realistic ceiling is fish-sized, over about ten years.** Every
shell success took ~3 years *with* a default handed to it, or 15–20
without one.

## What this means for the claims we make

- **Lead with compatibility and the exec path.** "Your existing scripts,
  rc files and tools work — and the shell is the one place that can
  sandbox a command, bound a plugin, and refuse to record a secret."
- **Never headline the plugin architecture.** It is real, it is good,
  and it is not why anyone would switch. docs/announcement.md already
  says never headline gRPC; the reason is now recorded rather than
  defensive.
- **Retire the backronym, and then the name.** "gRPC Interactive SHell"
  went first: it is no longer the story, so the "naming tension"
  paragraph that existed to defend it is unnecessary. `gish` followed,
  because with the expansion gone it meant nothing and carried three
  costs a mainstream name cannot: "Gish gallop" is a Merriam-Webster
  and Wikipedia entry, `gi-` has no settled pronunciation (the GIF
  problem), and several other `gish` CLIs already rank for it — three of
  them git shells. `koi` is a fish, which states the lineage rather than
  defending it.
- **Measure, publish, and let the numbers be unflattering.**
  docs/compat.md, docs/bench.md and docs/interactive-compat.md are the
  model: a corpus that grows can lower a percentage, and that is the
  point.

## Competitors worth watching

Re-measured 2026-08-17 from the GitHub API, in
[docs/competitors.md](competitors.md) — including one correction: brush
is *actively* developed (81 commits in the last quarter), so "cadence
slowed" was reading a slow release schedule as a slow project.

| project | what it is | why it matters |
| --- | --- | --- |
| **brush** (Rust, 2,154★) | our exact top-line pitch: run your scripts and .bashrc unchanged, with highlighting and autosuggestions; ~1,700 compat tests | actively developed (81 commits/quarter); the distance is the exec path and ecosystem inheritance, not the compat percentage |
| **IRIS** (Go, 1,222★ in four months) | IntelliSense-grade completion delivered *into* zsh/bash/fish; one binary, no account, no telemetry | the strategically dangerous one: it aims at the UX pillar without asking anyone to switch shells |
| **ble.sh** (4,618★) | fish-grade highlighting and autosuggestions in pure bash | absent from Homebrew core, so its true install base is unmeasurable; the star count may reflect distribution friction rather than absent demand |

## The open question

**Is there evidence people are tired of assembling starship + zoxide +
atuin + fzf + direnv and want one coherent thing — or do they actively
prefer the modular stack?**

The **behavioural** half is now measured, on two unrelated censuses
(docs/competitors.md, 2026-08-18): people assemble the stack and *keep*
it. Debian popcon use rates run 51–62% for direnv, starship, atuin and
mise, against 26% for nushell, 28% for xonsh and 12% for elvish. Tools
that add to the shell someone already runs get retained; shells that
replace it get abandoned. That rules out the comforting reading — the
stack is not a burden people are waiting to shed — and it is the half
that should drive decisions.

The **sentiment** half is still ungathered, and it is the part that would
say whether those same people would have preferred one coherent tool.
docs/competitors.md carries the method, including the two access notes
that matter:
reddit.com returns 403 to server-side fetchers, and `api.pullpush.io`
asked us to stop — an operator policy to respect, not a throttle to pace
around. The sanctioned route is Reddit's official OAuth API, which needs
credentials a human creates.

Either way the posture is the same, and now for a measured reason rather
than as a hedge: **inherit the stack rather than replace it**. The
sentiment answer changes the marketing, not the architecture.
