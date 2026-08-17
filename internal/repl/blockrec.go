package repl

import (
	"time"

	"github.com/blairham/gish/internal/blocks"
	"github.com/blairham/gish/internal/history"
)

// Recording a command's output alongside its history entry (#99 stage
// 3).
//
// The history entry is authoritative and is written whether or not
// capture is on; the block is an optional reference hung off it. That
// ordering matters: a failure to store output must never cost the user
// the record that the command ran.

// blockStore is the session's output store, nil when capture is off or
// the directory is unusable.
var blockStore *blocks.Store

// historyStore is the session's history, shared with the builtins that
// read it (fc, #60). Set once at startup like blockStore; nil where
// there is no history, which is a degradation those builtins report
// rather than crash on.
var historyStore *history.Store

// openBlockStore prepares output storage. A store that cannot be opened
// disables block recording silently — capture still mirrors to the
// screen, and history is unaffected.
func openBlockStore() *blocks.Store {
	s, err := blocks.OpenDefault()
	if err != nil {
		return nil
	}
	return s
}

// recordBlock stores a line's captured output and returns the reference
// to hang on its history entry. An empty ref means there was nothing to
// store, which is the ordinary case for a command that printed nothing
// and for every command while capture is off.
//
// Secrets are redacted inside the store, at write time, so there is no
// path by which unredacted output reaches disk — see blocks.Put.
func recordBlock(out []byte, truncated bool) string {
	if blockStore == nil || len(out) == 0 {
		return ""
	}
	ref, _, err := blockStore.Put(out, truncated)
	if err != nil {
		return "" // derived state: the entry is still worth writing
	}
	return string(ref)
}

// pruneBlocks enforces retention on exit, the same moment the session
// store prunes. Cheap, and the only point at which the shell knows it
// is finished.
func pruneBlocks() {
	if blockStore != nil {
		blockStore.Prune(time.Now())
	}
}
