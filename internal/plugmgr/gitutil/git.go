// Package gitutil shells out to the git binary, like Zi does — avoiding a
// pure-Go git dependency keeps the binary small and behavior identical to
// what users get from git on their machine (credentials, config, proxies).
package gitutil

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String(), nil
}

// Clone clones url into dir. depth 0 means a full clone; ver, when non-empty,
// is checked out after cloning (branch, tag, or commit).
func Clone(url, dir, ver string, depth int) error {
	args := []string{"clone", "--recursive"}
	if depth > 0 {
		args = append(args, "--depth", strconv.Itoa(depth))
	}
	args = append(args, url, dir)
	if _, err := run("", args...); err != nil {
		return err
	}
	if ver != "" {
		if _, err := run(dir, "checkout", ver); err != nil {
			return err
		}
	}
	return nil
}

// Pull updates an existing clone. Returns true when new commits arrived.
func Pull(dir string) (changed bool, err error) {
	before, err := run(dir, "rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	if _, err := run(dir, "pull", "--rebase", "--recurse-submodules"); err != nil {
		return false, err
	}
	after, err := run(dir, "rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(before) != strings.TrimSpace(after), nil
}

// IsRepo reports whether dir is a git checkout.
func IsRepo(dir string) bool {
	st, err := os.Stat(dir + "/.git")
	return err == nil && st.IsDir()
}

// Log returns recent one-line commit subjects (for `zi changes`).
func Log(dir string, n int) (string, error) {
	return run(dir, "log", "--oneline", "-n", strconv.Itoa(n))
}
