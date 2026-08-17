# ACP: the agent edge

gish speaks the [Agent Client Protocol](https://agentclientprotocol.com)
in two directions, deliberately in different places.

| direction | what it means | where it lives |
| --- | --- | --- |
| **inbound** — gish uses an agent | `??` and `explain` are answered by any ACP agent | `cmd/gish-acp`, a plugin |
| **outbound** — an agent runs inside gish | the agent's commands execute through gish's sandbox and deadlines | `gish acp`, core |

The split is not organizational. **A plugin may never hold an exec
channel** ([#34](https://github.com/blairham/gish/issues/34)), so the
outbound role cannot be a plugin without weakening the invariant that
makes plugins safe. The inbound role is a plugin precisely because it is
deletable if ACP does not take.

## The spike (#166), answered

The load-bearing assumption was that the terminal capability has real
adopters. Checked against the protocol's own documentation and the
2026 record:

- **The terminal capability is client-side and optional.** A client
  advertises `"terminal": true` in its `initialize` response, and an
  agent that does not see it **MUST NOT** call any terminal method. So
  gish implementing it is purely additive — every agent already has the
  branch for "no".
- **The methods are what the design assumed**: `terminal/create`
  (returns a terminal id immediately, the command runs in the
  background), `terminal/output` (non-blocking, with a `truncated` flag
  and an `exitStatus` when finished), `terminal/wait_for_exit`,
  `terminal/kill` (the terminal stays valid), `terminal/release`.
- **Adoption is real and broad.** 25+ agents by early 2026; clients
  include Zed, JetBrains IDEs, Neovim, VS Code, and — since June 2026 —
  Microsoft's Intelligent Terminal, whose agent pane auto-detects ACP
  CLIs (Copilot CLI, Claude Code, Codex CLI, Gemini CLI). GitHub shipped
  ACP support in Copilot CLI in January 2026.
- **Governance and stability**: wire v1 is stable and shipped, evolved
  through a public RFD process (15+ RFDs), under a vendor-neutral org
  with TypeScript, Kotlin and Rust SDKs. **v2 is a published draft** as
  of July 2026, and its own announcement is explicit that "adding v2
  support should not mean dropping v1" — v1-only peers will be common
  for a long time.
- **MCP does not compete.** MCP is agent↔tools; ACP is agent↔client.
  They sit at different layers and are routinely used together (an ACP
  `session/new` carries the MCP servers the agent should use).

**Verdict: go**, on v1, for both roles.

## What gish adds that ACP leaves out

The spec defines **no permission model, no sandboxing, and no timeout**
— it tells agents to race a timer against `wait_for_exit` and call
`terminal/kill`. That is the correct scope for a protocol and a real gap
for whoever hosts it. Every other ACP client today runs an agent's
commands the way `bash -c` would.

gish already has the missing pieces as invariants:

- **Sandbox profiles** ([#21](https://github.com/blairham/gish/issues/21)).
  `gish acp` wraps every command the agent asks for in the same re-exec
  the `sandbox` builtin uses — `workspace` by default. `--profile none`
  is allowed and says so loudly, because a sandbox the user turned off
  is a decision and one that quietly did nothing is a lie.
- **A deadline on every call**, which is this codebase's oldest plugin
  rule.
- **Visibility.** Preview-before-execute is not available here — the
  agent is executing, not proposing — so every tool call prints as it
  happens. Hiding them is the one thing that would make this worse than
  a terminal.

## What it does not do

- **No `fs` capability.** Reading and writing files on an agent's behalf
  is a different trust decision from running a command you can see, and
  gish has nowhere to ask for it yet. The client advertises `false` and
  refuses the methods by name.
- **The inbound plugin declines the terminal capability outright.** `??`
  composes a command for you to read; nothing there may execute.
- **No v2.** It is a draft, and the draft says not to drop v1.

## The honest caveat

The user-facing version of this — "people are leaving zsh and fish
because AI agents assume bash" — is **not evidenced**. It rested on two
HN accounts, and a Reddit corpus check returned 0 of 827. What *is*
evidenced is that the execution substrate is unserved: `just-bash`
(Vercel) reached 4.77M npm downloads a month in eight months, OpenAI
forks and patches zsh's `exec.c` to intercept `execve`, and VS Code
shipped a `riskAssessment` feature that has an LLM guess what a command
does.

So this is built because the substrate is unserved. It is **not** a
marketing pillar ([#169](https://github.com/blairham/gish/issues/169)).

## Using it

```sh
# Host an agent; its commands run under the workspace sandbox profile.
gish acp claude-code-acp

# Stricter, for an agent you are letting explore.
gish acp --profile readonly claude-code-acp

# The inbound half: any ACP agent answers ?? and explain.
export GISH_AI_PROVIDER=gish-acp
export GISH_ACP_AGENT="claude-code-acp"     # or `gemini --acp`, …
```

Sources: [agentclientprotocol.com/protocol/terminals](https://agentclientprotocol.com/protocol/terminals),
[the v2 draft announcement](https://agentclientprotocol.com/announcements/acp-v2-draft),
[ACP in Copilot CLI](https://github.blog/changelog/2026-01-28-acp-support-in-copilot-cli-is-now-in-public-preview/).
