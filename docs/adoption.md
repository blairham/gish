# adoption

How shells actually gain and lose users. Recorded from the Aug 2026
lifecycle sweep — growth winners, failure autopsies, switch-back
attrition, adjacent-tool playbooks — so the roadmap argues from evidence
rather than memory, the way docs/strategy.md and docs/user-research.md
already do.

docs/strategy.md answers *what gish should claim*. This page answers
*what determines whether anyone ever runs it*, and the two disagree
about nothing: distribution beats merit in both.

**Provenance.** Install counts, release counts, star counts and dates
are verified and cited below. The maintainer-narrative quotes from
Oils, xonsh, elvish, Warp and Fig were partially reconstructed after
the research run was interrupted — they are marked ⚠ where they appear
and **must be re-verified against the primary source before any public
use**. The conclusions do not rest on them; the numbers carry the
argument on their own.

## The composite law

> adoption ceiling ≈ **min**(distribution, bash-interop, first-minute
> delight, stability contract, maintainer count, user trust)

It is a minimum, not a sum, and that is the whole finding. Every shell
in the failure set maxed five factors and zeroed one. No amount of
excellence in the other five compensated.

| project | the five it had | the one it zeroed | measured outcome |
| --- | --- | --- | --- |
| **fish** | all six | — | ~193k brew installs/yr; 27% of Arch boxes. The only alt-shell to max the set |
| **nushell** | distribution, delight, trust, maintainers, interop-by-choice | **stability** — 7 years, ~115 releases, still 0.x; a core-team issue in May 2026 openly asks whether 1.0 should happen at all | ~49k brew installs/yr |
| **Oils** | interop (deepest bash compat in the field), stability posture, trust | **first-minute delight** — nothing visibly better in the first minute | 273 brew installs/yr despite a decade of front-page coverage |
| **brush** | interop, delight, trust | **distribution + maintainer count** | 423 brew installs/yr |
| **elvish / xonsh** | delight, stability, trust, maintainers | **bash-interop** | never left the low thousands |
| **Warp / Fig** | distribution, delight, maintainers, stability | **trust** — Fig acqui-killed ~6 months after acquisition; Warp required a login until Nov 2024 | Warp's 2026 open-sourcing scored **3 points** on HN: the audience had already left |
| **powerlevel10k** | everything a prompt can have — 55k stars | **maintainer count** | on life support at peak popularity |

Two consequences for gish:

1. **Our zero is maintainer count.** It is the scarcest resource here
   and it is the factor that killed a 55k-star project. Every decision
   that widens the maintenance surface — a cross-shell port, an
   ecosystem we host rather than inherit, a second config dialect —
   spends the one budget we cannot refill. #214 is this law applied.
