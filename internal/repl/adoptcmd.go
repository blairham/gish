package repl

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/blairham/koi-shell/internal/manifest"
)

// `koi adopt` — a team's koi setup, applied in one command (#209).
//
// The acquisition loop koi lacks is the group one: what shell you use
// is partly a group choice, and every adopter today converts alone. A
// `.koi.toml` checked into a project repo turns each adopter into a
// distribution channel — a teammate runs `koi adopt` and has the
// team's settings and plugins, layered *under* their own config.
//
// The payload splits by blast radius, and the split is the design:
//
//   - settings: names from `config`'s closed vocabulary, most with
//     enumerated values. Data validated against a vocabulary koi owns —
//     reviewable at a glance, impossible to weaponize beyond "your
//     prompt looks different".
//   - plugins: `plugins.toml` entries verbatim (#108) — third-party
//     code, but code that announces itself as such and rides the
//     manifest's own pin machinery.
//   - aliases and functions: deliberately NOT in v1. Arbitrary shell
//     code that silently shadows commands is the tier where "one
//     command" and "you reviewed this" genuinely conflict; excluded on
//     purpose rather than discovered later.
//
// Adoption is an explicit act, not an ambient one: it applies until
// reverted, like LazyVim's kickstart, not per-directory like direnv —
// koi already has the per-directory shape where it belongs (#12). The
// applied settings live in a fragment sourced *before* the user's rc,
// so last-write-wins means anything the user set already beats the
// repo, with no merge policy to reason about. Revert deletes the
// fragment and removes the plugin entries adoption added — one
// command, same as everything else (#212's exit-cost rule).

const adoptUsage = `usage: koi adopt [--yes]     apply the repo's .koi.toml
       koi adopt --revert    undo a previous adoption
       koi adopt --status    show what is adopted here

A .koi.toml carries two things: [settings] using ` + "`config`" + `'s names and
values, and [[plugins]] entries in plugins.toml's own format. Aliases
and functions deliberately do not travel. Nothing applies without this
command; a changed file is reported by doctor and re-applied only by
running adopt again.`

// teamConfig is the checked-in file. The two sections are the two
// existing formats, verbatim — a reader already knows how to read them,
// and adopt is a loop over two existing writers, not a new language.
type teamConfig struct {
	Settings map[string]string `toml:"settings"`
	Plugins  []manifest.Plugin `toml:"plugins"`
}

// adoptRecord is one adoption in the ledger: enough to detect drift
// (the hash) and to revert exactly what was added (the plugin sources —
// the fragment file carries the settings).
type adoptRecord struct {
	Dir      string `json:"dir"`      // the directory holding .koi.toml
	Hash     string `json:"hash"`     // sha256 of the file as adopted
	Fragment string `json:"fragment"` // the rc fragment written
	// Plugins holds the entries adoption itself added, in full: revert
	// compares against the current entry and removes only an exact
	// match, so one the user has since edited is theirs and stays.
	Plugins   []manifest.Plugin `json:"plugins"`
	AdoptedAt string            `json:"adopted_at"`
}

// RunAdopt implements the subcommand. A subcommand rather than only a
// builtin for the migrate reason: the moment it matters most is a
// fresh clone, before anyone has a koi session open.
func RunAdopt(in io.Reader, out, errOut io.Writer, args []string) error {
	var revert, status, yes bool
	for _, a := range args {
		switch a {
		case "--revert":
			revert = true
		case "--status":
			status = true
		case "--yes", "-y":
			yes = true
		case "--help", "-h":
			fmt.Fprintln(out, adoptUsage)
			return nil
		default:
			fmt.Fprintln(errOut, adoptUsage)
			return fmt.Errorf("unknown argument %q", a)
		}
	}

	path := findTeamConfig()
	if path == "" {
		return errors.New("no .koi.toml here or above — nothing to adopt")
	}
	dir := filepath.Dir(path)

	switch {
	case status:
		return adoptStatus(out, dir, path)
	case revert:
		return adoptRevert(out, dir)
	default:
		return adoptApply(in, out, errOut, dir, path, yes)
	}
}

