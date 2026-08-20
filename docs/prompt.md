# The prompt engine

koi ships a native port of [powerlevel10k][p10k]: the same prompt
shape, the same presets, the same configuration vocabulary, written in
Go and rendered in the shell's own process.

```sh
config theme p10k     # turn it on
prompt configure      # pick a look
prompt import         # bring your ~/.p10k.zsh across
```

`p10k` is the same command as `prompt`, kept because it is what people
arriving from powerlevel10k type without thinking. The engine is named
for what it does rather than for one of the dialects that configure it
([#184](https://github.com/blairham/koi-shell/issues/184)) — it renders
presets from more than one upstream, and naming the whole thing after
powerlevel10k would claim looks that project never shipped.

What did **not** change: `KOI_THEME=p10k`, `POWERLEVEL9K_*`,
`p10k.conf` and `~/.p10k.zsh`. Those name powerlevel10k compatibility,
which is exactly what they are, and
[#134](https://github.com/blairham/koi-shell/issues/134) settled that
pasting a line from an old config has to keep working.

[p10k]: https://github.com/romkatv/powerlevel10k

## Why port it rather than run it

powerlevel10k is the best prompt in the zsh world and the main reason
people say they cannot leave zsh. koi cannot run it — it is ~10,000
lines of zsh, and koi has no zsh interpreter and is not getting one
(`AGENTS.md`: no zsh dialect on the prompt path). So the prompt shape
had to be rebuilt.

Rebuilding it turned out to be worth doing on its own terms:

| | time to first prompt |
| --- | --- |
| zsh + powerlevel10k, real config | 86.3 ms |
| koi, p10k theme, every segment resolved | 6.1 ms |

Same prompt, measured the same way, on the same machine — see
[bench.md](bench.md) for the methodology and the rest of the table. The
gap is not cleverness. It is that a prompt segment here reads a file
where a shell prompt runs a command, and that koi is already running
when the prompt is asked for.

## Presets

`prompt configure` walks through them with a preview; `prompt preset <name>`
skips the wizard.

| preset | what it is |
| --- | --- |
| `lean` | two lines, no backgrounds — the default, and the one that needs no special font |
| `classic` | framed, one muted background, powerline separators |
| `rainbow` | framed, a background per segment |
| `pure` | the Pure theme's restraint: almost no color |
| `lean-8colors` | lean, restricted to the terminal's own eight colors |
| `robbyrussell` | the oh-my-zsh default, one line, unchanged |

### Looks from other projects

The layout pass spans "no backgrounds and a space separator" to "a
background per segment and powerline arrows", which is the same range
every other prompt's gallery lives in. So a look from another project
costs data and no code:

| preset | from | what it is |
| --- | --- | --- |
| `pastel-powerline` | starship | one line, purple → pink → orange → navy ribbon, no prompt character |
| `tokyo-night` | starship | two lines, cool dark ribbon fading to near-black at the clock |
| `agnoster` | oh-my-zsh | the powerline prompt people picture when they picture themed zsh |

The two starship presets are transcribed from starship's own preset
TOMLs, colour for colour. **`agnoster` is not a transcription** — an
oh-my-zsh `.zsh-theme` is a zsh program, not a config file, so there is
nothing to import and no converter that could produce one (see
[#185](https://github.com/blairham/koi-shell/issues/185)). It rebuilds the
published look.

Every one of these drops something, and `prompt preset <name>` says what
as it applies:

```console
$ prompt preset tokyo-night
saved tokyo-night to ~/.config/koi/p10k.conf

tokyo-night does not reproduce everything its upstream shows:
  starship $nodejs (needs a subprocess; no segment here forks)
  starship $bun (needs a subprocess; no segment here forks)
  …
```

The recurring gap is always the same one: starship runs `node --version`
and friends on the prompt path, and no segment in this engine forks —
that rule is where the speed comes from. koi's `nodenv` / `pyenv` /
`asdf` segments are *not* substituted in silently, because reading a pin
file answers a different question than running the tool: what the
project is pinned to, rather than what is on `PATH` right now.

It is said at apply time rather than in `prompt show` because the saved
file is a resolved list of settings — read it back and there is no way
to know what the preset chose not to carry.

## Configuration

Settings layer, later winning over earlier:

1. the preset (`KOI_P10K_PRESET`, default `lean`)
2. `$XDG_CONFIG_HOME/koi/p10k.conf`
3. `POWERLEVEL9K_*` variables set in the session

The file is the parameter namespace written down, one setting per line:

```
# koi p10k configuration
LEFT_PROMPT_ELEMENTS = dir vcs newline prompt_char
DIR_FOREGROUND = 31
TRANSIENT_PROMPT = always
```

Keys are the `POWERLEVEL9K_*` names with the prefix dropped; the prefix
is accepted on input, so a line pasted out of a `.p10k.zsh` works. Every
per-segment setting resolves through the same three-step chain upstream
uses, most specific first:

```
DIR_NOT_WRITABLE_FOREGROUND   →   DIR_FOREGROUND   →   FOREGROUND
```

Editing the file takes effect on the next prompt — it is re-read when
its mtime changes, so there is no reload command.

`prompt show` prints what is actually in effect and where it came from.

## Bringing your existing config across

```sh
prompt import              # ~/.p10k.zsh
prompt import path/to/file
```

Import reads the declarative settings out of a `.p10k.zsh` once and
writes them natively. It is the only part of koi that looks at zsh, and
it never runs at prompt time.

Against a real 1,720-line configuration it takes 304 of 310 settings.
**What it cannot take is the code.** A config generated by `p10k
configure` defines a shell function — `my_git_formatter` — and points
`VCS_CONTENT_EXPANSION` at it. Honoring that would mean running zsh to
draw a prompt. Those settings are listed by name when you import, rather
than dropped silently or half-interpreted into something that looks
nearly right. The native `vcs` segment already produces the same
information, so in practice the report is a note rather than a task.

## Transient prompt

```
TRANSIENT_PROMPT = always | same-dir | off
```

Once a command has run, its prompt collapses to the bare prompt
character, so scrollback is your output rather than twenty copies of a
two-line frame. This is probably the single setting most responsible for
a themed prompt staying usable.

`same-dir` differs slightly from upstream by necessity: the trim happens
when the line is accepted, before the command has run, so nothing can
yet know whether it will change directory. koi reads it the other way
round — a prompt is trimmed unless the *previous* command moved. Either
way a directory change leaves one full prompt behind as a landmark; it
lands just below the `cd` rather than just above it.

## Instant prompt

Not implemented, deliberately.

Upstream paints a cached prompt at startup because the real one is not
ready for tens of milliseconds. koi reaches a fully resolved p10k
prompt in about 7 ms — the same number it takes to draw the naked
default — so there is nothing to hide behind a cache, and a cache is not
free: it needs invalidating, it goes stale across directory changes, and
upstream's is well known for the console-output warnings it produces.

The setting is accepted so imported configurations keep working, and
`prompt show` says what it is doing (nothing) when one asks for it.

## Segments

Implemented:

```
anaconda asdf aws aws_eb_env azure background_jobs chezmoi_shell
command_execution_time context cpu_arch detect_virt dir direnv fvm
gcloud goenv google_app_cred haskell_stack jenv kubecontext lf luaenv
midnight_commander nix_shell nnn nodeenv nodenv nvm os_icon
per_directory_history perlbrew phpenv plenv prompt_char proxy pyenv
ranger rbenv rvm scalaenv status terraform time todo toolbox vcs
vim_shell virtualenv xplr yazi
```

`prompt list` prints the current set. An element in your configuration
that has no implementation renders as nothing, and `prompt show` names it
under `not yet` so it is not just invisible.

The rule this package holds to is that a segment may read files and
environment variables and nothing else — no forking, no dialing, no
blocking. That rule is where the 6 ms comes from.

**System state now lands inside it** (#132): `ram`, `swap`, `load`,
`disk_usage` and `battery` are files and one syscall, not subprocesses.
What is available is per-platform, and the gaps are per *metric* rather
than per segment:

| metric | Linux | macOS |
| --- | --- | --- |
| `load` | `/proc/loadavg` | `sysctl vm.loadavg` |
| `swap` | `/proc/meminfo` | `sysctl vm.swapusage` |
| `disk_usage` | `statfs` | `statfs` |
| `ram` | `/proc/meminfo` | — needs mach's `host_statistics64`, which wants cgo |
| `battery` | `/sys/class/power_supply` | — needs IOKit, same reason |

An unavailable metric renders nothing, which is what an absent reading
should look like; `vm_stat` and `pmset` would answer the last two, and a
prompt that forks twice is exactly what this engine exists not to be.

`ip` and `vpn_ip` are in (#132): enumerating interfaces forks nothing
and dials nothing, but one enumeration costs more than the whole lean
prompt (~51µs on darwin — N+1 routing-table dumps), so both read a
5-second TTL cache rather than the kernel per prompt; upstream itself
computes this table in an async worker. Selection is upstream's:
`POWERLEVEL9K_IP_INTERFACE` (default `.*`) and
`POWERLEVEL9K_VPN_IP_INTERFACE` (default wireguard/tun/tailscale/
zerotier) are anchored regexes over interface names, first match wins,
`VPN_IP_SHOW_ALL` shows every match. IPv4 only, as upstream's parsers
are.

Still absent: the dialing half of the network group (`public_ip`,
`nordvpn`) and the task trackers (`taskwarrior`, `timewarrior`), along
with the `*_version` family that shells out to each tool — the version
*managers* cover most of that need by reading pin files. The mechanism
they would
need is the one the git counters use: something else computes, the
prompt renders what is known and marks it stale. Wiring them to it is a
decision about how much a prompt should quietly be doing in the
background, worth making deliberately rather than because the mechanism
happened to exist.

## Git status

Split by what it costs:

- **the branch** is one cached file read, done synchronously — it is what
  you navigate by and it must never be late;
- **the counts** (modified, untracked, ahead, behind) need a full index
  versus working-tree walk. That is the expensive half, and it is
  exactly why upstream hands it to a separate process (gitstatusd).

A prompt with no scanner attached shows the branch and no counts. It
never waits for either.

## As a plugin

`cmd/koi-p10k` serves the same engine over the tier-2 `ThemeProvider`
contract, advertising `p10k-lean`, `p10k-rainbow` and so on. koi itself
does not use it — the built-in path renders in-process, and the shell's
own prompt should not pay a round trip to draw itself. It exists for
other shells speaking the protocol, for distributing a theme on its own
release cadence, and as the worked example for anyone writing a
`ThemeProvider`.

## Credit

powerlevel10k is by Roman Perepelitsa and contributors, MIT licensed.
This is a reimplementation of its observable behaviour in Go, not a
translation of its source. Where this document says "upstream", that is
what it means.

The `pastel-powerline` and `tokyo-night` palettes are transcribed from
[starship](https://github.com/starship/starship)'s preset TOMLs
(ISC, starship contributors). `agnoster` reproduces the look of the
[oh-my-zsh](https://github.com/ohmyzsh/ohmyzsh) theme of that name (MIT);
no code was taken, because there is no configuration in it to take.

Naming a preset after someone else's project is a claim about fidelity,
which is why every one of them declares what it does not reproduce
rather than shipping a near-miss under a borrowed name.
