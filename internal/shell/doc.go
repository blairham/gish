// Package shell holds koi's scripting substrate — the parser, the
// interpreter, the expansion engine, and the pattern matcher — lifted
// from mvdan.cc/sh and maintained here.
//
// # Why these packages live in koi
//
// interp and expand came first (#272), and for a reason that was about
// ownership rather than volume: every local fix koi has needed landed in
// one of them, and implementing "declare -i" required a new field on
// expand.Variable, which cannot be added to a struct in a package we do
// not own.
//
// syntax and pattern followed. The earlier version of this file argued
// they were worth more as dependencies than as copies, because upstream
// touched syntax 554 times in a year against zero local patches. What
// changed is the evidence: bash's own test suite (#211) says parse
// coverage is what dominates the score — 43% of the remaining diff was
// files stopped by a construct koi could not read — and each of those
// was a tokenizer rule (#423, #424, #428, #450) reachable from no seam
// koi had. The choice was to own the parser or to leave the largest
// measured gap permanently open.
//
// This is not a reimplementation. These packages arrived as upstream's
// code, tests and license included: ~20k lines of parser against its own
// 6k-line test suite, which now runs in this repository on every commit.
// bash compatibility is a long tail nobody derives from a spec — it is
// discovered by being bitten, and those tests are the record of the
// bites.
//
// # Provenance and attribution
//
// interp, expand and shinternal were lifted from github.com/blairham/sh
// (a fork of github.com/mvdan/sh) at commit
// 5967f857073ca1956bd532ab761ce9eaa139e455 — upstream master plus
// thirteen local fixes. syntax, pattern and fileutil were lifted from
// mvdan.cc/sh/v3 at v3.13.2-0.20260817215856-d6550df7ed8d, the commit
// go.mod pinned at the time, verbatim.
//
// Upstream's copyright and BSD-3-Clause license are retained verbatim in
// LICENSE alongside this file; that license permits this use, and the
// notice is the whole of the obligation.
//
// To review upstream work not carried here, from a clone of the fork:
//
//	git log 5967f857..upstream/master -- interp/ expand/ syntax/ pattern/
//
// # The seam to watch
//
// shinternal is a verbatim copy of upstream's internal package, and its
// sparse indexed-array helpers encode a representation shared with
// expand. Both sides of that seam live here, so they cannot drift apart
// — but a later re-sync from upstream must move them together or the
// mismatch will compile cleanly and misbehave at runtime.
//
// The same now applies to the parser: interp and expand consume
// syntax's node types directly, so a re-sync that takes a syntax change
// without the interp change that expects it will compile and be wrong.
package shell
