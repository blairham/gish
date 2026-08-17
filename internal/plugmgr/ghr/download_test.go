package ghr

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Download writes what a GitHub release hands back, and the asset name is
// remote data — it comes out of the API's JSON, not from anything the
// user typed. So every path built from it has to be contained, or
// installing a plugin lets the repo it came from write anywhere the shell
// can (#204).
//
// The archive paths were already guarded by securePath. These tests cover
// the two that were not.

func serving(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// tarGz builds a .tar.gz holding one entry under the given name.
func tarGz(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipped(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A traversing asset name must not write outside the destination, on any
// of the four branches Download dispatches to.
func TestDownloadContainsTraversingAssetNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		asset string
		body  func(*testing.T) []byte
	}{
		{
			// The default branch: any name that is not a recognized
			// archive is written verbatim.
			name:  "plain asset",
			asset: "../escaped-plain",
			body:  func(*testing.T) []byte { return []byte("payload") },
		},
		{
			// The .gz branch trims the suffix and writes the rest, so the
			// traversal survives the trim.
			name:  "gzipped asset",
			asset: "../escaped-gz.gz",
			body:  func(t *testing.T) []byte { return gzipped(t, "payload") },
		},
		{
			// Already guarded, kept here so the whole dispatch is covered
			// by one table and a future branch cannot quietly skip it.
			name:  "tar entry",
			asset: "payload.tar.gz",
			body:  func(t *testing.T) []byte { return tarGz(t, "../escaped-tar", "payload") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			outside := t.TempDir()
			dest := filepath.Join(outside, "dest")
			if err := os.MkdirAll(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			srv := serving(t, tt.body(t))

			err := Download(&Asset{Name: tt.asset, URL: srv.URL}, dest)

			// Refusing is the required outcome; what must never happen is
			// a file appearing above the destination.
			entries, rerr := os.ReadDir(outside)
			if rerr != nil {
				t.Fatal(rerr)
			}
			for _, e := range entries {
				if e.Name() != "dest" {
					t.Fatalf("wrote %q outside the destination (err=%v)", e.Name(), err)
				}
			}
			if err == nil {
				t.Errorf("traversing asset %q was accepted", tt.asset)
			}
		})
	}
}

// The ordinary case still works: a plain asset lands in the destination
// under its own name.
func TestDownloadWritesPlainAssets(t *testing.T) {
	t.Parallel()

	dest := t.TempDir()
	srv := serving(t, []byte("hello"))
	if err := Download(&Asset{Name: "tool", URL: srv.URL}, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("asset content = %q, want %q", got, "hello")
	}
}

// A .gz asset is decompressed and named without the suffix.
func TestDownloadDecompressesGzAssets(t *testing.T) {
	t.Parallel()

	dest := t.TempDir()
	srv := serving(t, gzipped(t, "binary-bytes"))
	if err := Download(&Asset{Name: "tool.gz", URL: srv.URL}, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "binary-bytes" {
		t.Errorf("decompressed content = %q", got)
	}
}

// A non-200 writes nothing and says why.
func TestDownloadReportsHTTPFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	dest := t.TempDir()
	err := Download(&Asset{Name: "tool", URL: srv.URL}, dest)
	if err == nil {
		t.Fatal("a 404 was treated as a successful download")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error does not name the status: %v", err)
	}
	entries, rerr := os.ReadDir(dest)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Errorf("a failed download left files behind: %v", entries)
	}
}
