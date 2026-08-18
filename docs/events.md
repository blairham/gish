# ShellEvents: event-triggered automations

Split out of [#34](https://github.com/blairham/koi-shell/issues/34) as its own
idea: plugins that react to what happens in a session — `cd` into a repo
and get the toolchain setup suggested, a lockfile changes and the install
command is offered, a command fails twice and `explain` is proposed.

**Status: contract defined, host not wired.** `proto/koi/plugin/v1/events.proto`
exists and `CAPABILITY_EVENTS` is allocated; nothing subscribes yet. That
ordering is deliberate and matches how the rest of this package was
built — the shape of a plugin contract is far harder to change once
plugins exist than before, so the contract is designed first and the
internals grow around it.

## Why this is the dangerous one

Every other tier-2 service answers a question the shell asked.
`CompletionProvider` is asked for candidates; `EnvProvider` is asked for
a diff; `ThemeProvider` is asked for a prompt. The host is in control of
when, and a plugin that misbehaves costs its own feature.

ShellEvents inverts that: the plugin is *told* things and gets to speak
unprompted. That is what makes it useful, and it is exactly why the
constraints below are not negotiable. A hook system is how shells
historically became slow and unpredictable — the reason koi notices
directory changes natively instead of asking users to `eval "$(... hook
bash)"` is that hooks bolted onto a prompt are a performance and
correctness hazard.

## The three rules

### 1. No exec channel, ever

A plugin proposes; the shell decides. This is the same line
[#111](https://github.com/blairham/koi-shell/issues/111) drew for the agent
builtin — **koi hosts other people's agents rather than being one** —
and it is precisely why orchestration cannot move behind the plugin
boundary without weakening the invariant.

`Proposal` has no field that causes execution. `suggested_command` lands
in the editor buffer for the user to read and edit, the #20 posture, and
the host owns quoting: a suggestion is *data*, never buffer text. That
closes the "a completion became code execution" class of bug for this
service too.

The `rationale` field is required by convention rather than by the
protocol, for a human reason: a suggestion nobody can evaluate gets
either blindly accepted or permanently ignored, and both are bad.

### 2. The host never blocks on a subscriber

Events are fire-and-forget into a bounded buffer, drop-oldest. A plugin
that stops reading loses events; it never slows the prompt.

This forced the RPC shape. In go-plugin the **host is the gRPC client**
and the plugin is the server, so a host→plugin push needs the host
sending on a stream. And a proposal arrives when a plugin is ready, not
as a reply to one event. So:

```proto
rpc Subscribe(stream ShellEvent) returns (stream Proposal);
```

Bidirectional. A request/response RPC per event would have made the host
wait on a plugin for every `cd`, which rule 2 forbids outright.

Proposals carry `event_seq`, so one answering a superseded event can be
dropped — a suggestion for the directory you just left is worse than no
suggestion. Same discipline as every other request in this package.

### 3. Events carry allowlisted, scrub-safe data only

An event stream is a new place for a secret to leak, so it is designed
not to carry one.

`EVENT_KIND_COMMAND_DONE` carries the command's **first word** — `git`,
not `git push --force origin main`. The argument list is where tokens,
private paths, and hostnames live, and a plugin reacting to "a git
command failed" does not need them. Environment is the allowlisted
subset, exactly as `EnvDiffRequest` carries it.

This is the #10/#12/#20 posture applied unchanged, and it is cheaper to
hold now than to retrofit: a plugin ecosystem that has learned to expect
full command lines cannot be told later that it can't have them.

## What a v1 would still have to decide

1. **Where subscription is declared.** The proto currently lets a plugin
   send `watch_paths` in a `Proposal`, so it can adjust what it watches
   as it learns where it is. Whether *event kinds* should also be
   declared that way, or in `DescribeResponse`, is open. Describe is
   simpler; in-stream is more flexible; doing both would be worst.
2. **File watching is the expensive part.** `koi-git` already runs an
   fsnotify watcher, so the machinery exists — but a per-plugin watch
   set, with plugins coming and going, is a different problem from one
   watcher for one repo. This is the piece to prototype before
   estimating anything.
3. **Proposal rate limiting.** A plugin that proposes on every event
   turns the prompt into a billboard. Probably a cap per event and a
   dedupe on identical notices, but the honest answer is that this needs
   a real plugin to tune against.
4. **Does a proposal survive the next prompt?** A lockfile-changed
   suggestion is still valid thirty seconds later; a
   command-failed suggestion probably is not. Likely a per-kind
   lifetime, decided when there is something real to observe.

## Prior art worth not repeating

- **zsh's `precmd`/`preexec`**: the reason "my shell got slow" is a
  genre. Anything a plugin does here must be unable to delay the prompt,
  which rule 2 enforces structurally rather than by asking politely.
- **direnv's hook**: a good feature whose install step (`eval "$(direnv
  hook bash)"`) is a permanent tax on every shell start and a source of
  ordering bugs. koi notices `cd` natively; that is the whole point of
  #12, and ShellEvents should never reintroduce a hook install.
- **PowerShell's event subsystem**: rich, and almost nobody uses it —
  a caution that the vocabulary should stay small enough to learn in one
  sitting.
