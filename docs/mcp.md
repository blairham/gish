# MCP: the shell as a context provider

An agent harness reconstructs shell state by grepping rc files and
firing probe commands — `alias`, `type`, `jobs`, `set -o` — then parsing
the output back apart. koi already knows all of it, live. `config mcp on`
serves it over the Model Context Protocol, read-only.

```sh
config mcp on          # in a koi session; off by default
```

Point an MCP client at `koi mcp`:

```json
{"mcpServers": {"koi": {"command": "koi", "args": ["mcp"]}}}
```

## Tools

| tool | answers |
| --- | --- |
| `aliases` | alias definitions, name → replacement text |
| `functions` | function names; with `name`, that function's source |
| `options` | every supported `set -o`/`shopt` option and its live state |
| `jobs` | the job table: id, pgid, state, command |
| `cwd` | working directory and the `pushd`/`popd` stack |
| `history_search` | history with metadata — command, cwd, exit status, duration |

## Why it is core, not a plugin

Every service in `proto/koi/plugin/v1` is host→plugin: in go-plugin the
host is the gRPC client, so a plugin answers questions and cannot ask
them. A plugin therefore *cannot* be this — it has no channel to the
shell's state. The server runs in the shell's own process, beside the
state it reports, and `koi mcp` is a stdio↔socket proxy because harnesses
spawn a command and speak over its pipes.

This is the third role on the agent edge, beside ACP's two
(`docs/acp.md`): ACP inbound is a plugin, ACP outbound (executing an
agent's commands) is core, and MCP is core because of where the state
lives rather than because it executes anything.

## Rules

- **Read-only, structurally.** No tool executes anything, so the "a
  plugin may never hold an exec channel" invariant has nothing here to
  hold.
  Execution stays with ACP's terminal serve mode.
- **Off by default**, re-read at each prompt, like `config blocks`.
- **Snapshot at the prompt**, not live: the interpreter is not safe for
  concurrent use, and between prompts is when a session's state is
  stable. A tool asked before the first prompt says so. History is the
  exception — its store carries its own lock, so it answers live.
- **Same-user only.** The socket lives in a 0700 directory under
  `$XDG_RUNTIME_DIR` (or a per-uid temp directory), named per session
  pid. That directory *is* the access policy; there is no auth layer.
- **Sockets are not secret-scrubbed the way history is.** History never
  records secret-bearing commands, so `history_search` inherits
  that guarantee — but an alias or function body can contain a token
  (`alias deploy='TOKEN=… ssh …'`) and nothing strips it. Turning the
  server on exposes your shell's definitions to whatever you pointed at
  the socket.
- **`koi mcp` attaches to the newest live session**, or to
  `KOI_MCP_SOCKET` when set (an instruction, not a candidate). Sockets
  that refuse a connection belonged to dead sessions and are removed in
  passing.
