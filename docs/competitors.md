# The competitive picture, re-measured

The August 2026 sweep lost two of six research streams and asked to be
re-run before "nobody owns the quadrant" was treated as settled (#171).
This is that re-run for the *measurable* half — repository facts, taken
from the GitHub API on **2026-08-17**, not from memory or from the
projects' own copy. Install and usage measurements were re-taken on
**2026-08-18** from two unrelated censuses — Homebrew's analytics and
Debian popcon — which is what closed the one question the first pass
left as unmeasurable (ble.sh, below).

The other half — the Reddit *sentiment* sweep — is **not done**, and the
reason is recorded at the bottom rather than papered over. What the
censuses could answer about demand is recorded there too, along with what
they could not.

## The three that matter

### brush — the same pitch, in Rust

| | |
| --- | --- |
| stars | 2,154 |
| language | Rust |
| last push | 2026-08-17 (today) |
| commits, last 90 days | 81 |
| releases | v0.4.0 (2026-05-03), v0.3.0 (2025-11-17), v0.2.23 (2025-08-30) |

Its top line is koi's: *run your existing scripts and `.bashrc`
unchanged, with syntax highlighting and autosuggestions built in*, with
~1,700 compatibility tests behind it.

The previous sweep called its cadence "slowed to two releases in twelve
months" and treated that as a weakness. **That reading no longer holds**:
81 commits in the last quarter and a push today are an actively
developed project with a slow *release* cadence, which is a different
thing. What is still true is the shape of the gap: no plugin system,
TOML configuration rather than a shell rc, and — the part that matters
most — no exec-path story. Nothing in brush sandboxes a command, bounds
a plugin, or refuses to record a secret.

**Posture: watch, and do not race it on compatibility percentage.**
Both projects will converge there. The distance is the exec path and the
ecosystem inheritance (#159), which is where the work has been.

### IRIS — the dangerous one

| | |
| --- | --- |
| stars | 1,222 |
| created | 2026-04-06 (**four months old**) |
| language | Go |
| licence | 0BSD |
| last push | 2026-08-16 |
| release cadence | v0.6.2, v0.6.3 and a nightly within four days |

IntelliSense-grade completion delivered **into existing zsh/bash/fish**,
one binary, no account, no telemetry. 1,222 stars in four months and
multiple releases a week.

This is the strategically dangerous pattern, and it is the one
docs/strategy.md names: a UX feature an add-on can deliver into the
shell someone already runs is not a reason to switch shells. IRIS aims
squarely at the completion pillar and asks for nothing in return.

**Posture: this validates the strategy correction rather than
threatening the product.** The answer is not to out-complete IRIS; it is
that completion is table stakes and the defensible layer is the one an
add-on cannot reach. If IRIS ever ships an ACP or plugin surface, the
right move is to *consume* it, the way koi consumes carapace.

### ble.sh — measurable after all, and the number is the finding

| | |
| --- | --- |
| stars | 4,618 |
| language | Shell (pure bash) |
| last push | 2026-08-11 |
| Homebrew core | **not a formula** — `brew info ble.sh` says no such formula |
| Debian popcon | **19 installations, 0 in recent use** (2026-08-18) |

The previous entry said its install base "cannot be measured the way
everything else in this category can", and floated the hypothesis that
its low star count "may reflect distribution friction rather than absent
demand for exactly our thesis". Homebrew cannot measure it. **Debian
popcon can**, and it answers the hypothesis rather than leaving it open:
19 installations against fish's 5,852 in the same census, and of those 19
not one shows recent use.

That does not mean nobody runs ble.sh — its documented install path is a
git clone, which no package census sees, so 19 is a floor rather than a
count. What it does mean is that the *distribution-friction* explanation
does not survive: where ble.sh **is** packaged, essentially nobody has
taken it, in a population of 221,673 reporting installations. The
add-on-into-bash pattern is the weakest-footprint thing in this whole
category, which is independent evidence for the decision recorded in
docs/design.md not to ship koi's UX that way (#214).

**A methodological trap, recorded because it nearly went into this
page.** Homebrew's analytics feed *does* list `ble.sh` with 82 installs
in 365 days. That is not 82 installs of a core formula: 39,422 of the
49,271 names in that feed are tap-qualified, and `ble.sh` resolves to no
formula at all. Presence in the analytics feed is not proof a formula
exists, so a number taken from it without a `brew info` check is a number
about nothing.

## Two censuses, and what they agree on

Homebrew installs over 365 days, read from `formulae.brew.sh` on
**2026-08-18**, alongside Debian popcon the same day. Two unrelated
populations — macOS-heavy and Linux/server-heavy — which is the point:
the numbers this project argues from should not depend on one of them.

| project | brew installs/yr | popcon installs | popcon in recent use | use rate |
| --- | --- | --- | --- | --- |
| bash | 374,214 | 282,094 | 251,748 | 89% |
| zsh | 193,885 | 19,706 | 11,649 | 59% |
| **fish** | **195,186** | **5,852** | **3,068** | **52%** |
| nushell | 51,021 | 100 | 26 | 26% |
| xonsh | 4,232 | 166 | 46 | 28% |
| elvish | 321 | 60 | 7 | 12% |
| murex | 326 | — | — | — |
| oils-for-unix | 273 | — | — | — |
| brush | 422 | — | — | — |
| ble.sh | not a formula | 19 | 0 | 0% |
| — *the unbundled stack* — | | | | |
| mise | 798,996 | 208 | 108 | 52% |
| fzf | 591,308 | 9,455 | 2,325 | 25% |
| starship | 254,479 | 780 | 486 | 62% |
| zoxide | 161,233 | 2,161 | 930 | 43% |
| atuin | 143,945 | 234 | 120 | 51% |
| direnv | 131,435 | 1,117 | 682 | 61% |

Three things this settles, and one it does not.

**The recorded numbers hold.** docs/strategy.md's corrected ratios
re-measure cleanly a day later: fzf/fish is 3.03:1 (recorded as 3:1),
starship/fish is 1.30:1 (recorded as 1.3:1), and fish ≈ zsh on Homebrew
(195,186 vs 193,885). nushell's popcon use rate is 26% against fish's
52%, which is the figure the "27% vs 52%" line records. Nothing here
needed retracting, which is worth knowing about a page full of numbers.

**fish is still the ceiling, and it is not close.** Every other
alt-shell is two to three orders of magnitude behind it on both
censuses.

**Retention, not acquisition, is where alt-shells lose.** popcon's vote
column is the share of installations whose files were used recently, and
it separates the category cleanly: bash 89%, zsh 59%, fish 52% — against
nushell 26%, xonsh 28%, elvish 12%. People try new shells and stop. The
unbundled stack does not have that problem: direnv 61%, starship 62%,
atuin 51%, mise 52%. Tools that add to the shell someone already runs
get kept; shells that replace it get abandoned.

**Caveats, so the table is not over-read.** popcon is an opt-in sample,
Debian-only and server-weighted; its absolute counts are not a census of
anything but itself, and only ratios *within* it mean much.
Source-installed tools are invisible to both feeds, which biases the
comparison against exactly the projects that distribute by `git clone` —
ble.sh most of all. Homebrew's analytics count install *events*, not
people.

## What is still missing (#171, honestly)

**The Reddit sentiment sweep is still not run**, and the reason is still
access rather than effort. The churn half is done — 827 comments, and it
is what the #161/#163 work is built on — but the demand half (unmet
wants, assembled-stack fatigue against modular preference, the dotfiles
flywheel, Reddit's AI sentiment against HN's, an independent check on
blocks demand) has never been gathered.

- reddit.com returns 403 to server-side fetchers and to local curl with
  a browser UA.
- `api.pullpush.io` answered once and then said *"This website does not
  provide free scraping resources for agents."* **That is an operator
  policy, not a throttle to pace around** — do not go back to it. Its
  archive ended 2025-05-19 anyway, so the last fifteen months were never
  covered, and that window matters most for AI sentiment.
- The sanctioned route is Reddit's official OAuth API: a **script**-type
  app at reddit.com/prefs/apps, then client credentials against
  `oauth.reddit.com`, ~100 queries/min, free. **This needs credentials a
  human creates** — there are none on this machine, which is where the
  sweep stops.

### What the censuses answered instead, and what they did not

The open question was:

> Are people tired of assembling starship + zoxide + atuin + fzf +
> direnv and wanting one coherent thing — or do they actively prefer the
> modular stack?

The measured half of that is now answered, and it is the half that
should drive decisions: **people assemble the stack and keep it.** Use
rates of 51–62% for direnv, starship, atuin and mise are retention, not
curiosity. Whatever anyone says about assembly fatigue, the behaviour is
sustained adoption.

The unmeasured half is the *feeling*, and it is genuinely unmeasured:
nothing here says whether those people would have preferred one coherent
tool, only that the modular one is not being abandoned. That is what the
OAuth sweep is for, and it is why the question stays open rather than
being quietly declared answered.

**What the numbers do rule out** is the comforting version of the
question — that the stack is a burden people are waiting to shed. If it
were, the tools would show the retention profile alt-shells show, and
they show the opposite.

So the posture is unchanged, and now for a measured reason rather than a
hedge: **inherit the stack rather than replace it** (#159). The answer to
the sentiment question changes the marketing, not the architecture.
