# Structured data: design exploration

Structured pipelines are the most-praised shell *idea* of the last
decade. nushell's `ls | where size > 10mb | sort-by modified` was itself
a 295-point HN story, and its 1477-point launch thread was about typed
data killing awk/sed/jq incantations.

But nushell's adoption ceiling — more stars than fish, roughly a quarter
of the installs — points at the trick nobody has pulled off: **structured
data without breaking bash muscle memory or script reuse.** That is the
gap [#104](https://github.com/blairham/koi-shell/issues/104) asks about, and
this document answers its questions before any code is written.

**Status: exploration. Nothing here is committed.** The point of writing
it now is that one of the answers came out differently than the issue
assumed, and that changes what a v1 would look like.

## The finding that reshapes the design

The issue states the constraint as: *"must be plain command syntax
(builtins + args), zero new grammar — the bash parser is the contract.
`ps aux | from auto | where %cpu > 50` should parse as ordinary commands
today."*

That example does parse as ordinary commands today. It just doesn't mean
what it looks like. Run against the current shell:

```
$ koi -c 'echo a | where %cpu > 50'
"where": executable file not found in $PATH
$ ls
50
```

`>` is a redirect. `where` receives one argument (`%cpu`), and the shell
creates **a file named `50`**. No error, no warning — a silent
filesystem side effect from a line that reads like a comparison.

This is not a detail to work around later. It is the whole tension:

> "Zero new grammar" and "nushell's comparison syntax" cannot both be
> true, because `>`, `<`, and `|` are already the most load-bearing
> characters in shell grammar.

Any design that ignores this ends up quietly introducing new grammar —
at which point the bash-parser-is-the-contract promise, which is the
*entire* differentiator over nushell, is gone.

## The answer POSIX already shipped

Shell solved this exact problem in the 1970s, for exactly this reason.
`test` could not use `>` for comparison either, so it took words:

```sh
[ 5 -gt 3 ]        # -gt, -lt, -eq, -ne, -ge, -le
```

Those operators are already in every shell user's fingers, and they
parse as ordinary words today — verified:

```
$ koi -c 'printf "[%s]" %cpu -gt 50'
[%cpu][-gt][50]
```

So the structured verbs should speak `test`'s vocabulary, not nushell's:

```sh
ps aux | from auto | where %cpu -gt 50 | sort-by %mem --desc | to table
```

Every token is an ordinary word. No new grammar, no quoting, no silent
redirect, and the operator names are ones a shell user already knows.
This is strictly better than the two alternatives:

| option | verdict |
| --- | --- |
| `where '%cpu > 50'` | parses fine, but it is a mini-language inside a string — awk/jq by another name, which is the thing the feature claims to kill |
| `where %cpu > 50` | silently creates a file; unusable |
| **`where %cpu -gt 50`** | **plain words, familiar operators, no grammar change** |

The cost is that it does not look like nushell. That is the right trade:
looking like nushell is worth nothing if you have nushell's problem.

## Where structure lives

OS pipes carry bytes. Structure therefore lives **in the shell**,
between builtins, and the boundary is explicit:

- `from json|csv|tsv|auto` parses bytes into an in-shell value space at
  the edge of a pipeline.
- A small verb set operates on values: `where`, `select`, `sort-by`,
  `first`, `last`, `length`.
- `to json|csv|table` serializes back. At a TTY with no explicit `to`,
  the default rendering is a table.

The rule that keeps the promise: **any structured value that reaches a
pipe to an external command serializes back to text.** Structure is
never mandatory, text-stream universality is never broken, and
`... | grep foo` keeps working at any point in a pipeline. A structured
pipeline that a coreutils command can't consume is a bug in this design,
not a limitation the user has to learn.

## What this is not

- **Not a new scripting language.** No new grammar, no types in scripts,
  nothing on a script's critical path. `koi -c` stays POSIX-clean
  (docs/compat.md is the contract).
- **Not a coreutils replacement.** `ls`, `ps`, and `df` keep working
  exactly as they do; `from auto` parses *their* output rather than
  replacing them.
- **Not mandatory.** Someone who never types `from` sees no difference.

## Open questions a v1 would still have to answer

1. **`from auto` needs a parser corpus.** The jc project already parses
   ~200 command outputs, and its output is JSON. Making it the
   `from auto` backend via a plugin fits the delegate line (#112) and
   avoids koi maintaining parsers for `ps` across four platforms. Not
   yet verified: whether jc's coverage and licensing suit this, and
   whether shelling out per pipeline is fast enough.
2. **Column naming.** `%cpu` is what `ps` prints; `size` is what `ls`
   means. Whether field names come from the source verbatim or are
   normalized is a UX decision with no obvious right answer.
3. **Where the value space lives.** `mvdan.cc/sh`'s interpreter has no
   notion of non-string values, so the values would live beside it, keyed
   by pipeline. That plumbing is the real implementation cost and needs a
   spike before any estimate is credible.
4. **Does the table renderer belong to #90's ui package?** Almost
   certainly yes — `to table` is exactly the lipgloss table already used
   by `plugins` and `zi list`, and it must degrade to plain columns under
   the same `ui.Enabled` gate.

## Recommendation

Worth building, **after** v1, and only with the `-gt` vocabulary above.
The feature's whole claim is "structured data that doesn't cost you
bash", and a design that silently creates files named `50` does not
have that claim. Sequencing it after v1 also means the compat scoreboard
(#101) and the benchmark suite (#102) exist to prove the substrate
didn't regress when it lands.

If it is ever cut, the honest reason to state is that the eight-tool
native stack already covers the jq/awk slot for most people, and this is
the most expensive remaining idea on the list.