2. **Trust decays irreversibly.** Warp's is the only failure in the set
   that could not be undone by later doing the right thing. That is why
   the trust paragraph ships *before* launch (#212) rather than being
   conceded afterwards, and why #210's re-touch loop may not phone home
   by default.

## The churn funnel

Where switchers actually leave, in the order they hit it. Each stage is
a documented first-person pattern from the attrition corpus, not an
inferred one.

| stage | what breaks | gish's answer | state |
| --- | --- | --- | --- |
| **minutes** | keybindings that don't answer; the `chsh` ask turning a trial into a commitment (fish's lockout stories: login shell absent from /etc/shells, distros requiring a Bourne-compatible login shell that reads /etc/profile) | readline parity (#96, #118, #116); never require chsh — terminal-profile launch is the happy path | parity **shipped**; exit-cost surfaces **open** (#212) |
| **week 1** | source-based tooling: nvm, rbenv, pyenv, conda, rvm, venv, virtualenvwrapper, direnv. A shell that cannot source them is unusable on day two, and it fails silently | the #161 source gate loads all of them unmodified, published in docs/interactive-compat.md and CI-enforced | **shipped** |
| **month 1** | the bash monoculture: the lookup tax on every pasted answer, ssh fleets where your shell isn't installed, and pairing — the coworker driving your terminal | bash-paste compat (docs/compat.md), `gish ssh` (#98), `gish migrate` (#160) | **shipped**; team-shareable config **open** (#209) |
| **months+** | update breakage and trust shocks — the nushell treadmill, the Warp login | published stability contract (#162), version-independent plugin ABI with CI enforcement (#168), frozen-additive protos | substance **shipped**; the version number does not yet say so (#213) |

The funnel's shape is the argument for #213. gish has already paid for
the months+ stage and gets no credit for it, because `0.0.x` reads as
"your investment may be taxed at any release" regardless of what the
contract says.

## Growth mechanics, ranked by transferability

1. **Default slots** — zsh's rise was Apple's decision, and the causal
   evidence is in docs/strategy.md. The slots still courtable in 2026
   are devcontainers, Codespaces, bootstrap repos, and security distros
   (Kali chose zsh *for its ecosystem*, which is a decision an
   ecosystem-inheriting shell can contest).
2. **The compat scoreboard** — Bun 1.2 (Jan 2025) answered a year of
   "it broke my app" by adopting the incumbent's own definition of
   correct: run Node's test suite on every change, publish per-module
   pass rates. Complaints "quieted down considerably" within months.
   The mechanism is asymmetric: users forgive a *documented* 5% gap and
   punish an *undocumented* 1% gap, because the undocumented one
   arrives at 2AM. #211 is this move, and the corpus discipline is
   already in place to receive it.
3. **Compat first, opinions later** — Neovim inherited vim's config and
   grew; Helix asked for new muscle memory and did not. Sequencing, not
   quality.
4. **Retention loops** — only two exist in the entire history of this
   ecosystem: Oh My Zsh's auto-updater (shipped ~3 weeks in; Robby
   Russell's retrospective calls it the turning point) and atuin's sync
   accounts. gish currently has neither; a `v0.0.x` install just ages.
   #210, under the no-telemetry constraint.
5. **Exit-cost engineering** — uv's skeptics never blocked adoption
   because rollback was free and forkability was the standing answer;
   ripgrep could refuse grep compatibility outright because quitting
   cost nothing. Market the rollback as loudly as the install (#212).
6. **Contribution surface** — fish's Rust port took its contributor
   count from 17 to 200+. Small, obvious, <50-line first PRs are the
   funnel; this is the only mechanic that directly addresses our zero.
7. **Relaunch events** — a rewrite, a 1.0, or a port is a second
   first-impression, and the only sanctioned way to get one.
8. **Screenshots as the ad unit** — the prompt is the product's face in
   every thread it appears in.

## What does not transfer

- **Waiting to be picked as a default.** It happened once, to zsh, by
  Apple, for reasons outside the project's control.
- **Corporate force.** PowerShell had Microsoft behind it and still
  lost on Unix. Distribution muscle does not substitute for interop.
- **Feature superiority alone.** Any copyable UX is absorbed by the
  incumbent's add-ons within a year or two — ble.sh does fish-grade
  highlighting and autosuggestions in pure bash, IRIS delivers
  IntelliSense-grade completion into the shell you already run. This is
  the csh ending from docs/strategy.md, and it is why the exec path
  (which an add-on structurally cannot own) is the defensible asset.

## What this drives

| finding | issue |
| --- | --- |
| the version number is unclaimed anti-churn signal | #213 — declare 1.0 early |
| exit cost is the first-10-minutes stage | #212 — print the revert, own /etc/shells |
| the incumbent's own test suite is the number skeptics can't argue with | #211 — compat scoreboard round 2 |
| no retention loop exists | #210 — opt-in re-touch, never phones home |
| shell choice is partly a group choice | #209 — team-shareable config |
| every add-on we ship removes a reason to switch | #214 — no cross-shell components |
| the 2026 interop anchor is the agent subshell, not StackOverflow | #208 — the AI-agent shell story |

## Sources

- Homebrew per-formula install analytics, measured 2026-08 — the only
  per-tool install census that exists, and the reason #164 (core, not a
  tap) is also a measurement decision.
- nushell: release history and the May 2026 core-team 1.0 issue;
  first-person switch-back accounts in the attrition corpus (827
  comments, r/commandline and HN).
- Bun 1.2 release notes and the Node test-suite pass-rate pages (Jan
  2025).
- Oh My Zsh: Robby Russell's 10-year retrospective.
- fish: the Rust port's contributor statistics; Arch `pkgstats`.
- Warp: HN discussion of the 2026 open-sourcing (3 points); the Nov
  2024 login-requirement change. Fig: acquisition and shutdown dates.
- ⚠ Maintainer-narrative quotes from Oils, xonsh, elvish, Warp and Fig
  are reconstructed and unverified — see the provenance note above.
- The demand half of the Reddit corpus was never gathered; #171 carries
  the method and the two access constraints (reddit.com returns 403 to
  server-side fetchers; api.pullpush.io asked us to stop, which is an
  operator policy to respect). Until it is answered, nothing here
  claims users *want* one coherent tool over the modular stack — see
  the open question in docs/strategy.md.
