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
| `Alt-u` / `Alt-l` / `Alt-c` | upcase / downcase / capitalize the word |
| `Alt-t` | transpose words (`Ctrl-T` does characters) |
| `Ctrl-V` / `Ctrl-Q` | quoted insert — a literal Tab or control character |
| `Alt-r` | revert a recalled history line to how it arrived |
| `Alt-<` / `Alt->` | oldest history entry / back to the line you were typing |
| `Ctrl-]` / `Alt-Ctrl-]` | character search, forward and backward |
| `Ctrl-S` | forward search — raw mode clears `IXON`, so nothing eats it |
| `Alt-4 Ctrl-D`, `Alt-3 Alt-d`, `Alt-8 -` | numeric arguments prefix any command |

The keymap is complete against bash's emacs bindings apart from keyboard
macros (`Ctrl-X (` / `)`) and `Alt-*` insert-completions.

### Vi mode

`set -o vi` in your rc works, and so do `config editmode vi` and
`KOI_EDIT_MODE=vi`. Whichever you use, `set -o`, `shopt -o vi` and
`$SHELLOPTS` report it, so the save-and-restore a script does around a
mode change reads the mode the shell is actually in — and `set +o vi` or
`set -o emacs` takes you back, as in bash, where the two are mutually
exclusive. Every line starts in insert mode, as in bash and
zsh; Escape enters normal mode, and the cursor changes shape so you can
see which one you are in.

It is built as operators × motions × text objects rather than a list of
individual commands, so composition works:

| you type | you get |
| --- | --- |
| `d2w`, `3x`, `2dd` | counts, including a count on each side of an operator |
| `ciw`, `daw`, `ci"`, `di(`, `ca{` | text objects, inner and around |
| `cw` | changes to the end of the word, leaving the space — vi's own quirk |
| `f`/`t`/`F`/`T`, then `;` / `,` | find within the line, and repeat it |
| `w W b B e E 0 ^ $ G gg` | the motion set |
| `x X D C Y s S r ~ p P u` | the single-key edits, undo included |
| `i I a A o O` | the ways into insert mode |
| `k` / `j` | previous/next history — or up/down a line when the buffer has several |
| `v` | open the line in `$EDITOR` (bash's binding) |
| `/` | reverse history search |

Insert mode keeps the emacs keys: `Ctrl-A` to the start of a line you
are still typing is not worth a mode switch, and control chords work in
normal mode too, so `Ctrl-C`, `Ctrl-R` and `Ctrl-L` behave the same in
both.

Two deliberate limits. **Alt is Escape** in vi mode: they are the same
byte on the wire, terminals hand `<Esc>w` over as one chunk, and
resolving toward Escape is what makes the mode usable at typing speed —
the cost is the emacs alt bindings inside vi insert mode. And there is
no visual mode, no named registers, and no marks; what is missing is
missing uniformly rather than scattered through the pairs, which is the
difference between a vi mode with gaps and one that cannot be trusted.

## Your existing tools

koi implements bash's **hook surface**, so the add-ons you already have
work without waiting to be adopted:

| what your rc does | what happens |
| --- | --- |
| `eval "$(starship init bash)"` | your starship prompt renders |
| `eval "$(direnv hook bash)"` | `.envrc` applies on `cd` |
| `eval "$(fzf --bash)"` | fzf's `Ctrl-T` and `Ctrl-R` widgets bind |
| `eval "$(zoxide init bash)"` / `"$(mise activate bash)"` | the `PROMPT_COMMAND` hook runs each prompt |
| `PS1='\u@\h \w\$ '` | your own prompt renders, escapes and all |
| `PROMPT_COMMAND`, `PS0`, `trap … DEBUG`, `shopt -s extdebug` | all honored |
| `bind -x '"\C-t": widget'` | the key runs your command, with `READLINE_LINE`/`READLINE_POINT` |
| `complete -F _fn cmd`, `compgen -W …` | your completions drive Tab |
| `command_not_found_handle` | runs, with the whole command line |

Measured per release in
[docs/interactive-compat.md](interactive-compat.md), by sourcing each
tool's own init line and asserting what it does — not by asserting that
we implement the hooks.

**koi claims bash's interface, not bash's identity.** `BASH_VERSION`
and `BASH_VERSINFO` report a modern bash, because tools use them as
feature probes and gate on them: fzf checks `BASH_VERSINFO[0] < 4` to
decide between a readline macro and `bind -x`, and koi implements the
latter. `$0` stays `koi`, and `KOI_VERSION` says exactly what you are
running.

Two known gaps, both honest: readline **macro** bindings (a key bound to
a string of editing commands) are not emulated, and `declare -F name` —
the "is this function defined?" test — is a known substrate gap.

## Things that changed on purpose

**The prompt starts naked.** Out of the box you get `user@host dir %` —
the shape you came from — because a new shell should not greet you as a
stranger. Themes are opt-in:

```sh
config theme p10k        # native two-line theme: git, runtime pins, jobs, duration, exit
config theme starship    # your exact starship prompt, unchanged
config theme             # interactive walkthrough (the small dialect)
```

`config` writes to your rc file *and* applies immediately, so there is
no "edit file, restart shell" loop.

**Your `.zshrc` does not carry over, and that is the point.** koi ships
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

- **Tools that probe identity rather than features**: `BASH_VERSION`
  answers the feature probes, but `$0` stays `koi`, so a tool that greps
  `$0` or `$SHELL` for the literal string `bash` takes its POSIX branch.
  Usually fine; occasionally a tool refuses.
- **`declare -A` associative arrays** are partially supported —
  `${#map[@]}` miscounts.
- **`exec 3>&1` fd juggling** does not persist.
- **`printf '%05.2f'`** rejects combined width.precision.

These are `mvdan.cc/sh` substrate gaps, kept with reproductions so
they can be re-verified — `${var/#pat}` and `${var/%pat}` were on this
list until #636 and now answer as bash does. Scripts that hit
them still run correctly under `bash script.sh` — koi does not change
what `#!/bin/bash` means.

## Trying it without commitment

Do **not** `chsh` on day one. Launch it from your terminal emulator's
profile, or just run `koi` in a tab. Everything koi stores lives under
`$XDG_CONFIG_HOME/koi` and `$XDG_DATA_HOME/koi`; deleting those puts
you exactly where you started.

```sh
koi                # try it in this tab
doctor              # what's configured, what's missing, and the fix for each
```

When you do want it as a login shell, `docs`-level instructions are in
the [README](../README.md#using-koi-as-your-login-shell) — and your
`.profile` still runs.
