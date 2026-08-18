//go:build unix

package main

import (
	"strings"
	"testing"
)

// Blocks, end to end through the surface a person uses (#99 stages 3-4).
//
// Everything under blocks was tested except the thing it exists for.
// internal/blocks covers the store, internal/capture covers the ring,
// capture_e2e covers stage 2's invariants (a pager still pages, a
// redirect still redirects), and internal/repl/blockscmd_test covers
// three pure helpers — blockIndex, firstMatchingLine, blockDetail.
//
// Nothing asserted that running a command records a block, that `blocks
// list` lists it, that `blocks show` prints what it printed, or that
// `blocks search` finds a command by its *output* — which is the one
// thing history structurally cannot do, and the whole claim of the
// feature. That is the same blind spot #118 turned up: helpers verified,
// the rendered surface taken on trust.
//
// Capture is on via the environment for the reason capture_e2e gives:
// typing `config blocks on` costs a prompt redraw per keystroke, and
// config's own tests already prove the command writes the setting.

func TestBlocksRecordsListsShowsAndSearches(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{Env: []string{"KOI_BLOCKS=on"}})
	s.waitForPrompt()

	// An external child, because capture substitutes an external child's
	// stdout: a builtin never becomes a child, so `printf` would go
	// straight to the terminal and be recorded nowhere. The marker is
	// split so the terminal's echo of the command cannot satisfy a wait
	// for the output.
	s.runLine(`/bin/echo blocks-payload-marker`)

	// The listing has to show the command that produced output.
	out := s.runProbe(`blocks list`, "blocks-payload")
	if !strings.Contains(out, "/bin/echo") {
		t.Errorf("blocks list does not show the command:\n%s", out)
	}

	// show prints what the command printed. This is the assertion that
	// distinguishes a recorded block from a listed history entry.
	out = s.runProbe(`blocks show 1`, "blocks-payload-marker")
	if !strings.Contains(out, "blocks-payload-marker") {
		t.Errorf("blocks show did not print the captured output:\n%s", out)
	}

	// And the claim history cannot make: find a command by what it
	// printed. The search term appears nowhere in the command line.
	out = s.runProbe(`blocks search payload-marker`, "/bin/echo")
	if !strings.Contains(out, "/bin/echo") {
		t.Errorf("blocks search did not find the command by its output:\n%s", out)
	}
}

// Capture is opt-in, and off must mean off: a shell that recorded output
// without being asked would be the trust problem the whole design avoids.
func TestBlocksRecordsNothingWhenOff(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{Env: []string{"KOI_BLOCKS=off"}})
	s.waitForPrompt()

	s.runLine(`/bin/echo unrecorded-payload`)

	// The listing must not carry it. Waiting on the D mark rather than on
	// a marker, because the expected output here is *nothing*, and a wait
	// for something absent can only ever time out.
	s.buf.Reset()
	s.send("blocks list\r")
	s.waitFor(commandDone)
	if got := s.plain(); strings.Contains(got, "unrecorded-payload") {
		t.Errorf("capture off still recorded output:\n%s", got)
	}
}
