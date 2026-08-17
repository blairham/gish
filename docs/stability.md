# stability contract

**Your config will not break.** This page says exactly what that
promise covers, what it does not, and what happens when something has
to change anyway.

It exists because the alternative is measurable. nushell is still 0.x
after roughly seven years, at least ten of its last fourteen releases
carried breaking changes, and its own maintainers opened *"Should we
plan on 1.0 at all?"*. The cost lands in month three, not during
evaluation:

> "There are the constant breaking changes… every few months I need to
> dig into my nushell setup scripts to unbreak half my shell tools
> because some syntax changed. Yeah, no thanks." — HN, 2023-08-31

> "having tried things like nushell i came back to zsh after i realized
> that is too much of an investment for something that can disappear in
> 5 minutes" — HN, 2023-11-03

A shell is something people build on for years. What follows is the
contract that makes that a reasonable thing to do.

## Covered

These surfaces are **frozen**: they keep working, with their current
meaning, for the life of the 1.x line.

| surface | what is frozen |
| --- | --- |
| **rc file** | it is bash syntax, run by the same interpreter that runs your scripts. The file locations (`$GISH_RC`, `$XDG_CONFIG_HOME/gish/gishrc`, `~/.gishrc`) and their precedence do not change. |
| **`GISH_*` variables** | every documented variable keeps its name and its accepted values. New values may be added; existing ones keep meaning what they meant. |
| **`config` keys** | every key `config` accepts, including the dotted theme keys. |
| **`plugins.toml`** | the manifest schema: field names, types, and the meaning of `source`, `kind`, `pin`, `lazy`, `enabled`. |
| **prompt escapes** | the escape set is frozen and zsh-spelled: `%n`/`%u`, `%m`/`%h`, `%~`/`%w`, `%W`, `%d`, `%?`, `%#`, `%p{id}`, `%%`. Unknown escapes pass through, which is what makes adding one safe. |
| **theme knobs** | `GISH_THEME`, the `GISH_THEME_*` family, and the `POWERLEVEL9K_*` names the p10k engine reads. |
| **bash's own surface** | `PROMPT_COMMAND`, `PS0`, `PS1`, `trap … DEBUG`, `bind -x`, `complete`/`compgen`, `command_not_found_handle`. These are not gish's to redefine — they are bash's, and the point of implementing them is that the scripts you already have keep working. |
| **the plugin contract** | `proto/gish/plugin/v1` is frozen-additive: new fields and new RPCs are fine; renames, type changes and removals are not. A breaking change means a `v2` package and a `Handshake.ProtocolVersion` bump, with v1 plugins still loading. |

## Not covered, stated plainly