// findTeamConfig walks up from the cwd, the .tool-versions posture: a
// teammate runs the command anywhere inside the repo, not only at its
// root. The walk stops at the filesystem root; $HOME is not special,
// since a repo can legitimately live at ~.
func findTeamConfig() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, ".koi.toml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func parseTeamConfig(path string) (*teamConfig, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	var tc teamConfig
	if err := toml.Unmarshal(raw, &tc); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	return &tc, hex.EncodeToString(sum[:8]), nil
}

// validateSettings checks every name and value against `config`'s own
// table before anything applies — all of it or none of it, because a
// half-applied team config is harder to reason about than a rejected
// one. Free-form settings (prompt) are allowed as-is: they are display
// strings, the same trust class as a theme name.
func validateSettings(settings map[string]string) ([]configSetting, map[string]string, error) {
	var resolved []configSetting
	for name, value := range settings {
		idx := slices.IndexFunc(configSettings, func(s configSetting) bool { return s.name == name })
		if idx < 0 {
			return nil, nil, fmt.Errorf("unknown setting %q — the vocabulary is `config`'s", name)
		}
		s := configSettings[idx]
		if len(s.allowed) > 0 && !slices.Contains(s.allowed, value) {
			return nil, nil, fmt.Errorf("%s must be one of: %s", s.name, strings.Join(s.allowed, " | "))
		}
		resolved = append(resolved, s)
	}
	sort.Slice(resolved, func(i, j int) bool { return resolved[i].name < resolved[j].name })
	return resolved, settings, nil
}

