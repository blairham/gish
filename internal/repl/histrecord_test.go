package repl

import (
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/interp"
	"github.com/blairham/koi-shell/internal/shell/syntax"
)

// resetSessionHistory gives a test the package-level history list to
// itself and puts it back afterwards. Not parallel-safe by design: the
// list is one session's, and the test binary is the session.
func resetSessionHistory(t *testing.T) {
	t.Helper()
	histMu.Lock()
	savedList, savedMut, savedBase, savedLast := histList, histMutated, histBase, histAmbientLast
	histList, histMutated, histBase, histAmbientLast = nil, true, 0, false
	histMu.Unlock()
	t.Cleanup(func() {
		histMu.Lock()
		histList, histMutated, histBase, histAmbientLast = savedList, savedMut, savedBase, savedLast
		histMu.Unlock()
	})
}

// parseForHistory feeds src to a recorder the way the shell does: a line
// at a time, as it is read.
func parseForHistory(t *testing.T, src string) *historyRecorder {
	t.Helper()
	sr := interp.NewScriptReader(strings.NewReader(src), "test")
	rec := newHistoryRecorder(sr.Source, func(string) string { return "" })
	for stmts, err := range sr.Lines() {
		if err != nil {
			t.Fatal(err)
		}
		rec.addLine(stmts)
	}
	return rec
}

