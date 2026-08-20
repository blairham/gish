# A team's koi setup, in the repo

What shell you use is partly a group choice, and every koi adopter
today converts alone. A `.koi.toml` checked into a project repo turns
each adopter into a distribution channel: a teammate clones, runs one
command, and has the team's setup — layered under their own, never
replacing it.

```sh
koi adopt            # preview, confirm, apply
koi adopt --status   # what is adopted here, and whether it has changed
koi adopt --revert   # undo exactly what adopt did
```

## The file

Two sections, both formats koi already speaks — a reader already knows
how to read them, and `adopt` is a loop over two existing writers
rather than a new config language:

```toml
[settings]              # names and values from `config`'s vocabulary
theme = "p10k"
editmode = "vi"

[[plugins]]             # plugins.toml entries, verbatim
source = "zsh-users/zsh-autosuggestions"
pin = "v0.7.1"
```

Settings are validated against `config`'s closed vocabulary before
anything applies — all of it or none of it. They are data, not shell
code: reviewable at a glance, and impossible to weaponize beyond "your
prompt looks different". Plugin entries ride the manifest's own pin
machinery; an unpinned entry warns, because the team's config would
otherwise drift per machine, which is what pins exist to stop.

**Aliases and functions deliberately do not travel.** They are
arbitrary shell code that silently shadows commands the user types a
hundred times a day — the tier where "one command" and "you reviewed
this" genuinely conflict. That exclusion is a decision, not a gap.

## How applying works

Adoption is an explicit act, not an ambient one — it applies until
reverted, like an editor distro kit, not per-directory like direnv (koi
has the per-directory shape where it belongs, in the env-provider trust
flow).

- `koi adopt` previews everything and asks; `--yes` skips the question
  for scripted setup.
- Settings land in a fragment under `$XDG_CONFIG_HOME/koi/adopted.d/`,
  sourced **before** your rc — last write wins, so anything you set
  yourself beats the repo, with no merge policy to reason about.
  `cat` the fragment to see exactly what was adopted.
- Plugin entries the manifest already has stay yours — adopt never
  repins or replaces an entry you chose.
- An entry adoption added and you later edited is yours too: revert
  removes only entries that still match what it added.

## Staleness

`git pull` never re-applies anything. The adoption ledger keeps the
file's hash, and `doctor` says when the checked-in config has changed
since you adopted it — review it, then `koi adopt` to re-apply or
`--revert` to undo. koi does not nag about a repo config you chose not
to adopt, and it does not host, index, or vouch for anyone's shared
config: it makes applying *your team's* trivial.
