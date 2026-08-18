// Package emit generates the shell payload that loads an object. Upstream
// zi-go emitted zsh for a shim to eval; in koi the shell itself runs the
// payload (the zi builtin becomes `source <payload>` via the CallHandler),
// so the dialect is koi's bash-level interpreter — zsh-isms are rewritten:
// path=(…) → PATH=, ${+commands[x]} → command -v, print -P → printf. fpath
// is still recorded (as a plain array) for the future completion engine.
package emit

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/blairham/koi-shell/internal/plugmgr/ice"
	"github.com/blairham/koi-shell/internal/plugmgr/spec"
)

// sourceCandidates is Zi's file-resolution order from lib/zsh/side.zsh:
// the repo-named plugin file first (the overwhelmingly common case), then
// progressively looser patterns.
func sourceCandidates(repo string) []string {
	return []string{
		repo + ".plugin.zsh",
		"*.plugin.zsh",
		"init.zsh",
		repo + ".zsh-theme",
		"*.zsh-theme",
		repo + ".zsh",
		"*.zsh",
		repo + ".sh",
		"*.sh",
	}
}

// ResolveSourceFile picks the file to source from a plugin dir, honoring the
// pick ice glob when present.
func ResolveSourceFile(dir, repo string, ic *ice.Ices) (string, error) {
	if pick := ic.Get("pick"); pick != "" {
		matches, err := filepath.Glob(filepath.Join(dir, pick))
		if err != nil {
			return "", err
		}
		if len(matches) == 0 {
			return "", fmt.Errorf("pick ice %q matches nothing in %s", pick, dir)
		}
		sort.Strings(matches)
		return matches[0], nil
	}
	for _, pattern := range sourceCandidates(repo) {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		if len(matches) > 0 {
			sort.Strings(matches)
			return matches[0], nil
		}
	}
	return "", fmt.Errorf("no sourceable file found in %s (set the pick ice)", dir)
}

// ResolveProgram picks the executable for as"program": the pick glob, the
// repo-named file, or the only executable regular file in the dir.
func ResolveProgram(dir, repo string, ic *ice.Ices) (string, error) {
	if pick := ic.Get("pick"); pick != "" {
		matches, err := filepath.Glob(filepath.Join(dir, pick))
		if err != nil {
			return "", err
		}
		if len(matches) == 0 {
			return "", fmt.Errorf("pick ice %q matches nothing in %s", pick, dir)
		}
		sort.Strings(matches)
		return matches[0], nil
	}
	if repo != "" {
		if st, err := os.Stat(filepath.Join(dir, repo)); err == nil && !st.IsDir() {
			return filepath.Join(dir, repo), nil
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var execs []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if info, err := e.Info(); err == nil && info.Mode()&0o111 != 0 {
			execs = append(execs, filepath.Join(dir, e.Name()))
		}
	}
	if len(execs) == 1 {
		return execs[0], nil
	}
	return "", fmt.Errorf("cannot determine the program in %s (set the pick ice)", dir)
}

// zq single-quotes a string for zsh.
func zq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Extra is zsh contributed by annex pre-load hooks: prelude runs after atinit
// (before sourcing), epilogue after sourcing but before the user's atload, so
// the user's ice keeps the last word.
type Extra struct {
	Prelude  []string
	Epilogue []string
}

func mergeExtras(extras []Extra) Extra {
	var out Extra
	for _, e := range extras {
		out.Prelude = append(out.Prelude, e.Prelude...)
		out.Epilogue = append(out.Epilogue, e.Epilogue...)
	}
	return out
}

func writeLines(b *strings.Builder, lines []string) {
	for _, l := range lines {
		b.WriteString(strings.TrimRight(l, "\n") + "\n")
	}
}

// PluginPayload renders the zsh that loads an installed plugin.
func PluginPayload(s *spec.Spec, dir, sourceFile string, ic *ice.Ices, extras ...Extra) string {
	extra := mergeExtras(extras)
	var b strings.Builder
	fmt.Fprintf(&b, "# zi-go payload: %s\n", s.Raw)
	writeGuards(&b, ic)
	as := ic.Get("as")
	if as != "completion" {
		fmt.Fprintf(&b, "fpath=( %s $fpath )\n", zq(dir))
	}
	if v := ic.Get("atinit"); v != "" {
		b.WriteString(v + "\n")
	}
	writeLines(&b, extra.Prelude)
	switch as {
	case "program":
		if sourceFile != "" {
			fmt.Fprintf(&b, "command chmod +x %s 2>/dev/null\n", zq(sourceFile))
		}
		fmt.Fprintf(&b, "PATH=%s:\"$PATH\"\n", zq(dir))
	case "null", "completion":
		// Nothing sourced.
	default:
		if sourceFile != "" {
			writeSource(&b, sourceFile, ic)
		}
	}
	if v := ic.Get("src"); v != "" {
		writeSource(&b, filepath.Join(dir, v), ic)
	}
	for _, f := range strings.Fields(ic.Get("multisrc")) {
		writeSource(&b, filepath.Join(dir, f), ic)
	}
	writeLines(&b, extra.Epilogue)
	writeEpilogue(&b, s, ic)
	return b.String()
}

// SnippetPayload renders the zsh that loads a downloaded snippet file.
func SnippetPayload(s *spec.Spec, file string, ic *ice.Ices, extras ...Extra) string {
	extra := mergeExtras(extras)
	var b strings.Builder
	fmt.Fprintf(&b, "# zi-go payload: %s\n", s.Raw)
	writeGuards(&b, ic)
	if v := ic.Get("atinit"); v != "" {
		b.WriteString(v + "\n")
	}
	writeLines(&b, extra.Prelude)
	if ic.Get("as") == "program" {
		fmt.Fprintf(&b, "command chmod +x %s 2>/dev/null\n", zq(file))
		fmt.Fprintf(&b, "PATH=%s:\"$PATH\"\n", zq(filepath.Dir(file)))
	} else {
		writeSource(&b, file, ic)
	}
	writeLines(&b, extra.Epilogue)
	writeEpilogue(&b, s, ic)
	return b.String()
}

// writeGuards emits the has/if conditional ices; a failed guard returns from
// the payload before anything is sourced.
func writeGuards(b *strings.Builder, ic *ice.Ices) {
	if v := ic.Get("has"); v != "" {
		fmt.Fprintf(b, "command -v %s >/dev/null 2>&1 || return 0\n", v)
	}
	if v := ic.Get("if"); v != "" {
		fmt.Fprintf(b, "if ! eval %s; then return 0; fi\n", zq(v))
	}
}

func writeSource(b *strings.Builder, file string, ic *ice.Ices) {
	if ic.Has("silent") {
		fmt.Fprintf(b, "builtin source %s &>/dev/null\n", zq(file))
	} else {
		fmt.Fprintf(b, "builtin source %s\n", zq(file))
	}
}

func writeEpilogue(b *strings.Builder, s *spec.Spec, ic *ice.Ices) {
	if v := ic.Get("atload"); v != "" {
		b.WriteString(v + "\n")
	}
	fmt.Fprintf(b, "ZI_GO_LOADED+=( %s )\n", zq(s.Raw))
	// Loads announce themselves unless lucid/silent, like Zi.
	if ic.Has("wait") && !ic.Has("lucid") && !ic.Has("silent") {
		fmt.Fprintf(b, "printf 'Loaded %%s\\n' %s\n", zq(s.Raw))
	}
}
