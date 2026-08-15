# AGENTS.md — gish

Guidance for AI coding agents (Claude Code, Cursor, Copilot, Codex, OpenCode, …) working in this repository. This is the **cross-tool single source of truth** — `CLAUDE.md` imports it.

## Project Overview

**gish** is a new interactive shell: zsh's interactive experience, bash's ubiquity as scripting substrate, and a native, contract-first plugin system. Cross-platform (macOS, Linux, Windows), written in Go, shipped as a single static binary named `gish`.

The name expands bash-style: **gish = gRPC Interactive SHell** — the tier-2 plugin system is the differentiator, so it's in the name, the way bash's expansion carries its own origin story. Rhymes with fish/wish; chosen after an exhaustive availability sweep (~55 candidates) found every other 3–4 letter `-sh` name claimed, most by existing shells.

The design bet is a **two-tier plugin system**:

- **Tier 1 — script plugins**: the existing zsh plugin ecosystem (zi/zinit/oh-my-zsh style). These run in-process against a zsh-compat layer so users keep their plugins on day one. This tier is the adoption story; the compat layer is by far the hardest part of the project and lands incrementally.
- **Tier 2 — native gRPC plugins**: resident subprocesses over hashicorp/go-plugin with a versioned protobuf API (`proto/gish/plugin/v1`). Completion providers, prompt segments, history backends. This tier is the differentiator: plugins get a real contract instead of poking shell internals, and can be written in any language. Precedent: gitstatusd and carapace already prove out-of-process is how fast shell tooling works.

Non-negotiable latency rule for tier 2: every host→plugin call carries a deadline; a slow plugin degrades (stale segment, missing completions), it never blocks a keystroke or the prompt.