- **The `agent` builtin** is frozen and experimental (#111). It is
  unadvertised, gets no further investment, and may be removed. `??`
  and `explain` are the advertised AI surface.
- **Exact output text** of `doctor`, `plugins`, `blocks`, `sessions`,
  `tool` and friends. Parse these at your own risk; they are for
  people. Where a machine-readable form is wanted, ask for it and it
  will be added as a flag rather than by changing the human one.
- **Internal Go packages.** `internal/` is internal; the exported
  surface for plugin authors is `pkg/pluginapi/v1` (the generated
  contract) and `pkg/pluginsdk/v1` (the handshake and `Serve`). Both
  paths carry the contract's version the way the proto package does, so
  a `v2` arrives beside them rather than by renaming them.
- **Which theme the naked default renders.** It will stay the stock
  bash/zsh *shape*, but the exact characters are not a contract.
- **Performance numbers.** They are measured and published
  ([bench.md](bench.md)), not promised.
- **Behavior that is a bug.** If gish does something that contradicts
  its own documentation, fixing it is not a breaking change.

## How a change happens when one has to

1. **Deprecate, don't remove.** A setting that is being retired keeps
   working and starts warning — once per session, at startup, naming
   the replacement. `zi` is the worked example: it still works, behind
   a one-time notice, with `plugin` as its documented replacement.
2. **Two minor releases minimum** between the first warning and any
   removal, and never inside a patch release.
3. **Removal only at a major.** Inside 1.x, a documented knob does not
   stop working — and since these rules bind during 0.x too (see
   below), "a major" means 1.0 at the earliest. Nothing on the covered
   list is removed between here and there.
4. **A rc that uses a removed knob still starts.** An unknown
   assignment is a variable, and an unknown `config` key is an error on
   one line — never a shell that refuses to open.

## The version line, and the road to 1.0

gish is pre-1.0 today, and this page is deliberately written as though
it were not, because the research finding is specific: **a published
stability contract matters more than the version number.** The surfaces
above were designed with freezing in mind — the prompt escape set
(#109) and the manifest schema (#108) were both settled by asking what
we would be willing to keep forever — and this page writes that habit
down as a promise rather than leaving it as an internal one.

The version path is decided (#213):

> **`v0.0.0` is the first tag. A short 0.x line runs through the public
> announcement. 1.0 follows it, gated by the closed list below.**

`v0.0.0` exists to prove the release pipeline — GoReleaser, the tap,
the checksums — before anyone is watching. The 0.x line exists because
a frozen set that no outsider has ever configured is a guess: the
surfaces above were designed to be frozen, but they have only met the
person who designed them. Freezing them at the moment of first contact
would mean discovering the mistake afterwards, when the only remaining
options are to break the contract or to carry the mistake to 2.0.

What 0.x here is **not** is nushell's seven years. The lifecycle
research (docs/adoption.md) is unambiguous that the perpetual pre-1.0
treadmill is the largest single adoption killer measured — but the
thing doing the damage is not the digit, it is an *unbounded* 0.x with
no stated endpoint. So two commitments bound this one:

**1. The contract above is already in force.** 0.x is not a licence to
break the covered table. Everything in *How a change happens* applies
today — deprecate rather than remove, two minor releases of warning,
and an rc using a retired knob still opens a shell. If something on the
covered list breaks during 0.x, that is the same bug it would be at
1.4.

**2. The gate is closed.** The list below is complete, and nothing may
be added to it. If work during 0.x turns up another thing that would be
nice to have first, it ships in 1.1 — the one thing that must never
happen is the gate growing faster than it is met, which is exactly the
shape of nushell's *"Should we plan on 1.0 at all?"*.

### The 1.0 gate

1. **Every user-configurable surface is classified** — on the covered
   table above, or explicitly in *Not covered*. Unclassified today and
   in scope for this item: the `config` keys added since the table was
   written (`blocks`, `jump`, `tools`, `highlight`, `editmode`,
   `ssh.bring`), the vi-mode keymap (#163), and `gish ssh`'s per-host
   policy state.
2. **The bash-suite scoreboard is published** (#211). Freezing a
   compatibility claim without knowing the incumbent's own denominator
   is a promise made blind.
3. **The deprecation machinery has survived one real upgrade.** `zi` →
   `plugin` is the worked example, and it needs to be observed working
   across an actual released version bump rather than merely existing
   in the code.
4. **The config-dialect decisions are settled** — #185 (convert other
   prompt dialects, or bridge) and #134, both of which can still move a
   covered surface.
5. **Windows is named as out of scope for the freeze.** Per #110 the
   native interactive port is sequenced to 1.x; the contract covers
   macOS and Linux, and the Windows interactive surfaces are listed as
   not-yet-covered rather than silently included.

Progress against this list is stated in each 0.x release's notes, with
the count of remaining items. That number only goes down. A 0.x line
that reaches double-digit minors with items still open is itself the
failure signal the research describes, and should be treated as one.

## Reporting a break

If an upgrade breaks something on the covered list, that is a bug and it
gets fixed or reverted — open an issue with the rc line and the
versions. The contract is only worth what it costs us to keep.
