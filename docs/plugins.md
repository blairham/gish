# koi — plugins

The tier-2 (native gRPC) plugin system: the plugins that ship with koi,
the fast/correct rules every plugin must obey, and what is planned. The
tier-1 zsh-compat story lives in [design.md](design.md).

## The dividing rule

**Per-keystroke + pure-local = core. Touches external state = plugin.**

The keystroke path never crosses a process boundary for things that don't
need it. Syntax highlighting, path/file completion, autosuggestions from
local history, alias expansion, last exit code, and command duration are
all core — the host already has the data, and IPC for them would be pure
overhead. Everything that *can* be slow or wrong (git, network, k8s,
cloud, disk scans) lives behind a deadline where it degrades instead of
blocking.

## The compatibility promise to plugin authors

**A plugin binary built against `plugin/v1` keeps working across koi
releases without a rebuild.** That is a promise, and it is enforced:
`internal/pluginhost/abi_test.go` snapshots every field number, wire
type, name and RPC signature in the v1 package, and a change that would
break a compiled plugin fails CI. Additions pass — that is what
"frozen-additive" means. A rename, a type change, a renumbered field or
a removal needs a `v2` package and a `Handshake.ProtocolVersion` bump,
with v1 plugins still loading.

The promise is only worth making if somebody outside this repository can
compile a plugin to hold us to it, so the handshake and the dispensers
are published as `pkg/pluginsdk/v1`, and `pkg/pluginsdk/v1/abi_test.go`
freezes the part of the ABI that the protos do not describe — the
handshake cookie and the eight service names both ends dispense by.

Both published paths carry the contract version the way the proto package
does (`koi.plugin.v1` → `pkg/pluginapi/v1`, `pkg/pluginsdk/v1`), so a
future v2 lands beside v1 instead of renaming it out from under installed
plugins.

This exists because it is the documented reason nushell's plugin
ecosystem never formed, and the reason is not the wire format:

> "the plugin interface requires strict version matching… which sort of
> kills the idea that I'm going to distribute these"

Their top plugin has 85 stars, and they have out-of-process plugins in
any language — the same architecture as this one. Distribution is what
failed. A plugin author needs to know that publishing a binary is not
signing up for a rebuild every release.

**And a plugin can never block a keystroke.** Every outbound call
carries a deadline (below); a plugin that hangs costs its own segment
and nothing else. nushell's plugin protocol has no timeout enforcement
at all, and nobody advertises this guarantee because almost nobody can:
an add-on that hooks zsh's ZLE runs *inside* the shell's process, where
there is no boundary to enforce. A test hangs a plugin deliberately and
measures what it costs, because a guarantee that is not tested is a
claim.

## Writing a plugin

A plugin is an ordinary executable. Implement the services you want from
`pkg/pluginapi/v1`, hand them to `pluginsdk.Serve`, and drop the binary in
`$XDG_DATA_HOME/koi/plugins` — there is no registration step, no manifest
the host must be told about, and nothing to rebuild when koi updates.

```go
package main

import (
	"context"

	pluginapi "github.com/blairham/koi-shell/pkg/pluginapi/v1"
	pluginsdk "github.com/blairham/koi-shell/pkg/pluginsdk/v1"
)

type info struct {
	pluginapi.UnimplementedPluginInfoServer
	caps []pluginapi.Capability
}

func (i info) Describe(context.Context, *pluginapi.DescribeRequest) (*pluginapi.DescribeResponse, error) {
	return &pluginapi.DescribeResponse{
		Name:         "koi-example",
		Version:      "0.1.0",
		Capabilities: i.caps,
	}, nil
}

type prompt struct {
	pluginapi.UnimplementedPromptSegmentProviderServer
}

func (prompt) Segments(context.Context, *pluginapi.SegmentsRequest) (*pluginapi.SegmentsResponse, error) {
	return &pluginapi.SegmentsResponse{
		Segments: []*pluginapi.SegmentDescriptor{{Id: "example", BudgetMs: 20}},
	}, nil
}

func (prompt) Render(_ context.Context, req *pluginapi.RenderRequest) (*pluginapi.RenderResponse, error) {
	if req.GetSegmentId() != "example" {
		return &pluginapi.RenderResponse{}, nil
	}
	return &pluginapi.RenderResponse{Text: "hello", TtlMs: 100}, nil
}

func main() {
	p := pluginsdk.Plugin{Prompt: prompt{}}
	p.Info = info{caps: pluginsdk.Capabilities(p)}
	pluginsdk.Serve(p)
}
```

