# Structured tracing: KOI_TRACE_JSON

`set -x` is the universal script debugger and its output is unparseable by
design — interleaved with the script's own stderr, no exit codes, no
timing. koi controls the interpreter, so it can offer what bash
structurally cannot: an opt-in trace that leaves stdout and stderr
untouched.

```sh
KOI_TRACE_JSON=/tmp/trace.jsonl koi deploy.sh
```

Every simple command the session executes appends one JSON object:

```json
{"src":"deploy.sh","line":12,"col":1,"cmd":"curl -fsS $URL","expanded":["curl","-fsS","https://example.com"],"exit":22,"started_unix_ms":1755640000000,"duration_ms":840}
```

- `cmd` is the command **as written** — `$URL` stays `$URL` — so the trace
  greps against the script that produced it. `expanded` is the argv it
  actually ran with.
- `src`/`line` follow execution the way `BASH_SOURCE` does: a command in a
  sourced library names the library, a command in a function carries the
  function's name in `func`.
- `started_unix_ms`/`duration_ms`/`exit` use the history JSONL's field
  names, so the two streams join.

## Rules

- **Read once, at session start.** Exporting the variable mid-run changes
  child shells, never the running session.
- **Invisible to scripts.** No `set -o` name, no `$-` letter, nothing in
  the environment a bash-compatible script doesn't already see. `set -x`
  and this trace are independent.
- **Follows execution everywhere** — functions, subshells, pipeline
  stages, `source`, `eval`. (Ambient history deliberately stops at the
  sourcing line; a trace that did that would miss the command that
  failed.)
- **v1 granularity is the simple command.** Bare assignments,
  `((…))`/`[[…]]` headers and function entry/exit are not traced —
  and may grow later without breaking consumers, since the stream is
  one self-describing object per line.
- **Never the reason a command fails.** An unopenable trace path costs one
  warning at startup; a full disk during the run costs trace lines, never
  the command being traced.
- **The trace file is not scrubbed.** History rejects secret-bearing
  command lines; a trace names its file explicitly and records
  expanded argv, which is the point of a debugging trace — treat the file
  accordingly.

Bash-compatible `set -x` fidelity (PS4 expansion, `BASH_XTRACEFD`) is a
separate, planned track — deliberately not folded into this trace.
