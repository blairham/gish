package repl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/plugmgr/ghr"
	"github.com/blairham/gish/internal/tools"
)

// The tool command (#77 v2): the user surface over native version
// switching. Pins are data (.tool-versions), so every subcommand is a
// file edit or a lookup — plus install, which either delegates to asdf
// (full plugin-ecosystem compat, same install tree) or downloads a
// GitHub release natively via the plugmgr ghr engine.

const toolUsage = `usage: tool [list <name> | pin <name> <ver> | global <name> <ver> | install <name> <ver> [--from <owner/repo>]]

  tool                       pins in scope: what is active, what is missing
  tool list golang           installed versions of one tool
  tool pin golang 1.26.6     write the nearest .tool-versions — live now
  tool global golang 1.26.6  write the ~/.tool-versions global
  tool install golang 1.26.6            via asdf (plugin ecosystem)
  tool install shellcheck v0.10.0 --from koalaman/shellcheck
                             native GitHub-release download into the
                             asdf tree (single-binary tools)`

// toolCallHandler intercepts `tool`, config-style.
func toolCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "tool" {
			return next(ctx, args)
		}
		return runTool(interp.HandlerCtx(ctx), args[1:]), nil
	}
}

func runTool(hc interp.HandlerContext, args []string) []string {
	fail := func(err error) []string {
		fmt.Fprintln(hc.Stderr, "tool:", err)
		return []string{"false"}
	}
	roots := tools.InstallRoots()

	switch {
	case len(args) == 0:
		showToolOverview(hc, roots)
		return []string{"true"}

	case args[0] == "help" || args[0] == "-h" || args[0] == "--help":
		fmt.Fprintln(hc.Stdout, toolUsage)
		return []string{"true"}

	case args[0] == "list" && len(args) == 2:
		versions := tools.Installed(roots, args[1])
		if len(versions) == 0 {
			fmt.Fprintf(hc.Stdout, "no installed versions of %s\n", args[1])
			return []string{"true"}
		}
		active := activeVersion(hc.Dir, roots, args[1])
		for _, v := range versions {
			marker := " "
			if v == active {
				marker = "*"
			}
			fmt.Fprintf(hc.Stdout, "%s %s\n", marker, v)
		}
		return []string{"true"}

	case (args[0] == "pin" || args[0] == "global") && len(args) == 3:
		path, err := pinTarget(args[0], hc.Dir)
		if err != nil {
			return fail(err)
		}
		if err := tools.SetPin(path, args[1], args[2]); err != nil {
			return fail(err)
		}
		fmt.Fprintf(hc.Stdout, "%s %s — saved to %s\n", args[1], args[2], displayPath(path))
		if !(tools.Pin{Tool: args[1], Versions: []string{args[2]}}).Resolves(roots) {
			fmt.Fprintf(hc.Stdout, "note: %s %s is not installed — tool install %s %s\n",
				args[1], args[2], args[1], args[2])
		}
		toolsChanged()
		return []string{"true"}

	case args[0] == "install" && len(args) == 3:
		// Delegate to asdf: full plugin-ecosystem compat, same tree.
		if _, err := exec.LookPath("asdf"); err != nil {
			return fail(fmt.Errorf("asdf is not installed — use `tool install %s %s --from <owner/repo>` for a native GitHub-release download",
				args[1], args[2]))
		}
		toolsChanged() // re-resolve at the next prompt, post-install
		return []string{"asdf", "install", args[1], args[2]}

	case args[0] == "install" && len(args) == 5 && args[3] == "--from":
		if err := installFromGitHub(hc, args[1], args[2], args[4]); err != nil {
			return fail(err)
		}
		toolsChanged()
		return []string{"true"}

	default:
		return fail(fmt.Errorf("unknown arguments %q\n%s", strings.Join(args, " "), toolUsage))
	}
}

// toolsChanged forces the next prompt to re-resolve pins (a pin edit or
// install invalidates both the PATH prepends and the pins segment).
func toolsChanged() {
	if toolsMgr != nil {
		toolsMgr.invalidate()
	}
	pinCache.Clear()
}