Then `%p{example}` in a prompt renders it.

Three things that shape are doing for you, each of which was a real bug
before it existed:

- **A nil field is not a registered service.** Declaring a capability you
  have not implemented leaves a door the host can open onto a nil pointer.
  The absent struct field is the whole declaration, so that state cannot be
  spelled.
- **`Capabilities(p)` is derived, not hand-kept.** The host gates every
  dispatch on `Describe`, so a service you implement but forget to announce
  is silently dead — never called, no error, nothing in the logs. Reading
  the list off the same struct that registers the services is what keeps
  the two from drifting.
- **Your `main` is the thing under test.** Every plugin in `cmd/` builds
  its `pluginsdk.Plugin` in a `newPlugin()` the tests call too, so
  "claims theme" and "serves theme" are the same assertion.

Non-Go plugins are equally supported: the contract is the protos plus
the go-plugin handshake, and `pkg/pluginsdk/v1/abi_test.go` records the
handshake values and service names another language's implementation
has to reproduce.

## Latency budgets

| Interaction | Budget | On miss |
| --- | --- | --- |
| Prompt segment render | 50ms default (`SegmentDescriptor.budget_ms`) | render previous (stale) value or nothing; repaint in place when the response lands |
| Whole-prompt theme render | 50ms default (`ThemeDescriptor.budget_ms`) | serve the theme's previous prompt set, else fall back to the built-in theme |
| Env diff on cwd change | 100ms | skip for this prompt; ask again on the next directory change |
| Completion request | ~80ms to first batch | show whatever batches arrived; stream stays open until the user types again |
| History append | none — fire-and-forget | shell never waits; backend scrubs/stores on its own time |
| History search (ctrl-r) | ~100ms to first batch (`DefaultHistorySearchBudget`) | local history renders alone; backend rows are additive, so a miss costs reach, not the picker |
| Command-not-found | human-scale (command already failed) | skip suggestion |

Two invariants sit under all of these:

- **Stale responses are dropped by sequence, not by luck.** Requests carry
  `event_seq`; a slow answer for the *previous* directory or buffer must
  never overwrite the current one.
- **Plugins return raw values; the host owns quoting and escaping.** A
  completion candidate is data, never buffer text. No plugin can inject
  shell metacharacters — this closes the "completion became code
  execution" class of bugs and keeps quoting identical across plugins.

## The first-party plugins

Every capability the host serves has a first-party plugin exercising it.
That is deliberate: the `cmd/` plugins are living contract tests — a
change that would break a compiled plugin breaks one of these at PR time
— and they are the worked examples a plugin author copies from (see the
no-registry decision in [design.md](design.md)).

### `koi-git` — prompt segment + completion (flagship)

A gitstatusd-class prompt segment: resident per-repo cache, fsnotify
invalidation on the `.git` directory (never polls), background refreshes
— a render answers from cache in microseconds, well inside the 50ms
budget, and cold scans happen off-prompt and repaint in place. Consumed
via the `%p{git}` prompt escape.

Completion rides the same connection as a second service, sharing the
repo cache — branches and remotes are read natively (loose refs,
packed-refs, `.git/config`; a linked worktree resolves through its
`gitdir:` and `commondir` hops) with no subprocess on the Tab path.
Only changed-file completion runs git, bounded by the completion budget.
Scope is arguments only — branch-taking subcommands, remotes-then-
branches for push/pull/fetch, changed files for add/restore — while
subcommand and flag breadth stays with carapace; anything out of scope
answers an empty final batch, never a guess.

### `koi-carapace` — completion breadth

Bridges the carapace completion registry (~1,000 CLIs) by shelling out
to the user's own carapace binary (`export` JSON), guarded by its
supported-command list. No carapace installed means empty results,
never errors.

### `koi-aws` — a many-capability vendor plugin

Four capabilities on one connection: the `%p{aws}` segment
(profile@region + SSO token expiry, from local config and token
*metadata* only — declared `env_keys`, deny-filtered host-side),
`--profile`/`--region` completion from the user's own config,
per-directory `AWS_PROFILE`/`AWS_REGION` proposals from a walk-up
`.aws-profile` file through the trust flow, and `aws-whoami` /
`aws-login` commands. Never calls AWS on the prompt path; never reads
credential values.

### `koi-direnv` — real direnv behind the env contract

