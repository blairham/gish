package repl

import (
	"fmt"
	"io"
	"strings"

	huh "charm.land/huh/v2"

	"github.com/blairham/koi-shell/internal/manifest"
)

// `plugin browse` and the interactive `plugin add` (#90).
//
// #90 asked for this as a *zi* surface — "huh select over the registry,
// ice editing as a form". It is built on `plugin` instead, and that is
// a deliberate correction rather than a substitution: #108 ratified the
// declarative manifest as the configuration surface and demoted ice
// syntax to a migration-only spelling. A shiny new form that taught
// people to write ice modifiers would be onboarding them onto the thing
// we just deprecated.
//
// So the form edits the manifest's four knobs — source, kind, pin,
// lazy — and writes plugins.toml through the same path `plugin add`
// uses from the command line. Same file, same semantics, same result;
// the form is only a way of filling it in.

// starterPlugins is a short, opinionated list for the empty case:
// someone who just installed koi and types `plugin browse` has nothing
// to browse, and a blank screen is a worse answer than four suggestions.
//
// It is explicitly **not a registry**. koi does not host, index, or
// vouch for plugins, and pretending otherwise would be a promise we
// have no way to keep. Anything on GitHub works by typing owner/repo.
var starterPlugins = []struct{ source, what string }{
	{"zsh-users/zsh-autosuggestions", "history suggestions (koi has this natively — for parity while you switch)"},
	{"zsh-users/zsh-syntax-highlighting", "command-line highlighting (also native here)"},
	{"zsh-users/zsh-completions", "extra completion definitions"},
	{"ohmyzsh/ohmyzsh", "the plugin pile — lazy-load a path from it"},
}

// browsable reports whether an interactive form can run here. It reuses
// interactiveChooser's gate rather than re-deriving one: both ends of
// the terminal real and color-willing, so NO_COLOR, dumb terminals,
// pipes, and CI all get the plain listing instead. One gate for every
// styled surface is the reason TestHeadlessSurfacesEmitNoEscapes can
// cover them all.
func browsable(in io.Reader, out io.Writer) bool {
	return interactiveChooser(in, out) != nil
}

// runPluginBrowse is the interactive manager: toggle what loads, and
// add something new. It edits the manifest and reports what changed;
// nothing is written until the form is completed.
func runPluginBrowse(hc handlerIO, path string, m *manifest.Manifest) []string {
	if !browsable(hc.Stdin, hc.Stdout) {
		// No terminal to host a form. The plain listing is the honest
		// fallback and is what scripts and CI should see anyway.
		hc.Errf("plugin browse: needs an interactive terminal — showing the list instead\n")
		return listPlugins(hc, m)
	}

	if len(m.Plugins) == 0 {
		return browseEmpty(hc, path, m)
	}

	// Which entries load. A multi-select over the configured set is the
	// whole enable/disable surface in one screen, which is the thing a
	// list of `plugin enable X` commands is not.
	opts := make([]huh.Option[string], 0, len(m.Plugins))
	var on []string
	for _, p := range m.Plugins {
		label := p.Name()
		if detail := browseDetail(p); detail != "" {
			label += "  " + detail
		}
		opts = append(opts, huh.NewOption(label, p.Name()))
		if p.On() {
			on = append(on, p.Name())
		}
	}
	selected := on
	form := huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("plugins").
			Description("space toggles what loads; enter saves").
			Options(opts...).
			Value(&selected),
	)).WithInput(hc.Stdin).WithOutput(hc.Stdout)
	if err := form.Run(); err != nil {
		fmt.Fprintln(hc.Stdout, "plugin browse: nothing changed")
		return []string{"true"}
	}

	changed := applyEnabled(m, selected)
	if changed == 0 {
		fmt.Fprintln(hc.Stdout, "nothing changed")
		return []string{"true"}
	}
	if err := m.Save(path); err != nil {
		hc.Errf("plugin browse: %v\n", err)
		return []string{"false"}
	}
	fmt.Fprintf(hc.Stdout, "%d change(s) saved to %s\n", changed, displayPath(path))
	fmt.Fprintln(hc.Stdout, "enabled entries load on the next shell, or `plugin update` now")
	return []string{"true"}
}

