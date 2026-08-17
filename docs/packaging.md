# packaging and distribution

The install line is the first thing anyone reads, and it is the line
that gets copied into blog posts, dotfiles READMEs and YouTube
descriptions for years. `brew install gish` and `brew tap … && brew
install …` are not the same artifact.

## Homebrew core (#164)

**The goal was "in core before the announcement, so the install line in
the post is one command". That is not achievable, and the reason is
worth writing down rather than rediscovering.** Homebrew's Package
Acceptance Policy sets a notability bar with numbers in it, and gish
fails all of them by a wide margin today:

| requirement | policy | gish, 2026-08-17 |
| --- | --- | --- |
| repository age | *"A code repository less than 30 days old is normally not eligible."* | **3 days** (created 2026-08-14) |
| notability, third-party submission | at least **30 forks, 30 watchers or 75 stars** | 0 forks, 0 watchers, 1 star |
| notability, **self**-submission by the repo owner | at least **90 forks, 90 watchers or 225 stars** | as above |
| stable release | *"Upstream must identify the packaged version as stable and provide an immutable tag or release."* | no tags yet |

The bar is circular with respect to launch: notability is what a
launch *produces*. There is no ordering in which core precedes the
announcement, so the announcement ships the tap line, and core follows
when the numbers arrive.

Three things follow, and the third is the actionable one:

1. **The launch install line is the tap.** `brew tap blairham/tap &&
   brew install gish`. Every blog post and dotfiles README that copies
   it keeps that extra command for as long as it lives — that cost is
   real and it is now unavoidable, so do not plan around avoiding it.
2. **A third party must submit it, and it is worth 3×.** The policy is
   explicit that self-submission by the repository owner is held to
   triple the thresholds: 225 stars instead of 75. Finding someone else
   to open the PR is not etiquette, it is the difference between a
   reachable bar and one three times further away. Line that person up
   before the numbers land, not after.
3. **75 stars is the trigger to watch.** It is the cheapest of the
   three third-party thresholds and the one a launch actually moves.
   The formula and this page exist so that submission is a same-day
   action when it trips, not a week of reconstruction.

Homebrew analytics are the other reason to care. Per-formula install
counts turned out to be the only per-tool install census that exists
for this category — it is how the fzf-vs-fish-vs-nushell numbers in
docs/adoption.md were measured at all. Being listed makes gish's own
adoption measurable, and the alternative is arguing from stars, which
measure something else and which we would then be optimizing directly.

**Current state.** GoReleaser publishes to `blairham/homebrew-tap`
(`brews:` in `.goreleaser.yaml`), which stays as the pre-core path and
for pre-releases. The tap and core are not in conflict: core carries
the release, the tap carries the edge.

**What core needs that the tap does not:**

- A stable release tarball URL and checksum — GoReleaser already emits
  both.
- A `test do` block that actually exercises the binary. `gish -c 'echo
  ok'` is the honest minimum; the tap formula's test is the same shape.
- No `caveats` that instruct people to run `chsh`. The tap formula's
  caveats explain how to make gish a login shell; core reviewers
  dislike caveats generally, and "never require chsh" is our own rule
  anyway — the caveat should read as optional, because it is.
- License, homepage, description — all present.

`packaging/homebrew/gish.rb` is the core-shaped formula, kept in the
repo so the third-party submitter has something to copy rather than
reconstruct.

## Everything else, in the order it matters

| channel | state | notes |
| --- | --- | --- |
| Homebrew tap | shipping | **the launch install line**, and the edge afterwards |
| Homebrew core | blocked on notability | #164 — submit at 75 stars, via a third party |
| winget / scoop | configured (#89) | ships with the next tag; both skip when their token is absent |
| nixpkgs | after launch | a Go module package; needs a vendorHash |
| AUR | after launch | `gish-bin` from the release tarball first |
| Debian/Ubuntu | later | needs a stable release cadence first |

The ordering is deliberate: each of these is a place a *user* looks,
and there is no point being in five package managers with nothing to
install. Launch, then breadth.

## A deprecation to handle before it becomes an error

GoReleaser is phasing `brews:` out in favor of `homebrew_casks:`, and
says so on every run. The migration is not a rename — casks install
differently — so it wants a release to verify against rather than a
blind edit, and it is deliberately not bundled with the #89 work. The
tap is the pre-core path anyway; core carries a hand-written formula
(`packaging/homebrew/gish.rb`), which this does not affect.

## What is never in a package

- Anything that modifies the user's login shell. No `chsh`, no
  `/etc/shells` edit at install time. gish is run in a tab first, and
  the packaging must not pre-empt that decision.
- Plugin binaries activated by default. `gish-git` and `gish-carapace`
  ship in the package; linking them into the plugin directory is one
  documented command, because "installed" and "running in your shell"
  should be different states.
