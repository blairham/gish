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
   stop working.
4. **A rc that uses a removed knob still starts.** An unknown
   assignment is a variable, and an unknown `config` key is an error on
   one line — never a shell that refuses to open.

## Before 1.0

gish is pre-1.0 today, and this page is deliberately written as though
it were not, because the research finding is specific: **a published
stability contract matters more than the version number.** The surfaces
above were designed with freezing in mind — the prompt escape set
(#109) and the manifest schema (#108) were both settled by asking what
we would be willing to keep forever — and this page writes that habit
down as a promise rather than leaving it as an internal one.

What pre-1.0 actually means here: the list of *covered* surfaces may
still grow, and things not yet on it (a new builtin's flags in its first
release) may move once before they settle. Nothing on the list moves.

## Reporting a break

If an upgrade breaks something on the covered list, that is a bug and it
gets fixed or reverted — open an issue with the rc line and the
versions. The contract is only worth what it costs us to keep.