Delegates `.envrc` evaluation and the whole stdlib (`use nix`,
`layout python`, `source_up`) to the user's real direnv; koi owns the
cd moment (no `direnv hook`), the approval UX, and apply/revert. It
checks `direnv status --json` **before** exporting — export fails
identically for blocked, denied, and broken-`.envrc`, and only the
status enum tells them apart. `DIRENV_*` bookkeeping is stripped (the
host does not send it back, so it has no consumer), and direnv's
symlink-resolved paths are mapped back into the caller's namespace so
proposals survive on macOS.

### `koi-dotenv` — plain `.env` loading

Parse only — never execute, never expand; no subprocess anywhere, and
the trust prompt shows every value. It walks up to the nearest `.env`
(skipping `.env` *directories*, a common virtualenv location); the
dialect is the rules motdotla/dotenv and docker compose agree on,
written down in the source because `.env` has no spec. `$VAR` stays the
literal string — anyone wanting interpolation or the direnv stdlib's
`dotenv` helper uses koi-direnv.

### `koi-atuin` — history sync bridge

A bridge to the user's own atuin, not a reimplementation: atuin's
opt-in, self-hostable, E2EE posture is the point, and reimplementing
sync would inherit none of it. Append mirrors each command
(`history start` + `end`; atuin's `--duration` is nanoseconds while the
proto is milliseconds), and Search serves ctrl-r from atuin's database.
Commands travel in `ATUIN_COMMAND_LINE` (`--command-from-env`) so
nothing is escaped, and results come back `--print0`-separated because
shell history is exactly the corpus full of newlines and quotes. No
atuin installed = empty results, never errors.

The shell-side halves live in core by design: secret scrubbing runs in
the shell's own store (a plugin cannot unwrite the authoritative local
file — matching commands are skipped entirely, with a notice, and
backends only ever receive scrubbed entries), and the backend fan-out is
async and deadline-bounded, fired after a successful local store. The
next prompt never waits.

### `koi-claude` — the reference AI provider

Drives the user's claude CLI behind the `AIProvider` contract: the `??`
prefix streams command candidates into the editor buffer (wrapped in a
visible sandbox invocation, never auto-executed), and the `explain`
builtin answers why-did-that-fail. Context is scrub-safe by
construction — recent commands come from the history store, which never
records secret-bearing commands, and env is the allowlist.
`KOI_AI_PROVIDER` selects among providers.

### `koi-p10k` — the prompt engine as a theme provider

Serves the native powerlevel10k-class engine over the `ThemeProvider`
contract as `p10k-<preset>` themes. The shell itself renders the same
engine in-process — its own prompt should not pay a round trip — so
this plugin exists as the reference `ThemeProvider` implementation, and
its round trip is measured in docs/prompt.md.

### `koi-acp` — the inbound agent bridge

The deletable half of the ACP integration (docs/acp.md): `koi acp`
lives in core because a plugin may never hold an exec channel; this
plugin is the inbound edge.

## Whole-prompt themes (`ThemeProvider`)

A theme plugin renders the entire prompt set — `prompt`, `cont_prompt`,
`rprompt` — from a `PromptContext` (cwd, exit code, duration, jobs,
user/host/ssh, width, color). It may serve several themes;
`KOI_THEME=<name>` selects by declared theme name, and built-in names
(plain, p10k, starship) cannot be claimed. A miss serves the previous
set or falls back to the built-in p10k-class theme, so a broken theme
costs its look, never the prompt.

## Env providers (`EnvProvider`)

Trust is the contract, enforced host-side: a proposal applies only after
`trust allow` records (plugin, directory, diff-hash); a changed diff
re-pends; deny-listed variables (loader hooks, `IFS`, `KOI_*`) are
stripped before a proposal exists; requests carry allowlisted env only.
Applied diffs revert when the shell leaves the proposal's subtree.

**Two trust models, one gesture.** A plugin wrapping a tool that has
its own approval — direnv's `direnv allow` — implements the additive
`EnvProvider.Allow` RPC. `trust allow` calls it before applying, so
nobody is asked twice for one action, and koi keeps its UI and its
record while the wrapped tool stays authoritative about what it will
evaluate. One gesture is only safe because both sides key on *content*:
editing the `.envrc` re-blocks direnv and changes koi's diff hash, so
the two re-prompt together instead of drifting. That was tested against
real direnv, not assumed. A plugin with no second trust model returns
unimplemented and nothing changes; a plugin that cannot record the
approval does not block it — koi's record is authoritative for koi —
but the user is told, since the next shell may re-prompt.