**Status: interactive core in progress (M2).** Scripting runs on `mvdan.cc/sh` (POSIX/bash parse + interp). Interactive terminals get the raw-mode line editor (`internal/editor`): emacs keymap, kill ring, undo, grapheme-aware multi-line editing, diff-based inline rendering, and byte-level type-ahead preservation across commands. Signal posture is in place (#3): the interactive shell survives SIGINT/SIGQUIT — Ctrl-C interrupts the foreground command (children via the kernel, builtin loops via context cancellation) and never the shell. History is in (#4): metadata-rich JSONL at `$XDG_DATA_HOME/gish/history.jsonl` (mirrors the plugin proto's HistoryEntry), prefix-aware up/down, ctrl-r incremental search, ignorespace + consecutive-dedup + secret scrubbing (#10: gitleaks-style rules in internal/history/scrub.go — matching commands never reach disk, with a notice). Stored entries fan out asynchronously to HistoryBackend plugins (fire-and-forget, deadline-bounded). Startup config is in (#6): the rc file (`$GISH_RC` → `$XDG_CONFIG_HOME/gish/gishrc` → `~/.gishrc`, first hit wins) runs in the session runner, and `GISH_PROMPT`/`GISH_PROMPT_CONT` take zsh-style escapes (`%u %h %w %W %? %%`) re-expanded before every prompt — a stopgap until the M3 prompt engine. Job control is in (#5): each command line runs in its own process group with terminal handoff (`SysProcAttr.Foreground`, race-free), stops observed via `Wait4(WUNTRACED)`, and jobs/fg/bg as gish builtins routed through the CallHandler rewrite. **The M2 interactive core is complete.** M3 has begun: the plugin host (#7) is live — discovery from `$XDG_DATA_HOME/gish/plugins`, lazy resident launch, Describe-driven capability wiring, deadline constants from docs/plugins.md, crash healing with exponential backoff, and the `plugins` builtin for inspection. The zi plugin manager (#23) is in: zi-go's engine ported to internal/plugmgr behind the replaceable Manager contract, driven by the `zi` builtin through the CallHandler — a load becomes `source <payload>` in the live shell (the upstream zsh shim is gone). Existing Zi syntax and ~/.zi-go layouts carry over. The flagship gish-git plugin (#8) ships in cmd/gish-git — a gitstatusd-class prompt segment (fsnotify invalidation, background refresh, stale-on-miss) consumed via the `%p{git}` prompt escape. Completion v1 (#32, M4) is in: Tab completes command names (PATH+builtins+functions, cached) and file paths (core providers in internal/complete), merges CompletionProvider plugins behind the 80ms budget, inserts escaped values (host owns quoting), and lists candidates below the edit line when ambiguous. The carapace bridge (#9, cmd/gish-carapace) plugs ~1,000 CLIs into that socket by shelling out to the user's carapace binary (`export` JSON), guarded by its supported-command list — no carapace installed means empty results, never errors. CommandProvider (#11) is live: plugins register commands (reserved builtin names rejected, PATH shadowed, streamed I/O, no raw-TTY handover in v1) routed by an mtime-cached command index — warm sessions know plugin commands without launching anything. Startup is guarded (#37): ~6ms warm to first prompt, CI regression gate in cmd/gish/startup_test.go (`make bench-startup`); prompt-segment discovery warms in the background and the first render waits only within its 50ms budget — plugin launches never block first paint. Fish parity is in (#38/#39): parser-driven syntax highlighting (real parse tree — unknown commands red, strings/expansions/comments styled, first-word fallback on mid-edit parse errors) and history ghost-text autosuggestions (Right/End accepts), both skipped under NO_COLOR/dumb terminals. M5 quick wins: live cross-session history (#40: lookups reload the file tail, own-session entries skipped, partial-line tolerant), Damerau did-you-mean suggestions on command-not-found (#42), native brew shellenv at startup (#44: stat/string only, no subprocess), and first-class starship (#45: GISH_THEME=starship renders via the binary, budgeted + stale fallback, hint-never-autoswitch). Login-shell correctness (#41): -l or a dash-prefixed argv[0] sources /etc/profile then ~/.gish_profile|~/.profile ($GISH_PROFILE overrides for tests); the -c/script path is POSIX-clean and prompt-free, pinned by the compat suite in cmd/gish/compat_test.go. The prompt starts naked: the out-of-box default is the stock zsh/bash shape (`user@host dir %`), and themes are explicit opt-ins — GISH_THEME=p10k selects the native p10k-class two-line theme (#26, internal/repl/theme.go: smart path, git segment, .tool-versions pins, jobs/duration/exit), GISH_THEME=starship the starship renderer, and any manual GISH_PROMPT always wins; NO_COLOR and TERM=dumb force naked regardless. Footgun diagnostics (#46, internal/repl/lint.go): the parsed buffer is checked per keystroke — unquoted expansion under destructive commands, cd sequenced (not &&-chained) before a destructive command, unquoted expansion in [ ], useless cat, self-truncating redirects — and warnings render as dim caution lines below the edit line via the editor's Diagnose hook; multi-line buffers additionally get a budget-bounded shellcheck pass on Enter when installed. Advisory only, never blocking; GISH_LINT=on|native|off. The `config` command (internal/repl/config.go, groundwork for #28) sets and persists settings in one step — `config theme starship` validates the value, rewrites the assignment in the rc file (creating the XDG one when none exists), and goes live via a CallHandler rewrite to `eval`; settings: theme (plain|p10k|starship), lint (on|native|off), prompt (free-form). Theme engine v2's first slice (#28): the themed first line is segment-driven — GISH_THEME_SEGMENTS picks and orders segments (built-ins dir git pins jobs duration exit, plus any %p{id} plugin segment by its id), GISH_THEME_COLOR_<ID> overrides a segment's color (named palette or raw SGR params), and config grew dotted keys: `config theme.git off` toggles one segment (built-ins re-slot in default order, plugin ids append), `config theme.segments 'dir git exit'` sets the list, `config theme.color.dir cyan` overrides (`default` clears), `config theme.lines 1` drops the two-line frame for an inline arrow, `config theme.sep powerline` swaps the two-space separator for a dim nerd-font chevron; bad values degrade to defaults, never break the prompt. `config theme` on a terminal is the p10k-configure-style wizard (internal/repl/wizard.go): ask each knob with Enter-keeps-current, show the powerline glyph and let the human judge the font (ask-don't-detect), preview the chosen layout, and persist only on the final confirmation — one rc write per changed variable, one eval for the live assignments; piped stdin keeps the plain show so scripts never hang. The doctor command (#67, internal/repl/doctor.go, a CallHandler builtin like config): checks the moving parts — rc file resolves and parses, GISH_THEME_* values valid (starship/shellcheck binaries present when selected), history file writable with a parsable tail, plugin-dir entries executable, TERM/NO_COLOR degradations explained — each line ✔/⚠/✘ with the exact fix command; exit 1 only on ✘, advisory-only (never mutates), and reachable from a working shell via `gish -c doctor`. The parallel builtin (#49 v1, internal/builtins/parallel.go): `parallel [-j N] [--collect] [--fail-fast] -- cmd… ::: inputs` (or stdin-fed) runs a goroutine pool of directly-exec'd process children — {} substitution (appended when absent), per-task line-prefixed streaming or --collect in input order, worst exit status, context-driven cancelation shared with Ctrl-C; shell functions as tasks are deferred until interp sub-runners. `zi update` (no target) fans its per-object updates out through the same ordered-pool discipline — 8-wide (network-bound), status lines in listing order. The editor's renderer is ANSI-aware (escapes are zero-width and atomic in wrapping) and prompts may span multiple lines (banner + edit line). No zsh dialect or plugin dispatch yet — the proto surface and go-plugin scaffolding exist so the contract is designed before the internals grow around it.

## Quick Reference

```bash
make build      # Build binary to build/gish (ldflags stamp version/commit/date)
make install    # go install ./cmd/gish
make test       # Run tests with race detector: go test -v -race ./...
make test-cover # Tests + coverage.html
make fmt        # Format: go tool gofumpt -w .
make vet        # go vet ./...
make lint       # go tool golangci-lint run ./...
make check      # fmt + vet + test  (note: does NOT run lint — run `make lint` separately before a PR)
make tidy       # go mod tidy
make proto      # Regenerate pkg/pluginapi from proto/ (needs protoc, protoc-gen-go, protoc-gen-go-grpc)
make sync       # Rewrite .tool-versions' golang pin from go.mod
make check-versions  # Assert go.mod ↔ .tool-versions agree (runs the pre-commit hook)
```

## Project Structure

```
cmd/gish/main.go        # Entry point: -c command, script file, or interactive REPL
internal/
  repl/                  # Read-eval loop over mvdan.cc/sh parser + interpreter.
                         # TTY stdin → line editor; piped stdin → plain loop;
                         # RunReader stays the script path
  editor/                # Raw-mode line editor (the zle-equivalent): buffer,
                         # keymap, kill ring, undo, history nav + ctrl-r search,
                         # Tab completion (escaped insertion, candidate list),
                         # diff-based inline renderer
  complete/              # Core completion providers: command names (PATH scan
                         # cached per PATH value, builtins, functions) and file
                         # paths (~ expansion, dir-aware, hidden-on-request)
  history/               # JSONL history store: entry shape mirrors the plugin
                         # proto's HistoryEntry; local file is authoritative
  jobs/                  # Job control (unix): per-line process groups, terminal
                         # handoff, WUNTRACED stop tracking, jobs/fg/bg builtins
  builtins/              # gish-native builtins on the ExecHandler seam; names
                         # the interpreter recognizes need the CallHandler rewrite
  plugmgr/               # The zi plugin manager: zi-go's engine in-tree (spec,
                         # ice, installer, ghr, emit, state...) behind the
                         # replaceable Manager contract. Payloads are emitted in
                         # gish dialect and sourced by the interpreter directly
  term/                  # TTY abstraction (raw mode, event decoding, type-ahead
                         # carry) — the swappable plumbing boundary from #1.
                         # Nothing outside this package imports a terminal library
  pluginhost/            # Tier-2 host: go-plugin handshake + capability glue, and
                         # the Host manager — discovery, lazy resident lifecycle,
                         # Describe wiring, crash healing, event_seq source. The
                         # hermetic fixture plugin lives in testdata/fixture
proto/gish/plugin/v1/   # The versioned tier-2 plugin contract (source of truth):
                         # common.proto (Describe/capabilities), completion.proto,
                         # prompt.proto, history.proto
pkg/pluginapi/           # protoc output from proto/ — generated, never hand-edited;
                         # exported so plugin authors can import it
docs/design.md           # Architecture: two-tier plugins, milestones, open questions
docs/plugins.md          # Tier-2 plugin roadmap: planned plugins, latency budgets, build order
```

## Architecture invariants

Read these before touching `pluginhost` or the protos.

- **`proto/gish/plugin/v1` is frozen-additive**: new fields and new RPCs are fine; renames, type changes, and removals are not. Breaking changes mean a `v2` package and a `Handshake.ProtocolVersion` bump.
- **Deadlines are the host's job**: `pluginhost` attaches a deadline to every outbound call (prompt segments default to a 50ms budget; segments can declare their own via `SegmentDescriptor.budget_ms`). Plugin responses that miss the deadline are dropped or served stale — never awaited.
- **Plugins are resident**: launched lazily on first use, kept alive for the session. Never spawn-per-call.
- **Environment is allowlisted**: completion/prompt requests carry a filtered env map, never the full environment.
- **The shell owns history**: a HistoryBackend plugin observes and serves search; the local history file works with zero plugins installed.
- **Windows is a target, not a port**: go-plugin falls back to TCP loopback where unix sockets are unavailable; don't introduce unix-socket-only assumptions.

## Code Conventions

- **Go version**: `go.mod`'s `go` directive and `.tool-versions`' `golang` pin must match **exactly** — enforced by the `check-go-version-sync` pre-commit hook from [blairham/pre-commit-hooks](https://github.com/blairham/pre-commit-hooks). `go.mod` is authoritative; run `make sync` to bring `.tool-versions` back in line. CI pins the minor line (`GO_VERSION: "1.26"`)
- **Formatter**: gofumpt via `go tool` (pinned in go.mod's `tool` block alongside golangci-lint). Formatting is applied at commit time by the `golangci-lint-fmt` hook, driven by `.golangci.yml`
- **Linter**: golangci-lint v2, config in `.golangci.yml` (`pkg/pluginapi` is excluded — it's generated)
- **Imports**: grouped by goimports with local prefix `github.com/blairham/gish` — local imports get their own trailing group
- **Generated code**: `pkg/pluginapi` comes from `make proto` only; regenerate rather than edit, and commit the output so builds don't require protoc
- **Exit codes**: `errors.As` with `interp.ExitStatus` distinguishes a script's exit status (propagated as the process exit code) from real gish errors (stderr + exit 1)
- **Commits/PRs**: no AI-attribution trailers — do not add `Co-Authored-By: Claude`, "Generated with Claude Code", or similar to commit messages or PR bodies

## CI/CD

- **ci.yml**: `test` (ubuntu + macos matrix, `make test`), `lint` (golangci-lint-action), then `build` — on push to main and PRs
- **goreleaser.yml**: GoReleaser on version tags (`v*`) or manual dispatch; publishes to `blairham/homebrew-tap`
- **Pre-commit hooks** (`pre-commit install`): trailing-whitespace, end-of-file-fixer, check-yaml, check-added-large-files, check-merge-conflict, detect-private-key, go-mod-tidy-repo, `check-go-version-sync`, `golangci-lint-fmt` + `golangci-lint` (changed-since-HEAD only; whole-repo lint stays in CI), gitleaks

## Key Dependencies

- `mvdan.cc/sh/v3` — POSIX/bash parser and interpreter; the scripting substrate the zsh dialect grows on top of
- `hashicorp/go-plugin` — tier-2 plugin transport (same architecture as the cloudctl/understudy/chaos-lab tools)
- `google.golang.org/grpc` + `protobuf` — the plugin contract
- `charmbracelet/ultraviolet` — input **decoding only** (`EventDecoder`), behind `internal/term`; gish owns the read loop
- `golang.org/x/term` — raw-mode entry/restore
- `rivo/uniseg` — grapheme clusters and display widths for the editor/renderer

## Testing

- `go test -race ./...`; the REPL core is tested through `repl.RunReader` with `interp.StdIO` redirection — no real terminal needed
- Tests must never touch real user state (home directory, real shell rc files, real history). Use `t.TempDir()` + `t.Setenv`
- Plugin host integration tests (spawning a real plugin binary over go-plugin) land with the first dispatch path; keep them hermetic by building the fixture plugin from `testdata` at test time