// pinTarget picks the file a pin lands in: the nearest .tool-versions
// in scope for `pin` (creating one in the working directory when none
// exists), the home global for `global`.
func pinTarget(sub, dir string) (string, error) {
	if sub == "global" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".tool-versions"), nil
	}
	if res := tools.Resolve(dir, nil); res.File != "" {
		return res.File, nil
	}
	return filepath.Join(dir, ".tool-versions"), nil
}

// activeVersion reports which installed version the scope's pin picks.
func activeVersion(dir string, roots []string, tool string) string {
	res := tools.Resolve(dir, roots)
	if res.File == "" {
		return ""
	}
	for _, pin := range tools.ParseFile(res.File) {
		if pin.Tool != tool {
			continue
		}
		for _, v := range pin.Versions {
			if slices.Contains(tools.Installed(roots, tool), v) {
				return v
			}
		}
	}
	return ""
}

func showToolOverview(hc interp.HandlerContext, roots []string) {
	res := tools.Resolve(hc.Dir, roots)
	if res.File == "" {
		fmt.Fprintln(hc.Stdout, "no .tool-versions in scope — tool pin <name> <version> creates one")
		return
	}
	fmt.Fprintf(hc.Stdout, "pins from %s:\n", displayPath(res.File))
	for _, pin := range tools.ParseFile(res.File) {
		if active := activeVersion(hc.Dir, roots, pin.Tool); active != "" {
			fmt.Fprintf(hc.Stdout, "  %-12s %s\n", pin.Tool, active)
		} else if pin.Resolves(roots) {
			fmt.Fprintf(hc.Stdout, "  %-12s %s\n", pin.Tool, pin.Versions[0])
		} else {
			fmt.Fprintf(hc.Stdout, "  %-12s %s — NOT INSTALLED (tool install %s %s)\n",
				pin.Tool, pin.Versions[0], pin.Tool, pin.Versions[0])
		}
	}
}

// installFromGitHub downloads a release asset into the asdf tree — the
// ubi shape: explicit repo, OS/arch asset heuristics from plugmgr/ghr.
func installFromGitHub(hc interp.HandlerContext, tool, version, repo string) error {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return fmt.Errorf("--from wants owner/repo, got %q", repo)
	}
	release, err := ghr.FetchRelease(owner, name, version)
	if err != nil {
		// Release tags are usually v-prefixed; try the other spelling.
		alt := "v" + version
		if strings.HasPrefix(version, "v") {
			alt = strings.TrimPrefix(version, "v")
		}
		release, err = ghr.FetchRelease(owner, name, alt)
		if err != nil {
			return fmt.Errorf("fetch %s %s: %w", repo, version, err)
		}
	}
	asset, err := ghr.PickAsset(release.Assets, "")
	if err != nil {
		return err
	}
	dir, err := tools.AsdfInstallDir(tool, strings.TrimPrefix(version, "v"))
	if err != nil {
		return err
	}
	// Extract into a staging dir, then flatten: release archives nest
	// their binaries (shellcheck-v0.10.0/shellcheck), and PATH needs
	// them directly in bin/.
	stage := filepath.Join(dir, ".stage")
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	cleanup := func() { _ = os.RemoveAll(dir) } //nolint:errcheck // partial install
	fmt.Fprintf(hc.Stdout, "downloading %s (%s %s)…\n", asset.Name, repo, release.TagName)
	if err := ghr.Download(asset, stage); err != nil {
		cleanup()
		return err
	}
	moved, err := flattenExecutables(stage, binDir)
	if err != nil {
		cleanup()
		return err
	}
	if moved == 0 {
		cleanup()
		return fmt.Errorf("no executables found in %s — not a single-binary release?", asset.Name)
	}
	_ = os.RemoveAll(stage) //nolint:errcheck // best-effort tidy
	fmt.Fprintf(hc.Stdout, "installed %s %s to %s (%d executable(s))\n",
		tool, strings.TrimPrefix(version, "v"), displayPath(binDir), moved)
	return nil
}

// flattenExecutables moves every executable regular file under stage
// into binDir, whatever directory nesting the archive used.
func flattenExecutables(stage, binDir string) (int, error) {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return 0, err
	}
	moved := 0
	err := filepath.WalkDir(stage, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil || info.Mode()&0o111 == 0 || !info.Mode().IsRegular() {
			return err
		}
		if err := os.Rename(path, filepath.Join(binDir, d.Name())); err != nil {
			return err
		}
		moved++
		return nil
	})
	return moved, err
}
