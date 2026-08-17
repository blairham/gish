// Package installer materializes plugins and snippets on disk and runs the
// install-time ice hooks (atclone/atpull, make, mv, cp), mirroring the
// responsibilities of Zi's lib/zsh/install.zsh.
package installer

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blairham/gish/internal/plugmgr/config"
	"github.com/blairham/gish/internal/plugmgr/ghr"
	"github.com/blairham/gish/internal/plugmgr/gitutil"
	"github.com/blairham/gish/internal/plugmgr/ice"
	"github.com/blairham/gish/internal/plugmgr/spec"
	"github.com/blairham/gish/internal/plugmgr/state"
)

type Installer struct {
	Cfg *config.Config
	// Out receives progress messages, shown in the user's terminal.
	Out io.Writer
}

// Annex hooks from upstream zi-go are not ported: gish's tier-2 plugin
// system is the extension mechanism here (see issue #23).

var snippetClient = &http.Client{Timeout: 2 * time.Minute}

// EnsurePlugin installs the plugin if missing and returns its directory.
// Already-installed plugins are returned as-is — updating is explicit via
// Update, matching Zi.
func (in *Installer) EnsurePlugin(s *spec.Spec, ic *ice.Ices) (string, bool, error) {
	dir := filepath.Join(in.Cfg.PluginsDir(), s.ID)
	if _, err := os.Stat(dir); err == nil {
		return dir, false, nil
	}
	if s.URL == "" {
		return "", false, fmt.Errorf("plugin %q is not installed and the spec has no source URL", s.Raw)
	}
	fmt.Fprintf(in.Out, "Downloading %s...\n", s.Raw)
	if err := in.fetchPlugin(s, ic, dir); err != nil {
		os.RemoveAll(dir)
		return "", false, err
	}
	if err := in.runInstallHooks(dir, ic, false); err != nil {
		return "", false, err
	}
	if err := state.SaveObject(dir, s, ic); err != nil {
		return "", false, err
	}
	return dir, true, nil
}

