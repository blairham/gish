package ice

import "testing"

func TestParseArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		key  string
		want string
	}{
		{"native quoted-stripped", []string{"wait1"}, "wait", "1"},
		{"native pick", []string{"pickinit.zsh"}, "pick", "init.zsh"},
		{"explicit equals", []string{"wait=1"}, "wait", "1"},
		{"bare flag", []string{"lucid"}, "lucid", ""},
		{"bare wait defaults empty", []string{"wait"}, "wait", ""},
		{"longest prefix wins", []string{"id-asauto"}, "id-as", "auto"},
		{"atpull percent atclone", []string{"atpull%atclone"}, "atpull", "%atclone"},
		{"from gh-r", []string{"fromgh-r"}, "from", "gh-r"},
		{"as program", []string{"asprogram"}, "as", "program"},
		{"surviving quotes trimmed", []string{`wait"2"`}, "wait", "2"},
		{"unknown key=value kept", []string{"mystery=42"}, "mystery", "42"},
		{"unknown bare kept", []string{"mystery"}, "mystery", ""},
		{"make with args", []string{"makeinstall PREFIX=/usr"}, "make", "install PREFIX=/usr"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ic, err := ParseArgs(tc.args)
			if err != nil {
				t.Fatalf("ParseArgs(%v): %v", tc.args, err)
			}
			if !ic.Has(tc.key) {
				t.Fatalf("ParseArgs(%v): missing key %q, got %v", tc.args, tc.key, ic.Map())
			}
			if got := ic.Get(tc.key); got != tc.want {
				t.Errorf("ParseArgs(%v)[%s] = %q, want %q", tc.args, tc.key, got, tc.want)
			}
		})
	}
}

func TestStringStableOrder(t *testing.T) {
	ic, err := ParseArgs([]string{"wait1", "lucid", "pickx.zsh"})
	if err != nil {
		t.Fatal(err)
	}
	want := `lucid pick"x.zsh" wait"1"`
	if got := ic.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
