// Package term abstracts the platform TTY behind gish's own small
// interface, so the line editor (#2) depends on this contract rather than
// on any particular terminal library.
//
// This is the swappable-plumbing boundary decided in #1: raw-mode entry
// and restore are backed by golang.org/x/term today; key/event decoding
// (ANSI sequences, kitty keyboard protocol, bracketed paste, Windows
// ConPTY) plugs in behind ReadEvent when the editor lands — the planned
// decoder is charmbracelet's ultraviolet/x/ansi layer, with a hand-rolled
// decoder as the documented fallback. Nothing outside this package may
// import a terminal library directly.
package term

import (
	"context"
	"fmt"
	"os"
	"sync"

	xterm "golang.org/x/term"
)

// Key identifies a non-rune key in a KeyEvent.
type Key int

const (
	// KeyRune means the event carries a printable character in Rune.
	KeyRune Key = iota
	KeyEnter
	KeyTab
	KeyBackspace
	KeyDelete
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPageUp
	KeyPageDown
	KeyEscape
)

// Mod is a bitmask of key modifiers.
type Mod uint8

const (
	ModCtrl Mod = 1 << iota
	ModAlt
	ModShift
)

// Event is a single terminal input event.
type Event interface{ isEvent() }

// KeyEvent is a keypress. Rune is meaningful only when Key == KeyRune.
type KeyEvent struct {
	Key  Key
	Rune rune
	Mod  Mod
}

// ResizeEvent reports a terminal size change (SIGWINCH or ConPTY resize).
type ResizeEvent struct {
	Width  int
	Height int
}

// PasteEvent is a bracketed paste: its text is inserted verbatim, never
// interpreted as keystrokes — pasting must not trigger keybindings.
type PasteEvent struct {
	Text string
}

func (KeyEvent) isEvent()    {}
func (ResizeEvent) isEvent() {}
func (PasteEvent) isEvent()  {}

// Terminal is the line editor's view of the TTY.
type Terminal interface {
	// EnterRaw switches the terminal to raw mode. The returned restore
	// function is idempotent and safe to call from any goroutine; callers
	// must arrange for it to run on every exit path (see WithRaw).
	EnterRaw() (restore func() error, err error)
	// Size reports the current terminal dimensions in cells.
	Size() (width, height int, err error)
	// Events starts decoding input and streams events until ctx is
	// canceled, at which point the channel is closed. Canceling ctx
	// stops the underlying read: the shell must never keep consuming
	// stdin once a child process owns the terminal.
	Events(ctx context.Context) (<-chan Event, error)
}

// TTY implements Terminal on a real terminal device.
type TTY struct {
	f   *os.File // input side; also the raw-mode fd
	out *os.File // output side, for mode-toggle sequences

	// Type-ahead carried between Events sessions: input the loop
	// consumed but the previous session's consumer never received.
	pending   []byte
	pendingEv []Event

	dec uvDecoder
}

// NewTTY wraps the two sides of a terminal, typically stdin and stdout.
func NewTTY(in, out *os.File) *TTY {
	return &TTY{f: in, out: out}
}

// IsTerminal reports whether f is a terminal device. Callers use this to
// choose between the line editor and the plain piped-input loop.
func IsTerminal(f *os.File) bool {
	return xterm.IsTerminal(int(f.Fd()))
}

func (t *TTY) EnterRaw() (func() error, error) {
	fd := int(t.f.Fd())
	state, err := xterm.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	// Bracketed paste: pasted text arrives delimited instead of as
	// keystrokes, so a pasted newline can never trigger accept-line.
	fmt.Fprint(t.out, "\x1b[?2004h")
	var once sync.Once
	restore := func() error {
		var rerr error
		once.Do(func() {
			fmt.Fprint(t.out, "\x1b[?2004l")
			rerr = xterm.Restore(fd, state)
		})
		return rerr
	}
	return restore, nil
}

func (t *TTY) Size() (int, int, error) {
	return xterm.GetSize(int(t.f.Fd()))
}

// WithRaw runs fn with t in raw mode and guarantees the terminal is
// restored on every exit path — normal return, error, or panic. This is
// correctness rule #1 for the interactive shell: never leave the user's
// terminal broken.
func WithRaw(t Terminal, fn func() error) (err error) {
	restore, err := t.EnterRaw()
	if err != nil {
		return err
	}
	defer func() {
		if rerr := restore(); rerr != nil && err == nil {
			err = rerr
		}
	}()
	return fn()
}
