package repl

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// runP10kScript drives the p10k command through the real read-eval loop
// with every path that could touch real state redirected: the rc file,
// the config directory, and stdin (which selects the line frontend of
// the chooser, so the wizard is driven by the same keys the interactive
// select returns).
func runP10kScript(t *testing.T, stdin, src string) (stdout, stderr string, dir string) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("KOI_RC", filepath.Join(dir, "koirc"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")

	var out, errOut strings.Builder
	err := RunReader(t.Context(), strings.NewReader(src), "test",
		interp.StdIO(strings.NewReader(stdin), &out, &errOut))
	if err != nil {
		t.Fatalf("script failed: %v\nstdout: %s\nstderr: %s", err, out.String(), errOut.String())
	}
	return out.String(), errOut.String(), dir
}

func TestP10kList(t *testing.T) {
	out, _, _ := runP10kScript(t, "", "p10k list\n")
	for _, want := range []string{"lean", "rainbow", "robbyrussell", "dir", "vcs", "prompt_char"} {
		if !strings.Contains(out, want) {
			t.Errorf("p10k list missing %q: %q", want, out)
		}
	}
}

func TestP10kShowReportsResolvedState(t *testing.T) {
	out, _, _ := runP10kScript(t, "", "p10k show\n")
	for _, want := range []string{"preset", "lean", "left", "dir vcs", "config"} {
		if !strings.Contains(out, want) {
			t.Errorf("p10k show missing %q: %q", want, out)
		}
	}
}

func TestP10kShowNamesUnimplementedElements(t *testing.T) {
	// An element with no implementation renders as nothing, which is
	// indistinguishable from "this tool isn't active here" unless we say
	// so. Ask for one and check it is named.
	// public_ip rather than battery: battery is implemented now (#132),
	// and the network segments are the ones that stay out until there is
	// a way to compute them off the prompt path.
	out, _, _ := runP10kScript(t, "",
		"POWERLEVEL9K_RIGHT_PROMPT_ELEMENTS='time public_ip'\np10k show\n")
	if !strings.Contains(out, "not yet") || !strings.Contains(out, "public_ip") {
		t.Errorf("unimplemented element not reported: %q", out)
	}
}

func TestP10kPresetWritesConfigAndActivates(t *testing.T) {
	out, _, dir := runP10kScript(t, "", "p10k preset rainbow\necho theme=$KOI_THEME\n")
	if !strings.Contains(out, "saved rainbow") {
		t.Errorf("no confirmation: %q", out)
	}
	// Configuring the prompt is one step: the file is written and the
	// theme is on, in the same breath.
	if !strings.Contains(out, "theme=p10k") {
		t.Errorf("theme not activated: %q", out)
	}

	conf := filepath.Join(dir, "config", "koi", "p10k.conf")
	data, err := os.ReadFile(conf)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if !strings.Contains(string(data), "DIR_BACKGROUND") {
		t.Errorf("rainbow settings not in the file: %s", data)
	}
	// And it persisted, so the next shell agrees.
	rc, err := os.ReadFile(filepath.Join(dir, "koirc"))
	if err != nil || !strings.Contains(string(rc), "KOI_THEME=p10k") {
		t.Errorf("theme not persisted to the rc: %q %v", rc, err)
	}
}

func TestP10kPresetRejectsUnknownName(t *testing.T) {
	out, errOut, dir := runP10kScript(t, "", "p10k preset nonsense\ntrue\n")
	if !strings.Contains(errOut, "no preset") {
		t.Errorf("no error for an unknown preset: %q %q", out, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "config", "koi", "p10k.conf")); err == nil {
		t.Error("a rejected preset should write nothing")
	}
}

func TestP10kImport(t *testing.T) {
	src := t.TempDir()
	zsh := filepath.Join(src, ".p10k.zsh")
	content := `() {
  typeset -g POWERLEVEL9K_LEFT_PROMPT_ELEMENTS=(dir newline prompt_char)
  typeset -g POWERLEVEL9K_DIR_FOREGROUND=201
  typeset -g POWERLEVEL9K_VCS_CONTENT_EXPANSION='${$((my_git_formatter(1)))+${my_git_format}}'
}
`
	if err := os.WriteFile(zsh, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Quote the path: a Windows temp directory is full of backslashes,
	// and unquoted they are escapes to the parser — the import then gets
	// a mangled path and quietly does nothing.
	out, _, dir := runP10kScript(t, "", "p10k import '"+zsh+"'\necho theme=$KOI_THEME\n")
	if !strings.Contains(out, "imported") {
		t.Errorf("no import summary: %q", out)
	}
	// The one setting that is shell code must be named, not swallowed.
	if !strings.Contains(out, "VCS_CONTENT_EXPANSION") {
		t.Errorf("unsupported setting not reported: %q", out)
	}
	if !strings.Contains(out, "theme=p10k") {
		t.Errorf("import did not activate the theme: %q", out)
	}

	data, err := os.ReadFile(filepath.Join(dir, "config", "koi", "p10k.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "DIR_FOREGROUND = 201") {
		t.Errorf("imported setting not written: %s", data)
	}
	// The preset underneath fills the gaps, so an import never leaves a
	// half-configured prompt.
	if !strings.Contains(string(data), "PROMPT_CHAR_OK_FOREGROUND") {
		t.Errorf("preset defaults missing from the written config: %s", data)
	}
}

func TestP10kImportMissingFile(t *testing.T) {
	_, errOut, _ := runP10kScript(t, "", "p10k import /nonexistent/.p10k.zsh\ntrue\n")
	if !strings.Contains(errOut, "p10k:") {
		t.Errorf("missing file not reported: %q", errOut)
	}
}

// TestP10kConfigureWizard drives the whole walkthrough through the line
// frontend: look, glyphs, three settings, then confirm.
func TestP10kConfigureWizard(t *testing.T) {
	answers := "classic\ny\ntrue\nalways\ny\n"
	out, _, dir := runP10kScript(t, answers, "p10k configure\n")

	if !strings.Contains(out, "Which look?") {
		t.Fatalf("wizard did not start: %q", out)
	}
	if !strings.Contains(out, "preview:") {
		t.Errorf("wizard did not preview the result: %q", out)
	}
	if !strings.Contains(out, "saved") {
		t.Errorf("wizard did not save: %q", out)
	}

	data, err := os.ReadFile(filepath.Join(dir, "config", "koi", "p10k.conf"))
	if err != nil {
		t.Fatal(err)
	}
	conf := string(data)
	for _, want := range []string{"TRANSIENT_PROMPT = always", "PROMPT_ADD_NEWLINE = true"} {
		if !strings.Contains(conf, want) {
			t.Errorf("answer not saved (%s): %s", want, conf)
		}
	}
}

// TestP10kInstantPromptIsExplained pins the one upstream feature this
// port declines to implement. Accepting the setting silently would leave
// someone believing a prompt cache is in play; erroring on it would
// break every imported config. Say so instead.
func TestP10kInstantPromptIsExplained(t *testing.T) {
	out, _, _ := runP10kScript(t, "", "POWERLEVEL9K_INSTANT_PROMPT=quiet\np10k show\n")
	if !strings.Contains(out, "INSTANT_PROMPT") || !strings.Contains(out, "nothing to cache") {
		t.Errorf("instant prompt not explained: %q", out)
	}
	// And it must stay quiet when nobody asked for it.
	plain, _, _ := runP10kScript(t, "", "p10k show\n")
	if strings.Contains(plain, "INSTANT_PROMPT") {
		t.Errorf("unasked-for note: %q", plain)
	}
}

func TestP10kConfigureAbortSavesNothing(t *testing.T) {
	// Answer everything, then decline at the confirmation.
	answers := "lean\ny\ntrue\noff\noff\nn\n"
	out, _, dir := runP10kScript(t, answers, "p10k configure\n")
	if !strings.Contains(out, "nothing saved") {
		t.Errorf("abort not reported: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "config", "koi", "p10k.conf")); err == nil {
		t.Error("declining at the end still wrote a config")
	}
}

func TestP10kConfigureAsciiStripsGlyphs(t *testing.T) {
	// Answering "the glyphs did not render" must produce a prompt with
	// no powerline glyphs anywhere — that answer exists precisely
	// because the user is looking at boxes.
	answers := "rainbow\nn\nfalse\noff\noff\ny\n"
	_, _, dir := runP10kScript(t, answers, "p10k configure\n")

	data, err := os.ReadFile(filepath.Join(dir, "config", "koi", "p10k.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, glyph := range []string{"", "", "", "", "╭", "╰"} {
		if strings.Contains(string(data), glyph) {
			t.Errorf("ascii answer still wrote glyph %q", glyph)
		}
	}
}

// The tests above all drive the command as `p10k`, which is now the
// alias (#184). Leaving them spelled that way is deliberate: the whole
// existing suite doubles as the regression test that the alias keeps
// working. The tests below cover the new name and the one behavior that
// is genuinely per-spelling — which name the command calls itself.

func TestPromptAndP10kAreTheSameCommand(t *testing.T) {
	viaPrompt, _, _ := runP10kScript(t, "", "prompt list\n")
	viaP10k, _, _ := runP10kScript(t, "", "p10k list\n")
	if viaPrompt != viaP10k {
		t.Errorf("alias diverged from the command:\n prompt: %q\n p10k:   %q", viaPrompt, viaP10k)
	}
	if !strings.Contains(viaPrompt, "lean") {
		t.Errorf("`prompt list` did not run: %q", viaPrompt)
	}
}

func TestPromptUsageEchoesInvokedName(t *testing.T) {
	for _, name := range []string{"prompt", "p10k"} {
		out, _, _ := runP10kScript(t, "", name+" --help\n")
		if !strings.Contains(out, "usage: "+name+" [") {
			t.Errorf("%s --help did not answer in its own vocabulary: %q", name, out)
		}
		// The other spelling must not leak into the help text: someone
		// who typed `p10k` should not be told to run `prompt preset`.
		other := "prompt"
		if name == "prompt" {
			other = "p10k"
		}
		if strings.Contains(out, other+" configure") {
			t.Errorf("%s --help mentions %q: %q", name, other, out)
		}
	}
}

func TestPromptErrorsEchoInvokedName(t *testing.T) {
	for _, name := range []string{"prompt", "p10k"} {
		_, errOut, _ := runP10kScript(t, "", name+" preset nonsense\ntrue\n")
		if !strings.HasPrefix(errOut, name+":") {
			t.Errorf("%s error not prefixed with the invoked name: %q", name, errOut)
		}
	}
}

func TestPromptIsCompletableAndSuggestable(t *testing.T) {
	// A command nothing can complete is half-shipped, and the
	// did-you-mean suggester reads the same list.
	for _, name := range []string{"prompt", "p10k"} {
		if !slices.Contains(callHandlerCommands, name) {
			t.Errorf("%q is not in callHandlerCommands, so Tab cannot complete it", name)
		}
	}
}
