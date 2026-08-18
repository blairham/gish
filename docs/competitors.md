# The competitive picture, re-measured

The August 2026 sweep lost two of six research streams and asked to be
re-run before "nobody owns the quadrant" was treated as settled (#171).
This is that re-run for the *measurable* half — repository facts, taken
from the GitHub API on **2026-08-17**, not from memory or from the
projects' own copy.

The other half — the Reddit demand sweep — is **not done**, and the
reason is recorded at the bottom rather than papered over.

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

### ble.sh — the unmeasurable one

| | |
| --- | --- |
| stars | 4,618 |
| language | Shell (pure bash) |
| last push | 2026-08-11 |

Fish-grade highlighting and autosuggestions **in pure bash**, and
**absent from Homebrew core** — so its install base cannot be measured
the way everything else in this category can. Its star count may reflect
distribution friction rather than absent demand for exactly our thesis.

That absence is itself the lesson behind #164: not being in core makes a
project invisible to the only per-tool install census that exists.

## For scale

| project | stars | note |
| --- | --- | --- |
| nushell | 40,271 | more stars than fish, ~1/4 the installs, 27% daily-use ratio on Debian popcon vs fish's 52% |
| fish | 34,020 | the realistic ceiling, reached over ~10 years |

## What is still missing (#171, honestly)

**The Reddit demand sweep was not run.** The churn half is done — 827
comments, and it is what the #161/#163 work is built on — but the demand
half (unmet wants, assembled-stack fatigue vs modular preference, the
dotfiles flywheel, Reddit's AI sentiment against HN's, an independent
check on blocks demand) has never been gathered.

It is not gathered here either, and the reason is access rather than
effort:

- reddit.com returns 403 to server-side fetchers and to local curl with
  a browser UA.
- `api.pullpush.io` answered once and then said *"This website does not
  provide free scraping resources for agents."* **That is an operator
  policy, not a throttle to pace around** — do not go back to it. Its
  archive ended 2025-05-19 in any case, so the last fifteen months were
  never covered, and that window matters most for AI sentiment.
- The sanctioned route is Reddit's official OAuth API: a **script**-type
  app at reddit.com/prefs/apps, then client credentials against
  `oauth.reddit.com`, ~100 queries/min, free. That needs credentials a
  human creates.

So the single most valuable open question stays open:

> Are people tired of assembling starship + zoxide + atuin + fzf +
> direnv and wanting one coherent thing — or do they actively prefer the
> modular stack?

Until it is answered, the posture that survives either answer is the one
the code already takes: **inherit the stack rather than replace it.**
That is what #159 built, and it is why the answer changes the marketing
rather than the architecture.
