// Package shell holds koi's scripting substrate: the interpreter and the
// expansion engine, lifted from mvdan.cc/sh and maintained here.
//
// # Why these packages live in koi
//
// koi consumes the parser (mvdan.cc/sh/v3/syntax) and the pattern matcher
// (.../pattern) as ordinary upstream dependencies, and always should. Upstream
// touched syntax 554 times in the last year against zero local patches: that
// package is worth far more as a dependency than as a copy, and every fix
// there — including the fuzz-found panics that would crash koi's highlighter,
// which reparses the line on every keystroke — arrives for free.
//
// interp and expand are different. Every local fix koi has needed landed in
// one of them, and expand is here for a specific reason: implementing
// "declare -i" required a new field on expand.Variable, and a field cannot be
// added to a struct in a package we do not own. interp and expand are also the
// packages koi intends to keep diverging in — see the bash-suite scoreboard,
// where the great majority of failing files parse correctly and then behave
// differently, which is by definition a runtime gap rather than a parse gap.
//
// # Provenance and attribution
//
// Lifted from github.com/blairham/sh (a fork of github.com/mvdan/sh) at commit
// 5967f857073ca1956bd532ab761ce9eaa139e455, which is upstream master plus the
// thirteen local fixes. Upstream's copyright and BSD-3-Clause license are
// retained verbatim in LICENSE alongside this file; that license permits this
// use, and the notice is the whole of the obligation.
//
// To review upstream work not carried here:
//
//	git log 5967f857..master -- interp/ expand/
//
// # The seam to watch
//
// shinternal is a verbatim copy of upstream's internal package, and its sparse
// indexed-array helpers encode a representation shared with expand. Both sides
// of that seam now live here, so they cannot drift apart — but a later
// re-sync from upstream must move them together or the mismatch will compile
// cleanly and misbehave at runtime.
package shell
