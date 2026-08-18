package term

import (
	"context"
	"os"
	"strings"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

// escTimeout disambiguates a lone ESC keypress from the start of an
// escape sequence: if no continuation bytes arrive within it, the buffer
// is decoded as-is.
const escTimeout = 50 * time.Millisecond

// uvDecoder keeps ultraviolet's type out of term.go — the whole input
// API surface of the library stays confined to this file.
type uvDecoder = uv.EventDecoder

// Events implements Terminal with an owned read loop over ultraviolet's
// EventDecoder (the #1 decision: borrow the decoder, own the loop). Owning
// the loop is what makes type-ahead correct:
//
//   - Bytes are read in small chunks and decoded one event per consumer
//     hand-off, so input the user typed ahead of the next prompt is never
//     sitting in a library's internal buffer when the session ends.
//   - On cancel, undecoded bytes and the at-most-one undelivered event are
//     stashed on the TTY and replayed to the next Events session.
//   - Bytes never read from the kernel remain there for child processes —
//     the shell stops consuming stdin the moment ctx is canceled.
func (t *TTY) Events(ctx context.Context) (<-chan Event, error) {
	cr, err := uv.NewCancelReader(t.f)
	if err != nil {
		return nil, err
	}
	go func() {
		<-ctx.Done()
		cr.Cancel()
	}()

	reads := make(chan []byte)
	go func() {
		defer close(reads)
		for {
			b := make([]byte, 256)
			n, rerr := cr.Read(b)
			if n > 0 {
				select {
				case reads <- b[:n]:
				case <-ctx.Done():
					// Bytes already read from the kernel must not be
					// dropped: they are the next session's type-ahead.
					t.stashBytes(b[:n])
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	winch, stopWinch := notifyResize()
	out := make(chan Event) // unbuffered: decode only as fast as consumed
	go func() {
		defer close(out)
		defer stopWinch()
		t.decodeLoop(ctx, reads, winch, out)
	}()
	return out, nil
}

// stashBytes appends undelivered input for the next Events session.
func (t *TTY) stashBytes(b []byte) {
	t.pending = append(t.pending, b...)
}

//nolint:gocognit // the event loop is one state machine; splitting it obscures the states
func (t *TTY) decodeLoop(ctx context.Context, reads <-chan []byte, winch <-chan os.Signal, out chan<- Event) {
	buf := t.pending
	t.pending = nil
	queued := t.pendingEv
	t.pendingEv = nil

	var (
		pasting   bool
		paste     strings.Builder
		escWaited bool
	)

	// emit hands one event to the consumer; on cancel it stashes the
	// event (and the caller stashes remaining bytes) so nothing is lost.
	emit := func(ev Event) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			t.pendingEv = append(t.pendingEv, ev)
			return false
		}
	}

	bail := func() {
		if pasting && paste.Len() > 0 {
			t.pendingEv = append(t.pendingEv, PasteEvent{Text: paste.String()})
		}
		t.pending = append(append([]byte{}, buf...), t.pending...)
	}

	for _, ev := range queued {
		if !emit(ev) {
			bail()
			return
		}
	}

	for {
		// Decode everything that is unambiguous.
		for len(buf) > 0 {
			if isPartialEscape(buf) && !escWaited {
				break // wait for continuation bytes or the esc timeout
			}
			n, uev := t.dec.Decode(buf)
			if n == 0 {
				break
			}
			buf = buf[n:]
			escWaited = false

			switch uev.(type) {
			case uv.PasteStartEvent:
				pasting = true
				paste.Reset()
				continue
			case uv.PasteEndEvent:
				pasting = false
				if !emit(PasteEvent{Text: paste.String()}) {
					bail()
					return
				}
				continue
			}
			if pasting {
				appendPasted(&paste, uev)
				continue
			}
			ev, ok := translate(uev)
			if !ok {
				continue
			}
			if !emit(ev) {
				bail()
				return
			}
		}

		// Need more input (or the esc-timeout verdict).
		var escTimer <-chan time.Time
		if len(buf) > 0 && isPartialEscape(buf) && !escWaited {
			escTimer = time.After(escTimeout)
		}
		select {
		case <-ctx.Done():
			bail()
			return
		case <-escTimer:
			escWaited = true // decode the partial sequence as-is
		case <-winch:
			if w, h, err := t.Size(); err == nil {
				if !emit(ResizeEvent{Width: w, Height: h}) {
					bail()
					return
				}
			}
		case b, ok := <-reads:
			if !ok {
				// Input closed (EOF or cancel): drain what remains.
				if len(buf) > 0 {
					escWaited = true
					continue
				}
				bail()
				return
			}
			buf = append(buf, b...)
			escWaited = false
		}
	}
}

// isPartialEscape reports whether b could be the prefix of a longer
// escape sequence, so decoding should wait for continuation bytes.
func isPartialEscape(b []byte) bool {
	if len(b) == 0 || b[0] != 0x1b {
		return false
	}
	if len(b) == 1 {
		return true // lone ESC or a sequence's first byte
	}
	if b[1] != '[' && b[1] != 'O' {
		return false // ESC+x is a complete alt-chord
	}
	// CSI/SS3: complete once a final byte (0x40..0x7e) appears.
	for _, c := range b[2:] {
		if c >= 0x40 && c <= 0x7e {
			return false
		}
	}
	return true
}

// appendPasted folds a decoded event inside a bracketed paste into text.
func appendPasted(paste *strings.Builder, ev uv.Event) {
	k, ok := ev.(uv.KeyPressEvent)
	if !ok {
		return
	}
	switch {
	case k.Text != "":
		paste.WriteString(k.Text)
	case k.Code == uv.KeyEnter || k.Code == '\n' || k.Code == '\r',
		// A bare LF inside a paste decodes as Ctrl-J, and CR as Ctrl-M:
		// they are the same bytes. Dropping them — which is what
		// happened before, since neither carries Text — silently glued a
		// pasted heredoc into `cat <<'EOF'line oneline two`, and turned
		// a pasted backslash-continued command into one long argument.
		// A multi-line paste losing its line structure is the #161
		// abandonment cause in its purest form.
		k.Mod.Contains(uv.ModCtrl) && (k.Code == 'j' || k.Code == 'm'):
		paste.WriteByte('\n')
	case k.Code == uv.KeyTab:
		paste.WriteByte('\t')
	}
}

// specialKeys maps ultraviolet key codes to koi keys. Anything not here
// and not printable is dropped — the editor only binds what it knows.
var specialKeys = map[rune]Key{
	uv.KeyEnter:     KeyEnter,
	uv.KeyTab:       KeyTab,
	uv.KeyBackspace: KeyBackspace,
	uv.KeyDelete:    KeyDelete,
	uv.KeyUp:        KeyUp,
	uv.KeyDown:      KeyDown,
	uv.KeyLeft:      KeyLeft,
	uv.KeyRight:     KeyRight,
	uv.KeyHome:      KeyHome,
	uv.KeyEnd:       KeyEnd,
	uv.KeyPgUp:      KeyPageUp,
	uv.KeyPgDown:    KeyPageDown,
	uv.KeyEscape:    KeyEscape,
}

func translate(ev uv.Event) (Event, bool) {
	switch ev := ev.(type) {
	case uv.KeyPressEvent:
		return translateKey(uv.Key(ev))
	case uv.PasteEvent:
		return PasteEvent{Text: ev.Content}, true
	case uv.WindowSizeEvent:
		return ResizeEvent{Width: ev.Width, Height: ev.Height}, true
	}
	return nil, false
}

func translateKey(k uv.Key) (Event, bool) {
	var mod Mod
	if k.Mod.Contains(uv.ModCtrl) {
		mod |= ModCtrl
	}
	if k.Mod.Contains(uv.ModAlt) {
		mod |= ModAlt
	}
	if k.Mod.Contains(uv.ModShift) {
		mod |= ModShift
	}

	if special, ok := specialKeys[k.Code]; ok {
		return KeyEvent{Key: special, Mod: mod}, true
	}

	// Printable input without Ctrl/Alt: trust Text — it carries the
	// shifted/IME-composed characters, so drop the Shift modifier.
	if k.Text != "" && mod&(ModCtrl|ModAlt) == 0 {
		runes := []rune(k.Text)
		if len(runes) == 1 {
			return KeyEvent{Key: KeyRune, Rune: runes[0]}, true
		}
		// Multi-rune text (IME commit) inserts verbatim, like a paste.
		return PasteEvent{Text: k.Text}, true
	}

	// Modified rune keys (Ctrl-A, Alt-b, …): Code carries the base rune.
	if k.Code >= ' ' && k.Code < uv.KeyExtended {
		return KeyEvent{Key: KeyRune, Rune: k.Code, Mod: mod}, true
	}
	return nil, false
}