func adoptApply(in io.Reader, out, errOut io.Writer, dir, path string, yes bool) error {
	tc, hash, err := parseTeamConfig(path)
	if err != nil {
		return err
	}
	resolved, values, err := validateSettings(tc.Settings)
	if err != nil {
		return err
	}
	if len(resolved) == 0 && len(tc.Plugins) == 0 {
		return fmt.Errorf("%s carries nothing to adopt", path)
	}

	// Preview before anything is written: the review is the point, and
	// the file was authored by someone else.
	fmt.Fprintf(out, "adopting %s:\n", path)
	for _, s := range resolved {
		fmt.Fprintf(out, "  setting  %-12s %s\n", s.name, values[s.name])
	}
	for _, p := range tc.Plugins {
		pin := p.Pin
		if pin == "" {
			pin = "UNPINNED"
		}
		fmt.Fprintf(out, "  plugin   %-30s %s\n", p.Source, pin)
		if p.Pin == "" {
			fmt.Fprintf(errOut, "koi adopt: %s is unpinned — the team's config will drift per machine; pins are what stop that\n", p.Source)
		}
	}
	if !yes {
		fmt.Fprint(out, "apply? [y/N] ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			fmt.Fprintln(out, "nothing applied")
			return nil
		}
	}

	// The fragment: sourced before the user's rc, so their own settings
	// win by last-write. One file per adopted repo, inspectable with cat.
	fragment, err := writeAdoptFragment(dir, path, resolved, values)
	if err != nil {
		return err
	}

	// Plugins go through the manifest, which is what manages them; only
	// entries adoption itself added are recorded, so revert never
	// removes something the user installed on their own.
	added, err := adoptPlugins(errOut, tc.Plugins)
	if err != nil {
		return err
	}

	rec := adoptRecord{
		Dir: dir, Hash: hash, Fragment: fragment, Plugins: added,
		AdoptedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := saveAdoptRecord(rec); err != nil {
		return err
	}
	fmt.Fprintf(out, "adopted — settings apply to new sessions; revert with `koi adopt --revert` here\n")
	return nil
}

// writeAdoptFragment renders the settings as the rc lines `config`
// itself would write, into $XDG_CONFIG_HOME/koi/adopted.d/<slug>.koirc.
func writeAdoptFragment(dir, path string, resolved []configSetting, values map[string]string) (string, error) {
	fragDir := adoptFragmentDir()
	if fragDir == "" {
		return "", errors.New("cannot resolve a config directory")
	}
	if err := os.MkdirAll(fragDir, 0o755); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# adopted from %s by `koi adopt` — delete this file or run\n", path)
	fmt.Fprintf(&b, "# `koi adopt --revert` in that repo to undo. Your own rc runs after\n")
	fmt.Fprintf(&b, "# this file, so anything you set there wins.\n")
	for _, s := range resolved {
		fmt.Fprintf(&b, "export %s=%q\n", s.varName, values[s.name])
	}
	fragment := filepath.Join(fragDir, adoptSlug(dir)+".koirc")
	if err := os.WriteFile(fragment, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return fragment, nil
}

func adoptPlugins(errOut io.Writer, plugins []manifest.Plugin) ([]manifest.Plugin, error) {
	if len(plugins) == 0 {
		return nil, nil
	}
	path, err := manifest.DefaultPath()
	if err != nil {
		return nil, err
	}
	m, err := manifest.Load(path)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", path, err)
	}
	var added []manifest.Plugin
	for _, p := range plugins {
		// An entry the user already has stays theirs: manifest.Add would
		// replace it, and a team config silently repinning a plugin the
		// user chose a version of is the clobbering this feature must
		// never do.
		if m.Find(p.Source) >= 0 {
			fmt.Fprintf(errOut, "koi adopt: %s is already in your plugins.toml — left as yours\n", p.Source)
			continue
		}
		m.Add(p)
		added = append(added, p)
	}
	if len(added) > 0 {
		if err := m.Save(path); err != nil {
			return nil, err
		}
	}
	return added, nil
}

// pluginEqual compares by value; Enabled is a pointer, and the recorded
// copy round-trips through JSON, so pointer identity would never match.
func pluginEqual(a, b manifest.Plugin) bool {
	enabled := func(e *bool) bool { return e == nil || *e }
	return a.Source == b.Source && a.Kind == b.Kind && a.Pin == b.Pin &&
		a.Lazy == b.Lazy && enabled(a.Enabled) == enabled(b.Enabled)
}

func adoptStatus(out io.Writer, dir, path string) error {
	recs, _ := loadAdoptRecords()
	idx := slices.IndexFunc(recs, func(r adoptRecord) bool { return r.Dir == dir })
	if idx < 0 {
		fmt.Fprintf(out, "%s exists and is not adopted — `koi adopt` applies it\n", path)
		return nil
	}
	rec := recs[idx]
	fmt.Fprintf(out, "adopted %s (%s)\n  fragment %s\n", rec.Dir, rec.AdoptedAt, rec.Fragment)
	for _, p := range rec.Plugins {
		fmt.Fprintf(out, "  plugin   %s\n", p.Source)
	}
	if _, hash, err := parseTeamConfig(path); err == nil && hash != rec.Hash {
		fmt.Fprintln(out, "  the file has changed since adoption — `koi adopt` re-applies it")
	}
	return nil
}

func adoptRevert(out io.Writer, dir string) error {
	recs, err := loadAdoptRecords()
	if err != nil {
		return fmt.Errorf("the adoption ledger is unreadable (%v) — the fragment under %s can be deleted by hand", err, adoptFragmentDir())
	}
	idx := slices.IndexFunc(recs, func(r adoptRecord) bool { return r.Dir == dir })
	if idx < 0 {
		return fmt.Errorf("nothing adopted from %s", dir)
	}
	rec := recs[idx]
	if err := os.Remove(rec.Fragment); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Remove only what adoption added, and only if it still looks like
	// what was added: an entry the user has since repinned or otherwise
	// edited is theirs now, and silently deleting it would revert more
	// than the adoption.
	if len(rec.Plugins) > 0 {
		path, perr := manifest.DefaultPath()
		if m, err := manifest.Load(path); perr == nil && err == nil {
			removed := false
			for _, p := range rec.Plugins {
				i := m.Find(p.Source)
				switch {
				case i < 0:
					// Already gone; nothing to say.
				case pluginEqual(m.Plugins[i], p):
					m.Remove(p.Source)
					removed = true
				default:
					fmt.Fprintf(out, "koi adopt: %s was edited since adoption — left alone\n", p.Source)
				}
			}
			if removed {
				if err := m.Save(path); err != nil {
					return err
				}
			}
		}
	}
	recs = slices.Delete(recs, idx, idx+1)
	if err := saveAdoptRecords(recs); err != nil {
		return err
	}
	fmt.Fprintf(out, "reverted — settings clear on the next session\n")
	return nil
}

// checkAdopted is doctor's staleness half of #209: the answer to "does
// adopt re-run on git pull" is that staleness detection is free (the
// ledger holds the hash) and noticing happens here, where advisory
// things are noticed — never ambiently on cd, and never by re-applying
// on its own. Silent when the cwd has no team config or has one the
// user chose not to adopt: koi does not push.
func checkAdopted() checkResult {
	path := findTeamConfig()
	if path == "" {
		return checkResult{checkOK, "adopted", "no team config in scope", ""}
	}
	recs, err := loadAdoptRecords()
	if err != nil {
		return checkResult{
			checkWarn, "adopted",
			"the adoption ledger is unreadable: " + err.Error(),
			"inspect " + adoptLedgerPath(),
		}
	}
	dir := filepath.Dir(path)
	idx := slices.IndexFunc(recs, func(r adoptRecord) bool { return r.Dir == dir })
	if idx < 0 {
		return checkResult{
			checkOK, "adopted",
			displayPath(path) + " exists and is not adopted (koi adopt applies it)", "",
		}
	}
	if _, hash, perr := parseTeamConfig(path); perr == nil && hash != recs[idx].Hash {
		return checkResult{
			checkWarn, "adopted",
			displayPath(path) + " has changed since it was adopted",
			"review it, then `koi adopt` to re-apply or `koi adopt --revert` to undo",
		}
	}
	return checkResult{checkOK, "adopted", displayPath(path) + " adopted and current", ""}
}

// ---- fragment and ledger locations ----

func adoptFragmentDir() string {
	confHome := os.Getenv("XDG_CONFIG_HOME")
	if confHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		confHome = filepath.Join(home, ".config")
	}
	return filepath.Join(confHome, "koi", "adopted.d")
}