// The entry text is raw source joined by bash's rules, every case here
// measured against bash 5.3 (`history` output compared byte for byte).
func TestHistoryRecorderRendersAsBashRecords(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []string
	}{
		// One line, two statements, one entry — the unit is the line.
		{"statements share their line", "echo a; echo b\n", []string{"echo a; echo b"}},
		// The tab survives: bash joins source lines, it does not
		// pretty-print, and this case is exactly why the recorder
		// slices source instead of rendering the tree.
		{
			"loop keeps its tab",
			"for x in one two three\ndo\n\t:\ndone\n",
			[]string{"for x in one two three; do \t:; done"},
		},
		{
			"then joins with a space",
			"if true; then\n  echo hi\nfi\n",
			[]string{"if true; then   echo hi; fi"},
		},
		{
			"backslash continuation is spliced",
			"echo one \\\ntwo\n",
			[]string{"echo one two"},
		},
		{"and-and joins with a space", "true &&\nfalse\n", []string{"true && false"}},
		{"pipe joins with a space", "echo x |\ncat\n", []string{"echo x | cat"}},
		{
			"case keywords join with spaces",
			"case a in\na) : ;;\nesac\n",
			[]string{"case a in a) : ;; esac"},
		},
		{"brace group", "{\necho grp\n}\n", []string{"{ echo grp; }"}},
		// A heredoc keeps its newlines and a trailing one after the
		// delimiter — measured byte-for-byte via `history -w`.
		{
			"heredoc keeps newlines and a tail",
			"cat <<HD\nbody\nHD\n",
			[]string{"cat <<HD\nbody\nHD\n"},
		},
		{
			"open quote keeps its newline",
			"echo 'two\nlines'\n",
			[]string{"echo 'two\nlines'"},
		},
		{
			"array assignment collapses to a space",
			"a=(1\n2)\n",
			[]string{"a=(1 2)"},
		},
		{
			"command substitution keeps its newline",
			"x=$(echo a\necho b)\n",
			[]string{"x=$(echo a\necho b)"},
		},
		// `done; echo tail` folds into the loop's entry: the trailing
		// command starts on the group's last line.
		{
			"trailing command joins the compound's entry",
			"for x in 1\ndo\n:\ndone; echo tail\n",
			[]string{"for x in 1; do :; done; echo tail"},
		},
		{
			"each line is its own entry",
			"echo a\necho b\n",
			[]string{"echo a", "echo b"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := parseForHistory(t, tc.src)
			var got []string
			for _, g := range rec.groups {
				got = append(got, rec.render(g))
			}
			if len(got) != len(tc.want) {
				t.Fatalf("entries = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// A statement from a different parse — the rc file an interactive `-c`
// session sources, a profile — must be ignored, not sliced against the
// wrong source. bash agrees on the outcome: rc commands are not
// recorded.
func TestHistoryRecorderIgnoresForeignStatements(t *testing.T) {
	resetSessionHistory(t)
	rec := parseForHistory(t, "echo mine\n")
	foreign, err := syntax.NewParser().Parse(strings.NewReader("echo theirs\n"), "rc")
	if err != nil {
		t.Fatal(err)
	}
	rec.record(foreign.Stmts[0])
	if entries := historyEntries(); len(entries) != 0 {
		t.Errorf("a foreign statement was recorded: %q", entries)
	}
}

func TestHistoryAppendFilters(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(name string) string { return vars[name] }
	}

	t.Run("ignorespace is a space, not any whitespace", func(t *testing.T) {
		resetSessionHistory(t)
		e := env(map[string]string{"HISTCONTROL": "ignoreboth"})
		historyAppendFiltered(" spaced", true, e)
		historyAppendFiltered("\ttabbed", true, e)
		if got := historyEntries(); len(got) != 1 || got[0] != "\ttabbed" {
			t.Errorf("entries = %q, want just the tabbed line", got)
		}
	})

	t.Run("ignoredups collapses consecutive only", func(t *testing.T) {
		resetSessionHistory(t)
		e := env(map[string]string{"HISTCONTROL": "ignoredups"})
		for _, s := range []string{"a", "a", "b", "a"} {
			historyAppendFiltered(s, true, e)
		}
		if got := strings.Join(historyEntries(), ","); got != "a,b,a" {
			t.Errorf("entries = %q, want a,b,a", got)
		}
	})

	t.Run("HISTIGNORE patterns and ampersand", func(t *testing.T) {
		resetSessionHistory(t)
		e := env(map[string]string{"HISTIGNORE": "&:history*:fc*"})
		for _, s := range []string{"echo x", "echo x", "history -w f", "fc -l", "echo y"} {
			historyAppendFiltered(s, true, e)
		}
		if got := strings.Join(historyEntries(), ","); got != "echo x,echo y" {
			t.Errorf("entries = %q, want echo x,echo y", got)
		}
	})

	t.Run("HISTIGNORE matches the whole entry", func(t *testing.T) {
		resetSessionHistory(t)
		e := env(map[string]string{"HISTIGNORE": "ls"})
		historyAppendFiltered("ls -l", true, e)
		if got := historyEntries(); len(got) != 1 {
			t.Errorf("a prefix match filtered a longer entry: %q", got)
		}
	})

	t.Run("erasedups removes earlier copies", func(t *testing.T) {
		resetSessionHistory(t)
		e := env(map[string]string{"HISTCONTROL": "erasedups"})
		for _, s := range []string{"a", "b", "a"} {
			historyAppendFiltered(s, true, e)
		}
		if got := strings.Join(historyEntries(), ","); got != "b,a" {
			t.Errorf("entries = %q, want b,a", got)
		}
	})
}

// The two trim shapes, both measured: an append onto a full list drops
// one and keeps the numbering (base += 1), while a multi-entry drop —
// which only an assignment shrinking HISTSIZE between appends produces —
// renumbers the survivors one lower (base += drop-1).
func TestHistorySizeTrimNumbering(t *testing.T) {
	t.Run("steady stifle keeps numbers", func(t *testing.T) {
		resetSessionHistory(t)
		e := func(name string) string {
			if name == "HISTSIZE" {
				return "2"
			}
			return ""
		}
		for _, s := range []string{"a", "b", "c", "d"} {
			historyAppendFiltered(s, true, e)
		}
		if got := strings.Join(historyEntries(), ","); got != "c,d" {
			t.Fatalf("entries = %q, want c,d", got)
		}
		if base := historyBase(); base != 2 {
			t.Errorf("base = %d, want 2 (c is entry 3)", base)
		}
	})

	t.Run("assignment shrink renumbers one lower", func(t *testing.T) {
		resetSessionHistory(t)
		size := ""
		e := func(name string) string {
			if name == "HISTSIZE" {
				return size
			}
			return ""
		}
		for _, s := range []string{"a", "b", "c", "HISTSIZE=2"} {
			historyAppendFiltered(s, true, e)
		}
		size = "2" // the assignment has now run; the next append trims
		historyAppendFiltered("history", true, e)
		if got := strings.Join(historyEntries(), ","); got != "HISTSIZE=2,history" {
			t.Fatalf("entries = %q", got)
		}
		// bash displays these as 3 and 4, not 4 and 5 (measured).
		if base := historyBase(); base != 2 {
			t.Errorf("base = %d, want 2", base)
		}
	})
}
