# packaging and distribution

The install line is the first thing anyone reads, and it is the line
that gets copied into blog posts, dotfiles READMEs and YouTube
descriptions for years. `brew install gish` and `brew tap … && brew
install …` are not the same artifact.

## Homebrew core, before launch (#164)

**Goal:** gish is in `homebrew/homebrew-core` when the announcement
goes out, so the install line in the post is one command.

Two findings shape how:

1. **A third party should submit it.** Homebrew core's notability bar
   is applied more leniently to a formula submitted by someone other
   than the author. Plan for that: the submission is not ours to make.
2. **Before the launch post, not after.** The post's install line is
   the thing being optimized; a formula that lands a week later has
   already missed every copy of it.

Homebrew analytics are the other reason. Per-formula install counts
turned out to be the only per-tool install census that exists for this
category — it is how the mise-vs-asdf ratio in the research was
measured at all. Being listed makes gish's own adoption measurable, and
the alternative is arguing from stars, which measure something else.

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
| Homebrew core | planned before launch | #164 |
| Homebrew tap | shipping | pre-releases and the edge |
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
