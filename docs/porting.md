# porting your muscle memory

You have twenty years of finger habits. This page is about keeping
them. Most of it is "that already works" — the list exists so you can
check rather than discover.

## Things that just work

Everything here behaves as it does in bash/zsh emacs mode. No
configuration, no plugin.

| you type | you get |
| --- | --- |
| `!!`, `!$`, `!^`, `!:2`, `!:1-3` | history expansion, echoed before it runs |
| `!make` | the last command starting with `make` |
| `^typo^fixed` | rerun the last command with a substitution |
| `Alt-.` / `Alt-_` | last argument of the previous command — **press again to walk further back** |
| `Ctrl-R` | incremental reverse search; press again to cycle, `Ctrl-G` to abort back to your line |
| `Ctrl-X Ctrl-E` | edit the current line in `$EDITOR`, come back with it loaded |
| `Alt-#` | comment out the line and park it in history |
| `Ctrl-O` | run this history entry and queue the next one |
| `Alt-b` / `Alt-f` / `Alt-d` / `Alt-Backspace` | word motions and word kills |
| `Ctrl-A` / `Ctrl-E` / `Ctrl-K` / `Ctrl-U` / `Ctrl-W` / `Ctrl-Y` / `Alt-Y` | line motions, kills, and the kill ring |
| `Ctrl-T` | transpose characters |
| `Ctrl-_` / `Ctrl-/` | undo |
| `Tab` on `$VAR` or `~` | expands it in place (the zsh behavior fish users miss) |
| a leading space | keeps the command out of history |

Not yet: numeric arguments (`Alt-4 Ctrl-D`) — tracked in
[#116](https://github.com/blairham/gish/issues/116).

## Things that changed on purpose

**The prompt starts naked.** Out of the box you get `user@host dir %` —
the shape you came from — because a new shell should not greet you as a
stranger. Themes are opt-in:

```sh
config theme p10k        # native two-line theme: git, runtime pins, jobs, duration, exit
config theme starship    # your exact starship prompt, unchanged
config theme             # interactive walkthrough
```

`config` writes to your rc file *and* applies immediately, so there is
no "edit file, restart shell" loop.

**Your `.zshrc` does not carry over, and that is the point.** gish ships
natively what most of that file was buying you: autosuggestions, syntax
highlighting, a p10k-class prompt, completions, direnv-class env
switching, asdf/mise version switching, zoxide-class jumping. Start from
nothing and add only what you miss — see [the config
guide](../README.md#configuration).

**Your `eval "$(tool init bash)"` hooks are mostly unnecessary.**
Homebrew's environment, `.tool-versions` switching, and directory
jumping are built in. Where a tool still needs its hook, it works — with
one caveat below.

## Known rough edges

Read [docs/compat.md](compat.md) for the measured scoreboard. The ones
you are most likely to hit:

- **Tools that check `$BASH_VERSION`** take their POSIX branch, because
  gish does not claim to be bash. Usually fine; occasionally a tool
  refuses. Tracked in
  [#120](https://github.com/blairham/gish/issues/120).
- **`declare -A` associative arrays** are partially supported —
  `${#map[@]}` miscounts.
- **`${var/#pat}` / `${var/%pat}`** anchored substitution silently does
  nothing. Use `${var#pat}` / `${var%pat}` for stripping.
- **`exec 3>&1` fd juggling** does not persist.
- **`printf '%05.2f'`** rejects combined width.precision.

All four are `mvdan.cc/sh` substrate gaps, tracked with reproductions in
[#119](https://github.com/blairham/gish/issues/119). Scripts that hit
them still run correctly under `bash script.sh` — gish does not change
what `#!/bin/bash` means.

## Trying it without commitment

Do **not** `chsh` on day one. Launch it from your terminal emulator's
profile, or just run `gish` in a tab. Everything gish stores lives under
`$XDG_CONFIG_HOME/gish` and `$XDG_DATA_HOME/gish`; deleting those puts
you exactly where you started.

```sh
gish                # try it in this tab
doctor              # what's configured, what's missing, and the fix for each
```

When you do want it as a login shell, `docs`-level instructions are in
the [README](../README.md#using-gish-as-your-login-shell) — and your
`.profile` still runs.
