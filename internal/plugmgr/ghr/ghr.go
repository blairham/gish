// Package ghr implements the from"gh-r" ice: installing prebuilt binaries
// from GitHub release assets instead of cloning source. Asset selection uses
// the bpick glob when given, otherwise an OS/arch scoring heuristic.
package ghr

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ulikunitz/xz"
)

type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

var httpClient = &http.Client{Timeout: 5 * time.Minute}

// FetchRelease queries the GitHub API for a release. ver "" or "latest"
// resolves the latest release; anything else is treated as a tag name.
// GITHUB_TOKEN, when set, lifts the unauthenticated rate limit.
func FetchRelease(user, repo, ver string) (*Release, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", user, repo)
	if ver != "" && ver != "latest" {
		url = fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/tags/%s", user, repo, ver)
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GitHub API %s: %s\n%s", url, resp.Status, body)
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// PickAsset chooses the asset to download. A non-empty bpick glob wins;
// otherwise assets are scored for the current OS/arch and the best match
// returned.
func PickAsset(assets []Asset, bpick string) (*Asset, error) {
	if len(assets) == 0 {
		return nil, fmt.Errorf("release has no assets")
	}
	if bpick != "" {
		for i := range assets {
			if ok, _ := path.Match(bpick, assets[i].Name); ok {
				return &assets[i], nil
			}
		}
		return nil, fmt.Errorf("no asset matches bpick %q", bpick)
	}
	best, bestScore := -1, -1
	for i := range assets {
		if s := score(assets[i].Name); s > bestScore {
			best, bestScore = i, s
		}
	}
	if bestScore <= 0 {
		return nil, fmt.Errorf("no asset matches %s/%s; use the bpick ice to choose one of: %s",
			runtime.GOOS, runtime.GOARCH, names(assets))
	}
	return &assets[best], nil
}

func score(name string) int {
	n := strings.ToLower(name)
	// Checksums/signatures are never the binary.
	for _, bad := range []string{".sha256", ".sha512", ".asc", ".sig", ".txt", ".sbom", "checksums"} {
		if strings.Contains(n, bad) {
			return -1
		}
	}
	s := 0
	osNames := map[string][]string{
		"darwin": {"darwin", "macos", "mac", "osx", "apple"},
		"linux":  {"linux"},
	}
	archNames := map[string][]string{
		"arm64": {"arm64", "aarch64"},
		"amd64": {"amd64", "x86_64", "x64"},
	}
	for _, alias := range osNames[runtime.GOOS] {
		if strings.Contains(n, alias) {
			s += 10
			break
		}
	}
	for _, alias := range archNames[runtime.GOARCH] {
		if strings.Contains(n, alias) {
			s += 5
			break
		}
	}
	// Prefer archives we can extract over raw blobs we'd have to guess about.
	if strings.HasSuffix(n, ".tar.gz") || strings.HasSuffix(n, ".tgz") || strings.HasSuffix(n, ".zip") {
		s += 2
	}
	return s
}

func names(assets []Asset) string {
	out := make([]string, len(assets))
	for i, a := range assets {
		out[i] = a.Name
	}
	return strings.Join(out, ", ")
}

// Download fetches an asset into destDir and extracts recognized archive
// formats (.tar.gz, .tgz, .tar.xz, .txz, .zip, .gz); anything else is
// saved as-is and made executable, since gh-r assets are programs by
// definition.
func Download(a *Asset, destDir string) error {
	resp, err := httpClient.Get(a.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", a.URL, resp.Status)
	}
	name := strings.ToLower(a.Name)
	switch {
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		return untar(resp.Body, destDir)
	case strings.HasSuffix(name, ".tar.xz"), strings.HasSuffix(name, ".txz"):
		xzr, err := xz.NewReader(resp.Body)
		if err != nil {
			return err
		}
		return untarStream(xzr, destDir)
	case strings.HasSuffix(name, ".zip"):
		tmp, err := os.CreateTemp("", "zi-go-ghr-*.zip")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if _, err := io.Copy(tmp, resp.Body); err != nil {
			return err
		}
		return unzip(tmp.Name(), destDir)
	case strings.HasSuffix(name, ".gz"):
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return err
		}
		defer gz.Close()
		return writeFile(filepath.Join(destDir, strings.TrimSuffix(a.Name, ".gz")), gz, 0o755)
	default:
		return writeFile(filepath.Join(destDir, a.Name), resp.Body, 0o755)
	}
}

func untar(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	return untarStream(gz, destDir)
}

// untarStream extracts an already-decompressed tar stream.
func untarStream(r io.Reader, destDir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := securePath(destDir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeFile(target, tr, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		}
	}
}

func unzip(archive, destDir string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		target, err := securePath(destDir, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFile(target, rc, f.Mode())
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// securePath guards against zip-slip: an archive entry must stay inside destDir.
func securePath(destDir, name string) (string, error) {
	target := filepath.Join(destDir, filepath.Clean(name))
	if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry %q escapes destination", name)
	}
	return target, nil
}

func writeFile(target string, r io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o100)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}
