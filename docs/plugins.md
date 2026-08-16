# gish — plugin roadmap

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
| `gish-git` | branch, dirty state, ahead/behind — gitstatusd-class | **Flagship; build first.** Resident, per-repo cache, fsevents/inotify invalidation (never poll). Cached render <1ms; cold scans happen off-prompt and repaint in place |
| `gish-aws` | ~~planned~~ **landed** (#79, cmd/gish-aws) | Four capabilities on one connection: %p{aws} segment (profile@region + SSO expiry from local config/token metadata — declared env_keys, deny-filtered host-side), --profile/--region completion, per-directory AWS_PROFILE via the #12 trust flow (.aws-profile), aws-whoami (5m TTL) / aws-login commands. Never calls AWS on the prompt path; never reads credential values |
| `gish-k8s` | kubeconfig context/namespace | File-watch invalidated; never talks to the cluster for a prompt |
| `gish-runtimes` | asdf/`.tool-versions` pins when they differ from global | One small file read, cached by cwd |

## Whole-prompt themes (`ThemeProvider`, #30)

| Plugin | What | Fast/correct notes |
| --- | --- | --- |
| `gish-starship-native` | starship as a resident gRPC theme | The reference external implementation: today's subprocess flavor (#45) spawns per prompt; the plugin flavor keeps it resident |
| community themes | any whole-prompt look, any language | `GISH_THEME=<name>` selects by declared theme name; built-in names (plain, p10k, starship) cannot be claimed |

A theme plugin renders the entire prompt set — `prompt`, `cont_prompt`,
`rprompt` — from a `PromptContext` (cwd, exit code, duration, jobs,
user/host/ssh, width, color). It may serve several themes; a miss serves
the previous set or falls back to the built-in p10k-class theme, so a
broken theme costs its look, never the prompt.

## Env providers (`EnvProvider`, #12)

| Plugin | What | Fast/correct notes |
| --- | --- | --- |
| `gish-direnv` | ~~planned~~ **landed** (#137, cmd/gish-direnv) | Delegates `.envrc` evaluation and the whole stdlib (`use nix`, `layout python`, `source_up`) to real direnv; gish owns the cd moment, the approval UX, and apply/revert. Checks `direnv status --json` **before** exporting — export fails identically for blocked, denied, and broken-`.envrc`, and only the status enum tells them apart. `DIRENV_*` bookkeeping is stripped (the host does not send it back, so it has no consumer). direnv reports symlink-resolved paths, so `for_dir` is mapped into the caller's namespace or the host discards every proposal on macOS |
| `gish-dotenv` | plain `.env` file loading | Parse only — never execute; the trust prompt shows every value |

Trust is the contract, enforced host-side: a proposal applies only after
`trust allow` records (plugin, directory, diff-hash); a changed diff
re-pends; deny-listed variables (loader hooks, `IFS`, `GISH_*`) are
stripped before a proposal exists; requests carry allowlisted env only.
Applied diffs revert when the shell leaves the proposal's subtree.

**Two trust models, one gesture** (#137). A plugin wrapping a tool that
has its own approval — direnv's `direnv allow` — implements the additive
`EnvProvider.Allow` RPC. `trust allow` calls it before applying, so
nobody is asked twice for one action, and gish keeps its UI and its
record while the wrapped tool stays authoritative about what it will
evaluate. One gesture is only safe because both sides key on *content*:
editing the `.envrc` re-blocks direnv and changes gish's diff hash, so
the two re-prompt together instead of drifting. That was tested against
real direnv, not assumed. A plugin with no second trust model returns
unimplemented and nothing changes; a plugin that cannot record the
approval does not block it — gish's record is authoritative for gish —
but the user is told, since the next shell may re-prompt.

## Completion providers (`CompletionProvider`)

| Plugin | What | Fast/correct notes |
| --- | --- | --- |
| `gish-carapace` | bridge to carapace's registry (~1,000 CLIs) | Day-one breadth for the cost of one plugin; build second |
| `gish-git-complete` | branches, remotes, modified files | Same process as `gish-git`, second service on the connection — shares the ref cache |
| `gish-kubectl` | cluster resource completion | Resident cache with TTL; upstream kubectl completion is slow *because* it's spawn-per-tab |
| `gish-make` | Makefile/justfile targets | Trivial parse, cached by file mtime |
| `gish-ssh` | hosts from `~/.ssh/config` | Skip hashed `known_hosts` entries — never un-hash, never guess |

## History backends (`HistoryBackend`)

| Plugin | What | Fast/correct notes |
| --- | --- | --- |
| *(native)* secret scrubbing | gitleaks-style rules in the shell's own store (#10) | Moved shell-side by design: a plugin cannot unwrite the authoritative local file. Matching commands are skipped entirely (ignorespace posture) with a notice; backends only ever receive scrubbed entries |
| *(native)* backend fan-out | async, deadline-bounded Append to every HistoryBackend after a successful local store | Fire-and-forget: the next prompt never waits; `stored=false` governs only a backend's own store |
| `gish-atuin` | ~~bridge to the user's own atuin~~ **landed** (#97, cmd/gish-atuin) | Append mirrors each command (`history start` + `end`; **`--duration` is nanoseconds** while the proto is milliseconds), Search serves ctrl-r from atuin's database. A bridge, not a reimplementation: atuin's opt-in, self-hostable, E2EE posture is the point, and gish reimplementing sync would inherit none of it. Commands travel in `ATUIN_COMMAND_LINE` (`--command-from-env`) so nothing is escaped, and results come back `--print0`-separated because commands contain newlines. No atuin installed = empty results, never errors |
| `gish-sync` | local-first SQLite history, cross-machine sync, frecency + directory-locality ctrl-r ranking | Local file is authoritative; sync is eventual and conflict-free (append-only log). Only worth building if the atuin bridge proves the demand |

## Plugin locality: where does a plugin belong when the shell is remote?

`gish ssh` (#98, docs/ssh.md) copies gish to a remote box and execs it
there. Plugins deliberately do **not** travel in v1 — the deadline-bounded
degradation already makes their absence safe, and a plugin is not a
single static file the way the shell is. But it raises a question the
contract has to answer before third-party plugins exist, because
retrofitting a default onto plugins already in the wild means guessing
wrong for half of them.

Every plugin is one of two kinds, and the distinction is not about speed:

- **Remote-local** — it describes the machine the shell is running on.
  `gish-git` reads the repo you are standing in; a version-manager
  segment reads that box's install tree. Running these anywhere else
  produces a confidently wrong answer, which is worse than no answer.
- **Identity-local** — it describes *you*, not the box. `gish-aws` reads
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

| Plugin | New surface | Notes |
| --- | --- | --- |
| command-not-found | one unary RPC | "did you mean", brew package suggestions; inherently off the hot path |
| env provider (direnv-class) | ~~`EnvProvider` service~~ **landed** (#12) | Trust model host-enforced: per-(plugin, dir, diff-hash) allow, deny-listed vars stripped, subtree revert |
| `gish-jump` (zoxide-class) | ~~CommandProvider~~ **landed** (#11) | Plugins register commands over gRPC: reserved-name guard, PATH shadowing, streamed I/O, mtime-cached discovery. gish-jump itself is now just a plugin to write |
| gish-agent | ~~new service~~ **landed** (#34, `AIProvider.Plan`) | `agent "<task>"` — the provider plans (a spec, never an exec channel); the shell renders it, saves it as an artifact, and executes approved steps through the real exec path, sandbox-wrapped, with destructive steps gating individually (provider flag OR the shell's own parse) and escalation as its own explicit answer. Hooks split to their own issue (ShellEvents) |
| AI assist | ~~invoked RPC~~ **landed** (#20, `AIProvider`) | `??` prefix → Compose (streamed candidates, best-first) lands in the editor buffer wrapped in a visible #21 sandbox invocation — never auto-executes; `explain` builtin → Explain for the last command. Context is scrub-safe by construction (recent commands come from the #10-gated history store; env is the allowlist). Human-scale deadline (90s), Ctrl-C cancels; `gish-claude` (cmd/gish-claude) is the reference provider, driving the user's claude CLI; GISH_AI_PROVIDER selects among several, GISH_AI_SANDBOX tunes the wrap |

## Build order

1. **`gish-git`** — hardest latency case (every prompt, every repo); proves
   the deadline/stale/repaint machinery before anything depends on it.
2. **`gish-carapace`** — completion breadth for free.
3. ~~`gish-scrub`~~ — done natively in the shell store (#10), with the
   HistoryBackend fan-out alongside it.

Everything else rides on the infrastructure those three force into
existence.
