package complete

import "testing"

func TestStripExecutableExt(t *testing.T) {
	t.Parallel()

	const pathext = ".COM;.EXE;.BAT;.CMD"
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"git.exe", "git", true},
		{"BUILD.CMD", "BUILD", true},
		{"setup.Bat", "setup", true},
		{"readme.txt", "", false},
		{"noext", "", false},
	}
	for _, tt := range tests {
		got, ok := stripExecutableExt(tt.in, pathext)
		if got != tt.want || ok != tt.ok {
			t.Errorf("stripExecutableExt(%q) = %q, %v — want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
