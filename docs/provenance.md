# provenance

Where koi's scripting substrate came from, what the attribution
obligation actually is, and why buying our way out of it would cost
more than it saves. This page exists because "can we stop crediting
mvdan?" is a question with an appealing answer that the repo's own
evidence refutes — and because the refutation is more useful than the
answer would have been.

`internal/shell/doc.go` is the record of *what* was lifted and from
which commit. This page is the record of *whether we should try to
undo it*. The short version: no, and the reason is bash compatibility,
not licensing.

## What is vendored, and how far it has drifted

All six packages now live in-tree; `mvdan.cc/sh` is no longer a module
dependency at all. Measured against `../sh` at `d6550df7`, ignoring
tests and testdata:

| package | source LOC | test LOC | lines changed vs upstream |
| --- | ---: | ---: | ---: |
| `interp` | 13,138 | 10,597 | 6,763 |
| `syntax` | 9,847 | 10,940 | **323** |
| `expand` | 4,435 | 520 | 1,824 |
| `shinternal` | 671 | 0 | 64, plus two files that are ours |
| `pattern` | 476 | 378 | **0** |
| `fileutil` | 84 | 67 | **0** |

51,153 lines total, of which 22,502 are tests. `interp` is genuinely
ours now — around half its lines differ. `syntax` is not: 323 changed
lines against 9,847 is upstream's parser with a set of tokenizer and
grammar rules added, which is exactly what the lift was for (#423,
#424, #428, #450, and every #277 fix since). `shinternal` gained
`brackets.go` and `extglob.go`, both koi's, and 60 changed lines in
`pattern.go`; `pattern` and `fileutil` are still byte-for-byte
upstream.

These numbers move every time a #277 stopper is fixed — re-measure
against `../sh` at `d6550df7` rather than trusting the table.

106 files across seven packages — `cmd/koi`, `internal/repl`,
`internal/builtins`, `internal/jobs`, `internal/mcpserve`,
`internal/migrate`, `internal/pluginhost` — import these directly and
name upstream's types: `interp.Runner` in 136 places,
`interp.HandlerContext` in 104, `syntax.NewParser` in 30, and 43
distinct `syntax` identifiers.

## The obligation is one file

Upstream is BSD-3-Clause. The whole of it: retain the copyright notice
and license text, and do not claim upstream's endorsement. It does not
propagate to koi's MIT license, does not restrict commercial use, does
not require publishing changes, and does not constrain the roadmap.

It is already discharged, correctly — `internal/shell/LICENSE` verbatim,
`internal/shell/doc.go` naming the exact lifted commits, and the
per-file headers retained (~55 files carrying Daniel Martí's copyright,
two carrying Andrey Nering's).

There is nothing to fix here. If the discomfort is legal, it is
misplaced.

## The three exits, and why two of them are closed

**Relicensing** is not viable. Ten years of contributors and at least
two copyright holders in the tree; every one of them would have to
agree.

**Rewriting from scratch** is the only real exit, and it comes with a
constraint that is easy to miss: it cannot be done by editing the
vendored copy. A modified copy remains a derivative work however
thoroughly it is rewritten line by line, and our own git history
documents the derivation. Independence requires new code written from
the specification — POSIX XCU and the bash manual, both public, since
grammar and behaviour are not copyrightable — in new files, with the
vendored tree deleted when the last consumer moves off.

**Keeping it** is the third exit, and it is the right one. The rest of
this page is why.

## Why the rewrite is the wrong trade

`internal/shell/doc.go` already contains the argument, made for a
different purpose:

> bash compatibility is a long tail nobody derives from a spec — it is
> discovered by being bitten, and those tests are the record of the
> bites.

Turn that on the licensing question and it decides it. The 22,378 lines
of tests are upstream's copyrighted work too, as is
`syntax/testdata`. A rewrite that keeps them to prove compatibility has
not escaped anything — it is still distributing his code, and now with
a weaker claim to the parser that satisfies them. A rewrite that
discards them starts bash compatibility over from zero.

That is the trade in full: koi would give up its single defensible
asset — the one AGENTS.md names as the historical gate, the thing
docs/compat.md and `make bash-suite` exist to measure — in exchange for
deleting a copyright notice that costs nothing. The #211 evidence says
parse coverage dominates the remaining diff; a spec-derived parser
would move that number in the wrong direction for months.

The repo already knows how to reason about this. `make bash-suite`
never commits bash's test files because every one carries a GPLv3
header and vendoring them would relicense an MIT repo by accident
(AGENTS.md). That is the same reasoning applied to a case where the
answer went the other way, and it is the reason to trust this one:
the licensing hygiene here is deliberate, not accidental.

## What a rewrite would cost, if it is ever revisited

Recorded so the estimate does not have to be redone, not as a plan:

- **`pattern` + `fileutil`** — 560 lines, zero divergence, both thin
  and spec-defined (glob-to-regexp translation, file type checks).
  Days. The only genuinely cheap piece.
- **`expand`** — 4,207 lines. Parameter, brace and arithmetic
  expansion, all specified. Medium, and the `Integer` field on
  `expand.Variable` that forced the original lift (#272) is ours
  already.
- **`interp`** — 12,905 lines but the most tractable of the large
  ones, because half of it is already koi's: job control, trace and
  history hooks, sandbox profiles, plugin call handlers.
- **`syntax`** — last, largest, and the one that matters. A new AST is
  a new API, and 106 files name the current one. This is where a
  decade of edge cases lives and where the rewrite would actually be
  judged.

The oracle problem is the harder half. `internal/compat/corpus.go` is
the right seed — it is ours, and its provenance notes are facts rather
than copied expression, which is fine. Growing it into a differential
harness against real `bash`/`sh` binaries is worth doing on its own
merits regardless of this question, because it is the only compat
evidence that does not depend on upstream's suite.

A facade package hiding `syntax` and `interp` behind koi-owned types
was considered as a first step and is **not** recommended. It only
pays if a swap is actually coming; a facade over an AST is awkward,
and the 106 consumers would be churned for a migration the rest of
this page argues against.

## The real cost is the re-sync seam, not the license

What deserves attention is the thing `doc.go` flags at the end: `interp`
and `expand` consume `syntax`'s node types directly, and `shinternal`'s
sparse indexed-array helpers encode a representation shared with
`expand`. A re-sync that takes one side without the other compiles
cleanly and is wrong at runtime.

With `syntax` at a few hundred diverged lines, re-syncing it is cheap and
still worth doing periodically — upstream's fuzz-found panic fixes
matter to us specifically, because `internal/repl/highlight.go`
reparses the line on every keystroke, so an upstream parser panic is a
koi crash. That was true when the parser was a dependency and it did
not stop being true when it became a copy; the difference is that the
fixes no longer arrive for free. This is the actual ongoing cost of
the lift, and it is a maintenance cost, not a legal one.

## Open recommendation

The positioning language overclaims. AGENTS.md:20 says correctly that
the substrate is "lifted from `mvdan.cc/sh`", but the same sentence
calls it "koi's own substrate", and the framing at AGENTS.md:11 pulls
the same way. For `interp` that is defensible; for `syntax`, at 323
changed lines out of 9,847, it is not.

The fix is wording, not code: say koi *maintains* its substrate in-tree
rather than that it *is* koi's own. That is both true and very nearly
as strong a claim — in-tree ownership is what closed the #211 parse
gaps, and it is the part that a competitor cannot replicate by filing
an upstream issue.