func (in *Installer) fetchPlugin(s *spec.Spec, ic *ice.Ices, dir string) error {
	if ic.Get("from") == "gh-r" {
		if s.User == "" || s.Repo == "" {
			return fmt.Errorf("from'gh-r' needs a user/repo spec, got %q", s.Raw)
		}
		rel, err := ghr.FetchRelease(s.User, s.Repo, ic.Get("ver"))
		if err != nil {
			return err
		}
		asset, err := ghr.PickAsset(rel.Assets, ic.Get("bpick"))
		if err != nil {
			return err
		}
		fmt.Fprintf(in.Out, "  release %s, asset %s\n", rel.TagName, asset.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := ghr.Download(asset, dir); err != nil {
			return err
		}
		// Record the installed tag so Update can skip up-to-date releases.
		return os.WriteFile(filepath.Join(dir, ".zi-go-release"), []byte(rel.TagName+"\n"), 0o644)
	}
	depth := 0
	if d := ic.Get("depth"); d != "" {
		n, err := strconv.Atoi(d)
		if err != nil {
			return fmt.Errorf("depth ice %q is not a number", d)
		}
		depth = n
	}
	return gitutil.Clone(s.URL, dir, ic.Get("ver"), depth)
}

// EnsureSnippet downloads a snippet if missing and returns the path of the
// stored file inside its snippet directory.
func (in *Installer) EnsureSnippet(s *spec.Spec, ic *ice.Ices) (string, bool, error) {
	dir := filepath.Join(in.Cfg.SnippetsDir(), s.ID)
	fname := filepath.Base(strings.TrimSuffix(s.URL, "/"))
	file := filepath.Join(dir, fname)
	if _, err := os.Stat(file); err == nil {
		return file, false, nil
	}
	fmt.Fprintf(in.Out, "Downloading snippet %s...\n", s.Raw)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	if err := in.fetchSnippet(s.URL, file); err != nil {
		os.RemoveAll(dir)
		return "", false, err
	}
	if err := in.runInstallHooks(dir, ic, false); err != nil {
		return "", false, err
	}
	if err := state.SaveObject(dir, s, ic); err != nil {
		return "", false, err
	}
	return file, true, nil
}

func (in *Installer) fetchSnippet(url, dest string) error {
	resp, err := snippetClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// runInstallHooks applies mv/cp, then make, then atclone (or atpull on
// update) — the same ordering Zi documents.
func (in *Installer) runInstallHooks(dir string, ic *ice.Ices, isUpdate bool) error {
	if v := ic.Get("mv"); v != "" {
		if err := moveOrCopy(dir, v, true); err != nil {
			return fmt.Errorf("mv ice: %w", err)
		}
	}
	if v := ic.Get("cp"); v != "" {
		if err := moveOrCopy(dir, v, false); err != nil {
			return fmt.Errorf("cp ice: %w", err)
		}
	}
	if ic.Has("make") {
		args := strings.Fields(ic.Get("make"))
		if err := in.shellRun(dir, "make", args, ic); err != nil {
			return fmt.Errorf("make ice: %w", err)
		}
	}
	hook := "atclone"
	if isUpdate {
		hook = "atpull"
	}
	if v := ic.Get(hook); v != "" {
		// Zi's atpull"%atclone" convention: rerun the atclone hook on update.
		if v == "%atclone" {
			v = ic.Get("atclone")
		}
		if v != "" {
			if err := in.shellRun(dir, "sh", []string{"-c", v}, ic); err != nil {
				return fmt.Errorf("%s ice: %w", hook, err)
			}
		}
	}
	return nil
}

func (in *Installer) shellRun(dir, prog string, args []string, ic *ice.Ices) error {
	cmd := exec.Command(prog, args...)
	cmd.Dir = dir
	if ic.Has("silent") {
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	} else {
		cmd.Stdout, cmd.Stderr = in.Out, in.Out
	}
	return cmd.Run()
}

// moveOrCopy implements the mv/cp ices' "from -> to" syntax with glob
// support on the left-hand side.
func moveOrCopy(dir, expr string, move bool) error {
	from, to, ok := strings.Cut(expr, "->")
	if !ok {
		return fmt.Errorf("want \"from -> to\", got %q", expr)
	}
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	matches, err := filepath.Glob(filepath.Join(dir, from))
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		return fmt.Errorf("no file matches %q", from)
	}
	// The destination comes from the plugin's own install recipe, so it
	// is the plugin author's text rather than the user's. Keeping it
	// inside the object directory is what stops `mv"x -> ../../y"` from
	// making a plugin install into an arbitrary write (#204). The source
	// is a glob rooted at dir and needs no separate check.
	dst, err := containedPath(dir, to)
	if err != nil {
		return err
	}
	src := matches[0]
	if move {
		return os.Rename(src, dst)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, st.Mode())
}

// Update refreshes one installed object dir, rerunning update hooks when new
// content arrived. Returns a short human-readable outcome.
func (in *Installer) Update(dir string) (string, error) {
	obj, err := state.LoadObject(dir)
	if err != nil {
		return "", fmt.Errorf("%s: %w (reinstall with delete + load)", filepath.Base(dir), err)
	}
	ic := ice.FromMap(obj.Ices)
	switch {
	case obj.Kind == "snippet":
		fname := filepath.Base(strings.TrimSuffix(obj.URL, "/"))
		if err := in.fetchSnippet(obj.URL, filepath.Join(dir, fname)); err != nil {
			return "", err
		}
		if err := in.runInstallHooks(dir, ic, true); err != nil {
			return "", err
		}
		return "refreshed", nil
	case ic.Get("from") == "gh-r":
		rel, err := ghr.FetchRelease(obj.User, obj.Repo, ic.Get("ver"))
		if err != nil {
			return "", err
		}
		cur, _ := os.ReadFile(filepath.Join(dir, ".zi-go-release"))
		if strings.TrimSpace(string(cur)) == rel.TagName {
			return "up to date (" + rel.TagName + ")", nil
		}
		asset, err := ghr.PickAsset(rel.Assets, ic.Get("bpick"))
		if err != nil {
			return "", err
		}
		if err := ghr.Download(asset, dir); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, ".zi-go-release"), []byte(rel.TagName+"\n"), 0o644); err != nil {
			return "", err
		}
		if err := in.runInstallHooks(dir, ic, true); err != nil {
			return "", err
		}
		return "updated to " + rel.TagName, nil
	case gitutil.IsRepo(dir):
		changed, err := gitutil.Pull(dir)
		if err != nil {
			return "", err
		}
		if !changed {
			return "up to date", nil
		}
		if err := in.runInstallHooks(dir, ic, true); err != nil {
			return "", err
		}
		return "updated", nil
	default:
		return "skipped (not a git repo, release, or snippet)", nil
	}
}

// containedPath joins name under dir and refuses anything that climbs
// out of it — the same rule ghr applies to archive entries, for the same
// reason: the value is authored by whoever wrote the plugin, not by the
// person installing it.
func containedPath(dir, name string) (string, error) {
	target := filepath.Join(dir, filepath.Clean(name))
	if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) {
		return "", fmt.Errorf("destination %q escapes the plugin directory", name)
	}
	return target, nil
}
