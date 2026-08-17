# The Fig completion corpus: a spike (#170)

**Verdict: adopt the static half, mechanically, once. Do not adopt the
corpus as a dependency, and do not run its generators.**

## What is actually there

`withfig/autocomplete`, measured 2026-08:

| | |
| --- | --- |
| stars | 25,219 |
| forks | 5,435 |
| licence | MIT |
| last push | **2025-05-05** (15 months stale) |
| archived | no — dormant rather than closed |
| top-level entries in `src/` | 735 |
| repo size | ~38 MB |

Provenance is what the issue hoped: MIT, ~400 contributors, and the
specs predate the AWS acquisition. Fig was acquired August 2023, access
ended 2024-09-01, and the successor (Amazon Q CLI) went closed-source
with one commit in 2026 and parent EOS 2027-04-30. The specs were the
moat and were discarded.

## The finding that decides it

**The specs are TypeScript programs, not data.** A spec is a module that
imports helpers, defines `generators` with `script` and `postProcess`
functions, and returns candidates by parsing the output of commands it
runs. From the sample:

| spec | names | descriptions | generator sites |
| --- | --- | --- | --- |
| `git.ts` | 1,860 | 1,588 | 82 |
| `docker.ts` | 1,645 | 951 | 44 |
| `kubectl.ts` | 608 | 480 | 13 |
| `curl.ts` | 363 | 226 | 1 |
| `npm.ts` | 284 | 224 | 8 |
| `ls.ts` | 47 | 44 | 0 |
| `jq.ts` | 41 | 29 | 0 |

So there are two corpora inside this one:

1. **A large static tree** — subcommands, flags, argument templates,
   and above all **descriptions**, which is the thing carapace does not
   have and the thing that makes a completion list readable. This is
   data wearing a TypeScript costume.
2. **A small dynamic layer** — 1–82 generators per spec that shell out
   (`git branch`, `docker ps`) and post-process the output in
   JavaScript.

Adopting (2) means a JavaScript runtime on the completion path. That
contradicts the line drawn in #112 — *native for the keystroke, prompt
and cd path; delegate everything else* — and it would put a Node process
inside an 80 ms budget. It is not a close call.

Adopting (1) is a one-time mechanical translation: parse the TypeScript,
keep the object literals, drop the functions, emit a declarative form
gish reads. The generators' *targets* are mostly things gish or a plugin
can produce natively anyway — branches, containers, contexts — and the
`gish-git`, `gish-aws` and carapace paths already do exactly that.

## How it would merge with carapace (#9)

They are complements, and the conflict rule follows from what each is:

- **carapace** is ~1,000 CLIs through one engine — *breadth*, generated,
  uniformly shallow, and always current with its own releases.
- **Fig specs** are hand-written *depth*: descriptions, per-argument
  suggestions, and the flag spellings a human actually checked.

Rule: **a Fig-derived candidate wins on description, carapace wins on
existence.** If carapace lists a flag the imported spec does not, the
flag is real and the spec is 15 months old. If both list it and only one
has a description, take the description. That ordering degrades in the
right direction as the imported data ages.

## Staleness, and why it is survivable

Fifteen months of drift is real: `docker` and `kubectl` move, `git`
barely does. The mitigation is the merge rule above — carapace is the
liveness source, the corpus is the prose source — plus never presenting
an imported description for a flag no current CLI reports.

## What a real adoption would cost

- A TypeScript-literal-to-declarative converter, run once, checked in as
  data. Roughly: parse with a TS-aware parser, walk for object
  literals, discard nodes containing function expressions, emit a
  compact format. The output is data, so it is reviewable and diffable.
- Lazy loading per command, keyed the same way bash-completion's own
  loader is (#159): a shell must not pay for 735 specs to complete `ls`.
- Attribution. MIT requires it; the honest form is a NOTICE naming the
  project and its contributors, not a line in a README.

## Is there a user base to invite?

5,435 forks and no upstream for fifteen months is a lot of stranded
work, and a maintained fork is a distribution channel as much as a
feature. But it is also the classic trap: inheriting a 38 MB corpus
means inheriting its issue tracker, and the reason it is dormant is that
maintaining per-CLI specs by hand does not pay for itself — which is
precisely why carapace generates instead.

**So: take the data, not the project.** Import once, attribute properly,
let carapace stay the liveness source, and do not promise the corpus a
future gish cannot fund.

## Not scheduled

This spike answers "should we", not "when". The completion engine
already merges core providers with plugins behind an 80 ms budget, and
the import above is a bounded piece of work that can land whenever it is
worth more than what it displaces. It is deliberately not on the v1
path.
