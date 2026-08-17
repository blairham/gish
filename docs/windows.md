# Windows

**Windows is a target, not a port** — but the interactive half is
hardware-gated, and this page is honest about which parts have been run
by a person and which have only been compiled.

Today: **WSL2 is the supported story.** Native Windows builds, passes
its test suite on a real windows runner, and packages
([#89](https://github.com/blairham/gish/issues/89)); what has not
happened is someone typing into gish on Windows Terminal
([#87](https://github.com/blairham/gish/issues/87)).

## What CI proves on every PR

A `windows-latest` job builds everything and runs the portable packages
under `-race`:

`term`, `complete`, `editor`, `history`, `envtrust`, `tools`, `sandbox`
(schema), `pluginhost` — including a real go-plugin round trip over the
TCP fallback — `repl`, `builtins`, `remote`, `session`, `capture`,
`blocks`, `prompt`, `migrate`, and the plugin binaries.

That covers the line editor's buffer, keymap, history, completion and
renderer; the prompt engine including its path handling; the plugin
host; and the config surfaces. It does not cover anything that needs a
console.

## What is done natively

- **PATHEXT-aware executable lookup.** Completion strips the extension,
  because the user types `git`, not `git.exe`.
- **Plugin discovery by extension**, since there is no exec bit.
- **`USERPROFILE` beside `HOME`** wherever a home directory is resolved.
- **Terminal size changes are polled**, at 250ms, because there is no
  SIGWINCH and the console reports a resize as an input *event* — which
  only the process currently reading input can see. Reading the input
  queue from the resize watcher would steal keystrokes from a running
  child, which is a far worse bug than a resize noticed a quarter of a
  second late.
- **go-plugin over TCP loopback**, since there are no unix sockets.

## What is gated on hands and a console (#87)

Each of these is written down as a checklist item rather than a claim,
and none of it is asserted anywhere until someone has run it:

- [ ] **Raw mode**: entry and restore through ConPTY / ConHost, and the
      terminal left usable after `exit`, after Ctrl-C, and after a
      crash.
- [ ] **VT input decoding**: ultraviolet's decoder claims Windows
      console input; prove it for arrows, Home/End, Alt chords, function
      keys, and bracketed paste.
- [ ] **Resize**: the poll above actually fires, and the editor reflows.
- [ ] **Grapheme width**: emoji and CJK in the prompt and the buffer.
- [ ] **Ctrl-C / Ctrl-Break**: they arrive as console control events
      rather than signals. The posture from
      [#3](https://github.com/blairham/gish/issues/3) has to hold — the
      foreground command dies, the shell never does — which needs
      `CREATE_NEW_PROCESS_GROUP` on the child and
      `GenerateConsoleCtrlEvent` to reach it.
- [ ] **Job objects**: Windows has no process groups and no SIGTSTP, so
      `fg`/`bg`/stop degrade by design; a job object per command line is
      what gives kill-tree correctness, which unix gets for free from
      the process group.

Job control is deliberately still reported as unsupported rather than
half-implemented: `jobs`, `fg` and `bg` say so. A shell that claims job
control and loses a background process is worse than one that says it
cannot do it yet.

## Installing

Once a release is tagged with
[#89](https://github.com/blairham/gish/issues/89)'s configuration:

```powershell
winget install blairham.gish
# or
scoop install gish
```

Both are wired through GoReleaser alongside the Homebrew tap, and both
skip cleanly when their token is absent — a release from a machine
without the secrets still publishes the GitHub release itself. Windows
on ARM is built too: it is no longer an exotic target.