// browseDetail summarizes an entry the way the listing does, so the
// form and the plain output describe the same thing.
func browseDetail(p manifest.Plugin) string {
	var parts []string
	if k := p.EffectiveKind(); k != manifest.KindPlugin {
		parts = append(parts, string(k))
	}
	if p.Pin != "" {
		parts = append(parts, "@"+p.Pin)
	}
	if p.Lazy != "" {
		parts = append(parts, "lazy "+p.Lazy)
	}
	return strings.Join(parts, " ")
}

// applyEnabled sets each entry's enabled flag from the selection and
// returns how many actually moved.
func applyEnabled(m *manifest.Manifest, selected []string) int {
	want := make(map[string]bool, len(selected))
	for _, name := range selected {
		want[name] = true
	}
	changed := 0
	for i := range m.Plugins {
		on := want[m.Plugins[i].Name()]
		if m.Plugins[i].On() == on {
			continue
		}
		enabled := on
		m.Plugins[i].Enabled = &enabled
		changed++
	}
	return changed
}

// browseEmpty is the first-run path: nothing configured yet, so offer
// the starter list rather than an empty screen.
func browseEmpty(hc handlerIO, path string, m *manifest.Manifest) []string {
	opts := make([]huh.Option[string], 0, len(starterPlugins)+1)
	for _, s := range starterPlugins {
		opts = append(opts, huh.NewOption(s.source+"  —  "+s.what, s.source))
	}
	opts = append(opts, huh.NewOption("something else (type owner/repo)", ""))

	var picked []string
	if err := huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("no plugins configured").
			Description("a starting point, not a registry — any owner/repo works").
			Options(opts...).
			Value(&picked),
	)).WithInput(hc.Stdin).WithOutput(hc.Stdout).Run(); err != nil {
		fmt.Fprintln(hc.Stdout, "plugin browse: nothing added")
		return []string{"true"}
	}

	var sources []string
	custom := false
	for _, p := range picked {
		if p == "" {
			custom = true
			continue
		}
		sources = append(sources, p)
	}
	if custom {
		var typed string
		if err := huh.NewForm(huh.NewGroup(
			huh.NewInput().Title("source").Description("owner/repo, or a URL").Value(&typed),
		)).WithInput(hc.Stdin).WithOutput(hc.Stdout).Run(); err == nil && strings.TrimSpace(typed) != "" {
			sources = append(sources, strings.TrimSpace(typed))
		}
	}
	if len(sources) == 0 {
		fmt.Fprintln(hc.Stdout, "nothing added")
		return []string{"true"}
	}

	added := 0
	for _, src := range sources {
		if m.Add(manifest.Plugin{Source: src}) {
			added++
		}
	}
	if err := m.Save(path); err != nil {
		hc.Errf("plugin browse: %v\n", err)
		return []string{"false"}
	}
	fmt.Fprintf(hc.Stdout, "added %d to %s\n", added, displayPath(path))
	fmt.Fprintln(hc.Stdout, "run `plugin update` to install them")
	return []string{"true"}
}

// handlerIO is the slice of interp.HandlerContext these functions need,
// so they can be tested with plain buffers.
type handlerIO struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// ErrLocation is the caller's interp.HandlerContext.ErrLocation,
	// carried so a diagnostic raised here says where it came from like
	// every other builtin's does (#611).
	ErrLocation string
}

// Errf writes a located diagnostic, mirroring
// interp.HandlerContext.Errf.
func (hc handlerIO) Errf(format string, a ...any) {
	fmt.Fprint(hc.Stderr, hc.ErrLocation+fmt.Sprintf(format, a...))
}

// listPlugins is the plain fallback: the same information, zero escape
// bytes, which is what the headless invariant requires.
func listPlugins(hc handlerIO, m *manifest.Manifest) []string {
	if len(m.Plugins) == 0 {
		fmt.Fprintln(hc.Stdout, "no plugins configured (plugin add owner/repo)")
		return []string{"true"}
	}
	for _, p := range m.Plugins {
		state := "on"
		if !p.On() {
			state = "off"
		}
		fmt.Fprintf(hc.Stdout, "%-3s %-40s %s\n", state, p.Name(), browseDetail(p))
	}
	return []string{"true"}
}
