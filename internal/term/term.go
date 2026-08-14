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
	"errors"
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
	// ReadEvent blocks until the next input event. Event decoding lands
	// with the line editor (#2); until then implementations may return
	// an error.
	ReadEvent() (Event, error)
}

// ErrNoDecoder is returned by ReadEvent until event decoding lands (#2).
var ErrNoDecoder = errors.New("term: event decoding lands with the line editor (#2)")

// TTY implements Terminal on a real terminal device.
type TTY struct {
	f *os.File
}

// NewTTY wraps an open terminal device, typically os.Stdin.
func NewTTY(f *os.File) *TTY {
	return &TTY{f: f}
}

func (t *TTY) EnterRaw() (func() error, error) {
	fd := int(t.f.Fd())
	state, err := xterm.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	restore := func() error {
		var rerr error
		once.Do(func() {
			rerr = xterm.Restore(fd, state)
		})
		return rerr
	}
	return restore, nil
}

func (t *TTY) Size() (int, int, error) {
	return xterm.GetSize(int(t.f.Fd()))
}

func (t *TTY) ReadEvent() (Event, error) {
	return nil, ErrNoDecoder
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