## Plugin locality: where does a plugin belong when the shell is remote?

`koi ssh` (docs/ssh.md) copies koi to a remote box and execs it there.
Plugins deliberately do **not** travel in v1 — the deadline-bounded
degradation already makes their absence safe, and a plugin is not a
single static file the way the shell is. But it raises a question the
contract has to answer before third-party plugins exist, because
retrofitting a default onto plugins already in the wild means guessing
wrong for half of them.

Every plugin is one of two kinds, and the distinction is not about speed:

- **Remote-local** — it describes the machine the shell is running on.
  `koi-git` reads the repo you are standing in; a version-manager
  segment reads that box's install tree. Running these anywhere else
  produces a confidently wrong answer, which is worse than no answer.
- **Identity-local** — it describes *you*, not the box. `koi-aws` reads
  your SSO tokens, a history backend holds your command record, a
  credential-backed completion source holds your secrets. The right place
  for these is the machine you sat down at, reached over a
  reverse-forwarded socket — which is also the strong security story:
  those tokens and that history never land on the server.

v1 ships neither: no plugins on the remote at all. When the reverse
socket lands, the kind becomes a declared field on `DescribeResponse`
(additive, so no version bump), and the host routes accordingly. Until
then, plugin authors should assume a remote session has no plugins and
make sure their segment's absence reads as "not shown" rather than
"broken".

## ShellEvents: designed, not yet served

`ShellEvents` is defined in proto/koi/plugin/v1/events.proto and
allocated `CAPABILITY_EVENTS`, but **the host does not serve it yet** —
contract first, internals after, the same order the rest of this package
was built in. docs/events.md has the design and the three rules it
cannot break: no exec channel ever (proposals only), the host never
blocks on a subscriber (bidi stream, bounded buffer, drop-oldest — a
request/response per event would make every `cd` wait on a plugin), and
events carry allowlisted scrub-safe data only (a command *name*, never
its argument list).

## Planned

The bar for a new first-party plugin: prove an unproven part of the
contract, or answer direct demand — a plugin architecture is not why
anyone switches shells, so the list stays short on purpose.

- **A first-party ShellEvents consumer** — lands with the host wiring
  above. ShellEvents would otherwise be the one capability no in-tree
  plugin exercises, which is exactly the state the in-tree plugins
  exist to prevent. Its first consumer should be small and chosen to
  exercise the contract's hard rules (the bidi stream, drop-oldest, the
  no-exec-channel line) — a notify-when-a-long-command-finishes plugin
  is the right shape.
- **`koi-kubectl`** — cluster resource completion from a resident cache
  with a TTL. Upstream kubectl completion is slow *because* it is
  spawn-per-tab, which is the one thing the carapace bridge cannot fix.
  The biggest lift on this list, so it waits for a user to ask.
- **`koi-starship-native`** — starship as a resident gRPC theme. The
  built-in starship theme already budgets its per-prompt subprocess and
  serves the previous prompt stale on a miss, so this is only worth
  building if the spawn is measured to actually hurt.
- **`koi-sync`** — local-first SQLite history with cross-machine sync
  and frecency + directory-locality ctrl-r ranking. The local file
  stays authoritative; sync would be eventual and conflict-free
  (append-only log). Only worth building if the atuin bridge proves the
  demand.
- **Package suggestions on command-not-found** — "install it with
  `brew install …`", one unary RPC, inherently off the hot path. The
  did-you-mean half is already native (Damerau suggestions in core).

## Deliberately not plugins

Things that look like plugin material and aren't, so they are not
re-proposed:

- **Tool-version switching and the pins segment** — native. Pins only
  select among installed versions, resolution is file reads with no
  subprocess on cd, and a plugin could only add latency.
- **Directory jumping (`z`)** — native. The shell is the tracking
  point: the loop notes directory changes with zero prompt hooks, and a
  fresh index bootstraps from the history store's recorded cwds.
- **The kubecontext prompt segment** — native. The prompt engine reads
  the kubeconfig's current-context scalar by hand; only cluster
  *completion* (above) still warrants a plugin.
- **Makefile/justfile targets and ssh-host completion** — carapace
  covers both. If ssh-host completion ever returns as a plugin: skip
  hashed `known_hosts` entries — never un-hash, never guess.
- **Secret scrubbing** — shell-side, structurally: a plugin cannot
  unwrite the authoritative local history file.
