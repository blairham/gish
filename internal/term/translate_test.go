package term

import (
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

func TestTranslateKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   uv.Event
		want Event
		ok   bool
	}{
		{
			name: "printable rune",
			in:   uv.KeyPressEvent{Code: 'a', Text: "a"},
			want: KeyEvent{Key: KeyRune, Rune: 'a'},
			ok:   true,
		},
		{
			name: "shifted rune drops shift",
			in:   uv.KeyPressEvent{Code: 'a', Text: "A", Mod: uv.ModShift},
			want: KeyEvent{Key: KeyRune, Rune: 'A'},
			ok:   true,
		},
		{
			name: "ctrl-a",
			in:   uv.KeyPressEvent{Code: 'a', Mod: uv.ModCtrl},
			want: KeyEvent{Key: KeyRune, Rune: 'a', Mod: ModCtrl},
			ok:   true,
		},
		{
			name: "alt-b",
			in:   uv.KeyPressEvent{Code: 'b', Mod: uv.ModAlt},
			want: KeyEvent{Key: KeyRune, Rune: 'b', Mod: ModAlt},
			ok:   true,
		},
		{
			name: "enter",
			in:   uv.KeyPressEvent{Code: uv.KeyEnter},
			want: KeyEvent{Key: KeyEnter},
			ok:   true,
		},
		{
			name: "alt-enter",
			in:   uv.KeyPressEvent{Code: uv.KeyEnter, Mod: uv.ModAlt},
			want: KeyEvent{Key: KeyEnter, Mod: ModAlt},
			ok:   true,
		},
		{
			name: "backspace",
			in:   uv.KeyPressEvent{Code: uv.KeyBackspace},
			want: KeyEvent{Key: KeyBackspace},
			ok:   true,
		},
		{
			name: "arrow up",
			in:   uv.KeyPressEvent{Code: uv.KeyUp},
			want: KeyEvent{Key: KeyUp},
			ok:   true,
		},
		{
			name: "ime multi-rune text becomes paste",
			in:   uv.KeyPressEvent{Text: "だよ"},
			want: PasteEvent{Text: "だよ"},
			ok:   true,
		},
		{
			name: "bracketed paste",
			in:   uv.PasteEvent{Content: "ls -la\n"},
			want: PasteEvent{Text: "ls -la\n"},
			ok:   true,
		},
		{
			name: "window size",
			in:   uv.WindowSizeEvent{Width: 120, Height: 40},
			want: ResizeEvent{Width: 120, Height: 40},
			ok:   true,
		},
		{
			name: "key release ignored",
			in:   uv.KeyReleaseEvent{Code: 'a'},
			want: nil,
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := translate(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}
