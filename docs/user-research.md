# User research: what people hate about their shells

Web sweep, 2026-08-14 — the complaints that drive shell switching, mapped
against gish. This document exists so the roadmap is driven by real pain,
not vibes. Issues filed under **M5 — Out-of-box experience**.

## The complaint → answer map

| # | Complaint (who bleeds users) | Evidence | gish answer | Status |
| --- | --- | --- | --- | --- |
| 1 | **Slow startup** — oh-my-zsh 500ms–5s; the #1 zsh→fish driver | HN threads, fish 4.0 marketing (<100ms) | static binary, lazy plugins, async segments — but make it a *guarantee* | #37: <30ms CI-enforced budget |
| 2 | **Bad defaults** — zsh needs a manual + plugin manager before it's pleasant; fish wins on out-of-box highlighting/autosuggestions/git prompt | every zsh-vs-fish comparison | theme ✓ prompt ✓ completion ✓ git ✓ — missing the two fish signatures | #38 syntax highlighting, #39 autosuggestions |
| 3 | **Breaking POSIX breaks tools** — fish/nushell as login shell → silent tool failures, scripts don't run, years of muscle memory lost | shell-env #20, nushell threads | *the founding bet*: bash-compatible core, zsh plugins via zi | #41 pins the login/non-interactive contract |
| 4 | **History lost across tabs/sessions** — bash keeps only the first shell's; zsh needs incantations; atuin's popularity is the demand signal | ohmyzsh #9430, atuin coverage | JSONL store already shared on disk | #40: live reload in concurrent sessions |
| 5 | **Unhelpful errors** — "command not found", nothing more | ubiquitous | we own the exec seam | #42: typo suggestions + install hints; #20 explain-error |
| 6 | **Config complexity** — oh-my-zsh sprawl, plugin-manager fatigue | manjaro/dev.to threads | zero-config defaults + zi built in (no bootstrap) + one rc file | shipped (#23, #26) |
| 7 | **Quoting/word-splitting footguns, ugly syntax** | HN perennials | inherited deliberately (compat > purity); #46: shellcheck-grade diagnostics at the prompt (we hold a real parse tree); plus #38 red-unknown-commands and #20 explain | #46 |
| 8 | **Windows second-class** | nushell's win there | M7 milestone opened; ConPTY groundwork in term boundary; WSL2 supported today | #47 |

## The positioning sentence this research supports

> gish: fish's out-of-box experience, bash's compatibility, and a plugin
> system with an actual contract — without giving up your scripts, your
> zsh plugins, or your login shell.

## Sources

- Manjaro forum: zsh opinions; sunlightmedia, tech-insider, linuxjunkies: bash/zsh comparisons
- HN 39101533 ("zsh gets heavyweight and slow"), HN 3533895, HN 4281382
- travisbrady.github.io + ziap.github.io + dev.to (joshmedeski, dinkopehar): zsh→fish switch stories
- github.com/sindresorhus/shell-env#20: non-POSIX login shells failing silently
- github.com/ohmyzsh/ohmyzsh#9430: history not shared across tabs
- commandlinux.com shell usage statistics 2026; botmonster/sumguy 2026 shell comparisons
