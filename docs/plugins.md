# koi — plugin roadmap

The plugins we intend to write, and the fast/correct rules they must obey.
This is the tier-2 (native gRPC) roadmap; the tier-1 zsh-compat story lives
in [design.md](design.md).

## The dividing rule

**Per-keystroke + pure-local = core. Touches external state = plugin.**

The keystroke path never crosses a process boundary for things that don't
need it. Syntax highlighting, path/file completion, autosuggestions from
local history, alias expansion, last exit code, and command duration are
all core — the host already has the data, and IPC for them would be pure
overhead. Everything that *can* be slow or wrong (git, network, k8s,
cloud, disk scans) lives behind a deadline where it degrades instead of
blocking.

## The compatibility promise to plugin authors (#168)

**A plugin binary built against `plugin/v1` keeps working across koi
releases without a rebuild.** That is a promise, and it is enforced:
`internal/pluginhost/abi_test.go` snapshots every field number, wire
type, name and RPC signature in the v1 package, and a change that would
break a compiled plugin fails CI. Additions pass — that is what
"frozen-additive" means. A rename, a type change, a renumbered field or
a removal needs a `v2` package and a `Handshake.ProtocolVersion` bump,
with v1 plugins still loading.

The promise is only worth making if somebody outside this repository can
compile a plugin to hold us to it, which until #188 nobody could: the
handshake and the dispensers lived in `internal/`. They are now published
as `pkg/pluginsdk/v1`, and `pkg/pluginsdk/v1/abi_test.go` freezes the part
of the ABI that the protos do not describe — the handshake cookie and the
eight service names both ends dispense by.

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

Non-Go plugins are equally supported and always were: the contract is the
protos plus the go-plugin handshake, and `pkg/pluginsdk/v1/abi_test.go`
records the handshake values and service names another language's
implementation has to reproduce.

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

## Prompt segments (`PromptSegmentProvider`)