// adoptSlug names the fragment after the repo, with a short hash of the
// absolute path so two checkouts of the same name cannot collide.
func adoptSlug(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	base := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, filepath.Base(dir))
	return base + "-" + hex.EncodeToString(sum[:4])
}

func adoptLedgerPath() string {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "koi", "adopted.json")
}

func loadAdoptRecords() ([]adoptRecord, error) {
	path := adoptLedgerPath()
	if path == "" {
		return nil, errors.New("cannot resolve a data directory")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []adoptRecord
	if err := json.Unmarshal(raw, &recs); err != nil {
		// Corrupt is reported, not reset: the ledger is what revert
		// depends on, and silently starting fresh would orphan fragments.
		return nil, err
	}
	return recs, nil
}

func saveAdoptRecord(rec adoptRecord) error {
	recs, err := loadAdoptRecords()
	if err != nil {
		return err
	}
	idx := slices.IndexFunc(recs, func(r adoptRecord) bool { return r.Dir == rec.Dir })
	if idx >= 0 {
		recs[idx] = rec
	} else {
		recs = append(recs, rec)
	}
	return saveAdoptRecords(recs)
}

// saveAdoptRecords writes the ledger write-then-rename, the envtrust
// posture: the file either has the old contents or the new, never half.
func saveAdoptRecords(recs []adoptRecord) error {
	path := adoptLedgerPath()
	if path == "" {
		return errors.New("cannot resolve a data directory")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// adoptedFragments returns the fragments to source before the user's
// rc, sorted for a stable order. Missing directory is the normal state.
func adoptedFragments() []string {
	dir := adoptFragmentDir()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".koirc") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}
