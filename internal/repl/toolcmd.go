package repl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/tools"
	"github.com/blairham/koi-shell/internal/ui"
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
  tool install golang 1.26.6  via asdf (installing is a package
                             manager's job; --from prints the ubi/mise
                             one-liner for the tool you want)`

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
		hc.Errf("tool: %v\n", err)
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
		// Descoped (#112): installing software is a package manager's
		// job. koi switches versions; it does not ship a downloader.
		printInstallDelegation(hc, args[1], args[2], args[4])
		return []string{"false"}

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
	style := ui.Styles(ui.Enabled(hc.Stdout))
	fmt.Fprintf(hc.Stdout, "pins from %s:\n", displayPath(res.File))
	for _, pin := range tools.ParseFile(res.File) {
		name := style.Bold.Render(fmt.Sprintf("%-12s", pin.Tool))
		if active := activeVersion(hc.Dir, roots, pin.Tool); active != "" {
			fmt.Fprintf(hc.Stdout, "  %s %s\n", name, style.Accent.Render(active))
		} else if pin.Resolves(roots) {
			fmt.Fprintf(hc.Stdout, "  %s %s\n", name, pin.Versions[0])
		} else {
			fmt.Fprintf(hc.Stdout, "  %s %s — %s %s\n",
				name, pin.Versions[0], style.Fail.Render("NOT INSTALLED"),
				style.Dim.Render(fmt.Sprintf("(tool install %s %s)", pin.Tool, pin.Versions[0])))
		}
	}
}

// printInstallDelegation names the tool that should do this install.
// The scope line (#112): koi is native for the keystroke, prompt, and
// cd path — switching versions is its job — and delegates everything
// else. A release downloader carries package-manager obligations
// (archive formats, provenance, platform matrices, rate limits) that
// mise, ubi, and asdf already own full time.
func printInstallDelegation(hc interp.HandlerContext, tool, version, repo string) {
	v := strings.TrimPrefix(version, "v")
	hc.Errf("tool: koi switches versions, it does not install them.\n")
	// The one-liners under it are a continuation rather than
	// diagnostics of their own, so they go unlocated (#611).
	hc.RawErrf("  ubi  --project %s --tag %s --in %s\n",
		repo, version, asdfBinDir(tool, v))
	hc.RawErrf("  mise use -g %s@%s        (if mise has a backend for it)\n", tool, v)
	hc.RawErrf("  asdf plugin add %s && asdf install %s %s\n", tool, tool, v)
	hc.RawErrf("Any of those installs into a tree koi already resolves; `tool list %s` will show it.\n", tool)
}

// asdfBinDir is where a manually installed version must land for the
// switcher to find it.
func asdfBinDir(tool, version string) string {
	dir, err := tools.AsdfInstallDir(tool, version)
	if err != nil {
		// Home is unknown: name the conventional location literally.
		return "~/.asdf/installs/" + tool + "/" + version + "/bin"
	}
	return filepath.Join(dir, "bin")
}
