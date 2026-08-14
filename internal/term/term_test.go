package term_test

import (
	"errors"
	"os"
	"testing"

	"github.com/blairham/gish/internal/term"
)

// fake implements term.Terminal and records raw-mode transitions.
type fake struct {
	raw          bool
	restoreCalls int
	enterErr     error
}

func (f *fake) EnterRaw() (func() error, error) {
	if f.enterErr != nil {
		return nil, f.enterErr
	}
	f.raw = true
	return func() error {
		f.restoreCalls++
		f.raw = false
		return nil
	}, nil
}

func (f *fake) Size() (int, int, error)        { return 80, 24, nil }
func (f *fake) ReadEvent() (term.Event, error) { return nil, term.ErrNoDecoder }

func TestWithRawRestoresOnReturn(t *testing.T) {
	t.Parallel()

	f := &fake{}
	err := term.WithRaw(f, func() error {
		if !f.raw {
			t.Error("fn ran outside raw mode")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithRaw: %v", err)
	}
	if f.raw {
		t.Error("terminal left in raw mode after return")
	}
	if f.restoreCalls != 1 {
		t.Errorf("restoreCalls = %d, want 1", f.restoreCalls)
	}
}

func TestWithRawRestoresOnError(t *testing.T) {
	t.Parallel()

	f := &fake{}
	wantErr := errors.New("boom")
	err := term.WithRaw(f, func() error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if f.raw {
		t.Error("terminal left in raw mode after error")
	}
}

func TestWithRawRestoresOnPanic(t *testing.T) {
	t.Parallel()

	f := &fake{}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected panic to propagate")
			}
		}()
		_ = term.WithRaw(f, func() error { panic("editor bug") })
	}()
	if f.raw {
		t.Error("terminal left in raw mode after panic")
	}
	if f.restoreCalls != 1 {
		t.Errorf("restoreCalls = %d, want 1", f.restoreCalls)
	}
}

func TestWithRawEnterFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("not a tty")
	f := &fake{enterErr: wantErr}
	ran := false
	err := term.WithRaw(f, func() error { ran = true; return nil })
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if ran {
		t.Error("fn ran despite EnterRaw failure")
	}
}

func TestTTYEnterRawRejectsNonTerminal(t *testing.T) {
	t.Parallel()

	// A pipe is not a terminal; raw mode must fail cleanly, never
	// silently pretend.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	if _, err := term.NewTTY(r).EnterRaw(); err == nil {
		t.Error("EnterRaw on a pipe succeeded, want error")
	}
}

func TestTTYReadEventUnimplemented(t *testing.T) {
	t.Parallel()

	_, err := term.NewTTY(os.Stdin).ReadEvent()
	if !errors.Is(err, term.ErrNoDecoder) {
		t.Errorf("err = %v, want ErrNoDecoder", err)
	}
}
