package p10k

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The dir settings that used to import cleanly and do nothing (#133).

// DIR_CLASSES is the most-used of the group and the one that is a
// safety feature: people use it to make production directories look
// different from everything else.
func TestDirClassesColorAndIcon(t *testing.T) {
	t.Parallel()

	ctx := sampleContext()
	cfg := Preset("lean")
	cfg.SetList("DIR_CLASSES", []string{
		filepath.Join("~", "dev", "*"), "WORK", "W",
		"*", "DEFAULT", "",
	})
	// A class name is a parameter state, so this is how it gets colored
	// — no new mechanism, the same three-step chain as everything else.
	cfg.Set("DIR_WORK_FOREGROUND", "red")

	rendered, ok := renderDir(cfg, ctx)
	if !ok {
		t.Fatal("dir rendered nothing")
	}
	if rendered.State != "WORK" {
		t.Errorf("state = %q, want WORK — the class did not match", rendered.State)
	}
	if rendered.Icon != "W" {
		t.Errorf("icon = %q, want the class's own", rendered.Icon)
	}
}

// Patterns are matched against the tilde form too, because that is what
// people write in their config.
func TestDirClassPatternForms(t *testing.T) {
	t.Parallel()

	home := filepath.Join(string(filepath.Separator), "fixture", "you")
	work := filepath.Join(home, "work", "prod")
	cfg := Preset("lean")
	cfg.SetList("DIR_CLASSES", []string{filepath.Join("~", "work", "*"), "PROD", "!"})

	class, icon := classifyDir(cfg, work, home)
	if class != "PROD" || icon != "!" {
		t.Errorf("classifyDir = %q/%q, want PROD/!", class, icon)
	}
	// A trailing * means "and everything below", which is what someone
	// writing ~/work/* means even though filepath.Match stops at a
	// separator.
	deep := filepath.Join(work, "a", "b", "c")
	if class, _ := classifyDir(cfg, deep, home); class != "PROD" {
		t.Errorf("deep path class = %q, want PROD", class)
	}
	if class, _ := classifyDir(cfg, filepath.Join(home, "play"), home); class != "" {
		t.Errorf("unrelated path matched: %q", class)
	}
}

// The hyperlink is zero-width, which is the property that lets it exist
// at all: the layout arithmetic must not notice it.
func TestDirHyperlinkCostsNoColumns(t *testing.T) {
	t.Parallel()

	ctx := sampleContext()
	plainCfg := Preset("lean")
	linkCfg := Preset("lean")
	linkCfg.Set("DIR_HYPERLINK", "true")

	plainOut := Render(plainCfg, ctx)
	linkOut := Render(linkCfg, ctx)
	if !strings.Contains(linkOut.Prompt, "\x1b]8;;file://") {
		t.Fatal("no OSC 8 link in the prompt")
	}
	for i, line := range strings.Split(plain(linkOut.Prompt), "\n") {
		want := strings.Split(plain(plainOut.Prompt), "\n")[i]
		if displayWidth(line) != displayWidth(want) {
			t.Errorf("line %d is %d columns with the link and %d without", i, displayWidth(line), displayWidth(want))
		}
	}
}

// A marked directory is the one people navigate by, so it survives
// shortening; TRUNCATE_BEFORE_MARKER drops everything above it.
func TestShortenFolderMarker(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repo := filepath.Join(root, "averylongprojectname")
	deep := filepath.Join(repo, "internal", "some", "package")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := sampleContext()
	ctx.Cwd, ctx.Home = deep, root
	ctx.Width = 30
	cfg := Preset("lean")
	cfg.Set("SHORTEN_FOLDER_MARKER", "go.mod")

	rendered, ok := renderDir(cfg, ctx)
	if !ok {
		t.Fatal("dir rendered nothing")
	}
	var text strings.Builder
	for _, s := range rendered.Spans {
		text.WriteString(s.Text)
	}
	if !strings.Contains(text.String(), "averylongprojectname") {
		t.Errorf("the marked directory was shortened away: %q", text.String())
	}

	cfg.Set("DIR_TRUNCATE_BEFORE_MARKER", "true")
	rendered, _ = renderDir(cfg, ctx)
	text.Reset()
	for _, s := range rendered.Spans {
		text.WriteString(s.Text)
	}
	if strings.HasPrefix(text.String(), "~") {
		t.Errorf("TRUNCATE_BEFORE_MARKER kept the components above the marker: %q", text.String())
	}
}

// The command line gets its columns first: a path is context, the thing
// being typed is the work.
func TestDirLeavesRoomForTheCommand(t *testing.T) {
	t.Parallel()

	ctx := sampleContext()
	ctx.Cwd = filepath.Join(ctx.Home, "one", "two", "three", "four", "five", "six")
	ctx.Width = 60
	cfg := Preset("lean")
	cfg.Set("DIR_MIN_COMMAND_COLUMNS_PCT", "80")

	render := func(cfg *Config) string {
		rendered, _ := renderDir(cfg, ctx)
		var text strings.Builder
		for _, s := range rendered.Spans {
			text.WriteString(s.Text)
		}
		return text.String()
	}
	// The comparison is against the same path with nothing reserved:
	// the strategy shortens as far as it can and no further, so the
	// assertion is that reserving columns *moves* it, not that it hits
	// an arbitrary number.
	reserved := render(cfg)
	if w, unreserved := displayWidth(reserved), displayWidth(render(Preset("lean"))); w >= unreserved {
		t.Errorf("dir took %d columns with 80%% reserved and %d without: %q", w, unreserved, reserved)
	}
}

// A setting that is stored and not acted on has to be visible as such,
// or the user cannot tell it from one that works.
func TestUnhonouredSettingsAreNamed(t *testing.T) {
	t.Parallel()

	cfg := Preset("lean")
	if got := cfg.UnhonouredSettings(); len(got) != 0 {
		t.Errorf("a stock preset reported ignored settings: %v", got)
	}
	cfg.Set("SHORTEN_STRATEGY", "truncate_to_unique")
	got := cfg.UnhonouredSettings()
	if len(got) != 1 || !strings.Contains(got[0], "truncate_to_unique") {
		t.Errorf("UnhonouredSettings = %v, want the strategy named with its reason", got)
	}
}