| Plugin | What | Fast/correct notes |
| --- | --- | --- |
| `koi-git` | branch, dirty state, ahead/behind — gitstatusd-class | **Flagship; build first.** Resident, per-repo cache, fsevents/inotify invalidation (never poll). Cached render <1ms; cold scans happen off-prompt and repaint in place |
| `koi-aws` | ~~planned~~ **landed** (#79, cmd/koi-aws) | Four capabilities on one connection: %p{aws} segment (profile@region + SSO expiry from local config/token metadata — declared env_keys, deny-filtered host-side), --profile/--region completion, per-directory AWS_PROFILE via the #12 trust flow (.aws-profile), aws-whoami (5m TTL) / aws-login commands. Never calls AWS on the prompt path; never reads credential values |
| `koi-k8s` | ~~kubeconfig context/namespace~~ **superseded** | The native p10k engine's kubecontext segment reads the kubeconfig scalar by hand, so the segment half needs no plugin. What remains plugin-worthy is cluster-resource *completion* — see `koi-kubectl` below |
| `koi-runtimes` | ~~asdf/`.tool-versions` pins~~ **superseded** (#77) | Native tool-version switching resolves pins on cd and the pins prompt segment shows install truth, with no subprocess — strictly better than a plugin could do it |

## Whole-prompt themes (`ThemeProvider`, #30)

| Plugin | What | Fast/correct notes |
| --- | --- | --- |
| `koi-starship-native` | starship as a resident gRPC theme — **deferred unless measured** | #45's subprocess flavor already budgets the call and serves the previous prompt stale on a miss, and cmd/koi-p10k is now the ThemeProvider reference (#30, 129µs round trip). Build this only if someone measures the per-prompt spawn actually hurting |
| community themes | any whole-prompt look, any language | `KOI_THEME=<name>` selects by declared theme name; built-in names (plain, p10k, starship) cannot be claimed |

A theme plugin renders the entire prompt set — `prompt`, `cont_prompt`,
`rprompt` — from a `PromptContext` (cwd, exit code, duration, jobs,
user/host/ssh, width, color). It may serve several themes; a miss serves
the previous set or falls back to the built-in p10k-class theme, so a
broken theme costs its look, never the prompt.

## Env providers (`EnvProvider`, #12)

| Plugin | What | Fast/correct notes |
| --- | --- | --- |
| `koi-direnv` | ~~planned~~ **landed** (#137, cmd/koi-direnv) | Delegates `.envrc` evaluation and the whole stdlib (`use nix`, `layout python`, `source_up`) to real direnv; koi owns the cd moment, the approval UX, and apply/revert. Checks `direnv status --json` **before** exporting — export fails identically for blocked, denied, and broken-`.envrc`, and only the status enum tells them apart. `DIRENV_*` bookkeeping is stripped (the host does not send it back, so it has no consumer). direnv reports symlink-resolved paths, so `for_dir` is mapped into the caller's namespace or the host discards every proposal on macOS |
| `koi-dotenv` | ~~plain `.env` file loading~~ **landed** (#475, cmd/koi-dotenv) | Parse only — never execute, never expand; no subprocess anywhere, and the trust prompt shows every value. The second real EnvProvider, proving the trust flow generalizes beyond the tool it was built against. Walks up to the nearest `.env` (skipping `.env` *directories* — a common virtualenv location); the dialect is the rules motdotla/dotenv and docker compose agree on, written down in the file header because `.env` has no spec |

Trust is the contract, enforced host-side: a proposal applies only after
`trust allow` records (plugin, directory, diff-hash); a changed diff
re-pends; deny-listed variables (loader hooks, `IFS`, `KOI_*`) are
stripped before a proposal exists; requests carry allowlisted env only.
Applied diffs revert when the shell leaves the proposal's subtree.

**Two trust models, one gesture** (#137). A plugin wrapping a tool that
has its own approval — direnv's `direnv allow` — implements the additive
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

## Completion providers (`CompletionProvider`)

| Plugin | What | Fast/correct notes |
| --- | --- | --- |
| `koi-carapace` | ~~bridge to carapace's registry~~ **landed** (#9, cmd/koi-carapace) | Shells out to the user's own carapace binary (`export` JSON) guarded by its supported-command list; no carapace installed means empty results, never errors |
| `koi-git-complete` | ~~branches, remotes, modified files~~ **landed** (#476, cmd/koi-git/complete.go) | Second service on koi-git's connection, sharing the repo cache — the multi-capability shape koi-aws proved out. Branches and remotes read natively (loose refs, packed-refs, .git/config; worktree `gitdir:`/`commondir` hops resolved) with no subprocess on the Tab path; only changed-file completion runs git, budget-bounded. Out-of-scope lines get an empty final batch, never a guess — subcommand and flag breadth stays with carapace |
| `koi-kubectl` | cluster resource completion — **demand-gated** | Resident cache with TTL; upstream kubectl completion is slow *because* it's spawn-per-tab, which is the one thing the carapace bridge can't fix. The biggest lift here, so it waits for a user to ask |
| `koi-make` | ~~Makefile/justfile targets~~ **dropped** | carapace already completes make/just targets; a dedicated plugin buys a few milliseconds on completions nobody has complained about |
| `koi-ssh` | ~~hosts from `~/.ssh/config`~~ **dropped** | carapace already completes ssh hosts. If it ever comes back: skip hashed `known_hosts` entries — never un-hash, never guess |

## History backends (`HistoryBackend`)

| Plugin | What | Fast/correct notes |
| --- | --- | --- |
| *(native)* secret scrubbing | gitleaks-style rules in the shell's own store (#10) | Moved shell-side by design: a plugin cannot unwrite the authoritative local file. Matching commands are skipped entirely (ignorespace posture) with a notice; backends only ever receive scrubbed entries |
| *(native)* backend fan-out | async, deadline-bounded Append to every HistoryBackend after a successful local store | Fire-and-forget: the next prompt never waits; `stored=false` governs only a backend's own store |
| `koi-atuin` | ~~bridge to the user's own atuin~~ **landed** (#97, cmd/koi-atuin) | Append mirrors each command (`history start` + `end`; **`--duration` is nanoseconds** while the proto is milliseconds), Search serves ctrl-r from atuin's database. A bridge, not a reimplementation: atuin's opt-in, self-hostable, E2EE posture is the point, and koi reimplementing sync would inherit none of it. Commands travel in `ATUIN_COMMAND_LINE` (`--command-from-env`) so nothing is escaped, and results come back `--print0`-separated because commands contain newlines. No atuin installed = empty results, never errors |
| `koi-sync` | local-first SQLite history, cross-machine sync, frecency + directory-locality ctrl-r ranking | Local file is authoritative; sync is eventual and conflict-free (append-only log). Only worth building if the atuin bridge proves the demand |

## Plugin locality: where does a plugin belong when the shell is remote?

`koi ssh` (#98, docs/ssh.md) copies koi to a remote box and execs it
there. Plugins deliberately do **not** travel in v1 — the deadline-bounded
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

## Needs new proto services (v1 is frozen-additive — new services are fine)

`ShellEvents` (#83) is defined in proto/koi/plugin/v1/events.proto and
allocated `CAPABILITY_EVENTS`, but **the host does not serve it yet** —
contract first, internals after, the same order the rest of this package
was built in. docs/events.md has the design and the three rules it
cannot break: no exec channel ever (proposals only, #111's line), the
host never blocks on a subscriber (bidi stream, bounded buffer,
drop-oldest — a request/response per event would make every `cd` wait on
a plugin), and events carry allowlisted scrub-safe data only (a command
*name*, never its argument list).

| Plugin | New surface | Notes |
| --- | --- | --- |
| command-not-found | one unary RPC | ~~"did you mean"~~ done natively (#42, Damerau suggestions). What's left for a plugin is package suggestions ("install it with brew install …"), inherently off the hot path; demand-gated |
| env provider (direnv-class) | ~~`EnvProvider` service~~ **landed** (#12) | Trust model host-enforced: per-(plugin, dir, diff-hash) allow, deny-listed vars stripped, subtree revert |
| `koi-jump` (zoxide-class) | ~~CommandProvider~~ **landed** (#11); the plugin itself **superseded** (#94) | Native `z` ships in the shell — frecency index bootstrapped from history, zero prompt hooks — so there is nothing left for the plugin to add |
| koi-agent | ~~new service~~ **landed** (#34, `AIProvider.Plan`) | `agent "<task>"` — the provider plans (a spec, never an exec channel); the shell renders it, saves it as an artifact, and executes approved steps through the real exec path, sandbox-wrapped, with destructive steps gating individually (provider flag OR the shell's own parse) and escalation as its own explicit answer. Hooks split to their own issue (ShellEvents) |
| AI assist | ~~invoked RPC~~ **landed** (#20, `AIProvider`) | `??` prefix → Compose (streamed candidates, best-first) lands in the editor buffer wrapped in a visible #21 sandbox invocation — never auto-executes; `explain` builtin → Explain for the last command. Context is scrub-safe by construction (recent commands come from the #10-gated history store; env is the allowlist). Human-scale deadline (90s), Ctrl-C cancels; `koi-claude` (cmd/koi-claude) is the reference provider, driving the user's claude CLI; KOI_AI_PROVIDER selects among several, KOI_AI_SANDBOX tunes the wrap |

## Build order

The original three, all resolved:

1. **`koi-git`** — landed (#8). Hardest latency case (every prompt, every
   repo); proved the deadline/stale/repaint machinery before anything
   depended on it.
2. **`koi-carapace`** — landed (#9). Completion breadth for free.
3. ~~`koi-scrub`~~ — done natively in the shell store (#10), with the
   HistoryBackend fan-out alongside it.

## What's next (reassessed 2026-08)

Every capability the host serves now has a first-party plugin exercising
it, which was the point of building them in-tree (docs/design.md's
no-registry decision: the `cmd/` plugins are living contract tests). And
#169's correction binds here too — a plugin architecture is not why
anyone switches shells — so the bar for a new first-party plugin is:
prove an unproven contract surface, or answer direct demand. Not fill
out this table.

What clears that bar, in order:

1. **`koi-dotenv`** (#475, landed — cmd/koi-dotenv) — the second real EnvProvider. Tiny
   (parse-only, no subprocess), and it proves the #12 trust flow
   generalizes beyond the tool it was built against rather than being
   shaped around direnv. `.env` files are ubiquitous in exactly the
   population koi is courting, and most of them don't run direnv.
2. **git completion as a second service on `koi-git`** (#476, landed —
   cmd/koi-git/complete.go) — the roadmap's koi-git-complete row, in
   the same binary as the segment. Proves the multi-service-one-
   connection shape on the flagship and answers Tab for the
   most-completed CLI at ref-cache latency.
3. **A first-party ShellEvents consumer** — when #83's host wiring
   lands. ShellEvents is the one allocated capability no in-tree plugin
   exercises, which is exactly the state the in-tree plugins exist to
   prevent. Its first consumer should be small and chosen to exercise
   the contract's hard rules (the bidi stream, drop-oldest, the
   no-exec-channel line) — a notify-when-a-long-command-finishes plugin
   is the right shape.

Superseded rather than pending — struck in the tables above so this doc
stops advertising overtaken work: `koi-runtimes` (#77 made tool-version
switching native), `koi-jump` (#94 made `z` native), the `koi-k8s`
prompt segment (the native kubecontext segment), `koi-make` and
`koi-ssh` (carapace covers both), the native half of command-not-found
(#42). Deferred with a condition attached: `koi-starship-native` (only
if the per-prompt spawn is measured to hurt), `koi-kubectl` (waits for
demand), `koi-sync` (gated on the atuin bridge proving demand).
