package editor_test

import (
	"context"
	"strings"
	"testing"

	"github.com/blairham/gish/internal/editor"
	"github.com/blairham/gish/internal/term"
)

func diagnoseConfig() editor.Config {
	return editor.Config{
		Prompt: "$ ",
		Diagnose: func(text string) []string {
			if strings.Contains(text, "rm $d") {
				return []string{"caution: unquoted $d"}
			}
			return nil
		},
	}
}

func TestDiagnosticsRenderBelowLine(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	events := append(typed("rm $d"), key(term.KeyEnter))
	ed := editor.New(&fakeTerm{events: events}, &out, diagnoseConfig())
	got, err := ed.ReadCommand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "rm $d" {
		t.Fatalf("diagnostics must never block acceptance: got %q", got)
	}
	if !strings.Contains(out.String(), "caution: unquoted $d") {
		t.Errorf("caution line not rendered: %q", out.String())
	}
}

func TestDiagnosticsClearWhenFixed(t *testing.T) {
	t.Parallel()

	// Quote the variable: the caution line must leave the final frame.
	var out strings.Builder
	events := append(typed(`rm $d`), typed("x")...) // "rm $dx" no longer matches
	events = append(events, key(term.KeyEnter))
	ed := editor.New(&fakeTerm{events: events}, &out, editor.Config{
		Prompt: "$ ",
		Diagnose: func(text string) []string {
			if text == "rm $d" {
				return []string{"caution: unquoted $d"}
			}
			return nil
		},
	})
	if _, err := ed.ReadCommand(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The warning appeared mid-edit, then the fixed buffer re-rendered
	// without it: the last occurrence must be followed by a clear.
	s := out.String()
	last := strings.LastIndex(s, "caution: unquoted $d")
	if last == -1 {
		t.Fatal("caution line never rendered")
	}
	if !strings.Contains(s[last:], "\x1b[") {
		t.Errorf("no repaint after the caution line went stale: %q", s[last:])
	}
}
