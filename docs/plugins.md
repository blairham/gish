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
| Completion request | ~80ms to first batch | show whatever batches arrived; stream stays open until the user types again |
| History append | none — fire-and-forget | shell never waits; backend scrubs/stores on its own time |
| History search (ctrl-r) | ~100ms to first batch | partial results render, best-first |
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
| `gish-aws` | active profile + SSO token expiry countdown | Reads the local token cache only; never calls AWS on the prompt path |
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
| `gish-sync` | local-first SQLite history, cross-machine sync, frecency + directory-locality ctrl-r ranking | Local file is authoritative; sync is eventual and conflict-free (append-only log) |

## Needs new proto services (v1 is frozen-additive — new services are fine)

| Plugin | New surface | Notes |
| --- | --- | --- |
| command-not-found | one unary RPC | "did you mean", brew package suggestions; inherently off the hot path |
| env provider (direnv-class) | `EnvProvider` service | On cwd change, plugin returns an env diff. Requires direnv's `allow` model: allowlist + explicit per-directory approval — an env plugin must not be able to silently rewrite `PATH` for every directory |
| `gish-jump` (zoxide-class) | ~~CommandProvider~~ **landed** (#11) | Plugins register commands over gRPC: reserved-name guard, PATH shadowing, streamed I/O, mtime-cached discovery. gish-jump itself is now just a plugin to write |
| AI assist | invoked RPC (not ambient) | natural language → command, "explain that error". Explicitly invoked, human-scale latency, can never touch the keystroke path |

## Build order

1. **`gish-git`** — hardest latency case (every prompt, every repo); proves
   the deadline/stale/repaint machinery before anything depends on it.
2. **`gish-carapace`** — completion breadth for free.
3. ~~`gish-scrub`~~ — done natively in the shell store (#10), with the
   HistoryBackend fan-out alongside it.

Everything else rides on the infrastructure those three force into
existence.
