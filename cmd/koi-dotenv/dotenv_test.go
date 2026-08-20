package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	pluginapi "github.com/blairham/koi-shell/pkg/pluginapi/v1"
)

// The dialect, case by case. Every rule here is one the two de-facto
// references (motdotla/dotenv, docker compose) agree on — where they
// disagree the parser refuses rather than guesses, and those refusals
// are tested below too.
func TestParse(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want map[string]string
	}{
		{"basic", "FOO=bar\n", map[string]string{"FOO": "bar"}},
		{"export prefix", "export FOO=bar\n", map[string]string{"FOO": "bar"}},
		{"export is a name not a keyword", "exportFOO=bar\n", map[string]string{"exportFOO": "bar"}},
		{"comments and blanks", "# a comment\n\n  # indented comment\nFOO=bar\n", map[string]string{"FOO": "bar"}},
		{"spaces around equals", "FOO = bar\n", map[string]string{"FOO": "bar"}},
		{"unquoted trailing comment", "FOO=bar # comment\n", map[string]string{"FOO": "bar"}},
		{"hash inside value is not a comment", "FOO=bar#baz\n", map[string]string{"FOO": "bar#baz"}},
		{"single quotes are literal", "FOO='$HOME # x \\n'\n", map[string]string{"FOO": "$HOME # x \\n"}},
		{"double quote escapes", `FOO="a\nb\tc\"d\\e"` + "\n", map[string]string{"FOO": "a\nb\tc\"d\\e"}},
		{"unknown escape kept verbatim", `FOO="a\d+b"` + "\n", map[string]string{"FOO": `a\d+b`}},
		{"no expansion ever", "FOO=$HOME\nBAR=${FOO}\n", map[string]string{"FOO": "$HOME", "BAR": "${FOO}"}},
		{
			"multiline double quoted", "KEY=\"-----BEGIN-----\nabc\n-----END-----\"\nNEXT=1\n",
			map[string]string{"KEY": "-----BEGIN-----\nabc\n-----END-----", "NEXT": "1"},
		},
		{
			"multiline single quoted", "KEY='line one\nline two'\nNEXT=1\n",
			map[string]string{"KEY": "line one\nline two", "NEXT": "1"},
		},
		{"text after closing quote ignored", `FOO="bar" # comment` + "\n", map[string]string{"FOO": "bar"}},
		{"later duplicate wins", "FOO=one\nFOO=two\n", map[string]string{"FOO": "two"}},
		{"invalid name skipped, next line parsed", "1FOO=x\nFOO-BAR=y\nOK=1\n", map[string]string{"OK": "1"}},
		{"line without equals skipped", "not an assignment\nOK=1\n", map[string]string{"OK": "1"}},
		{"empty value", "FOO=\nBAR=1\n", map[string]string{"FOO": "", "BAR": "1"}},
		{"crlf endings", "FOO=bar\r\nBAZ=qux\r\n", map[string]string{"FOO": "bar", "BAZ": "qux"}},
		{"bom stripped", "\uFEFFFOO=bar\n", map[string]string{"FOO": "bar"}},
		{"unquoted value keeps inner spaces", "FOO=a b c\n", map[string]string{"FOO": "a b c"}},
		// The rest of the file is inside the unclosed string; parsing on
		// would manufacture variables out of string content.
		{"unterminated quote stops parsing", "FOO=\"never closed\nBAR=looks like an assignment\n", map[string]string{}},
		{"empty file", "", map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parse(tc.src)
			if len(got) != len(tc.want) {
				t.Fatalf("parse(%q) = %+v, want %+v", tc.src, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("parse(%q)[%s] = %q, want %q", tc.src, k, got[k], v)
				}
			}
		})
	}
}

// Walking up is direnv's own scope rule; the nearest .env wins.
func TestFindDotenvWalksUp(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	rootEnv := filepath.Join(root, ".env")
	if err := os.WriteFile(rootEnv, []byte("FOO=root\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := findDotenv(sub); got != rootEnv {
		t.Errorf("findDotenv(%q) = %q, want the parent's %q", sub, got, rootEnv)
	}

	nearer := filepath.Join(root, "a", ".env")
	if err := os.WriteFile(nearer, []byte("FOO=nearer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := findDotenv(sub); got != nearer {
		t.Errorf("findDotenv(%q) = %q, want the nearer %q", sub, got, nearer)
	}
}

// A directory named .env is a common virtualenv location (`python -m
// venv .env`); it must not stop the walk or be read as a file.
func TestFindDotenvSkipsVirtualenvDirectory(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	if err := os.MkdirAll(filepath.Join(proj, ".env"), 0o700); err != nil {
		t.Fatal(err)
	}
	rootEnv := filepath.Join(root, ".env")
	if err := os.WriteFile(rootEnv, []byte("FOO=root\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := findDotenv(proj); got != rootEnv {
		t.Errorf("findDotenv(%q) = %q, want %q (the .env directory must be skipped)", proj, got, rootEnv)
	}
}

// cleanDir returns a temp dir after confirming no ancestor directory
// carries a .env — the walk deliberately goes to the filesystem root,
// so a stray /tmp/.env on the machine would make "found nothing"
// untestable. That is a precondition, not a failure.
func cleanDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if got := findDotenv(dir); got != "" {
		t.Skipf("a real .env exists above the temp dir at %q", got)
	}
	return dir
}

func TestFindDotenvNone(t *testing.T) {
	if got := findDotenv(cleanDir(t)); got != "" {
		t.Errorf("findDotenv in an empty tree = %q, want empty", got)
	}
}

// for_dir must be the .env's directory, not the cwd — that is what
// keys the host's trust record to the subtree.
func TestEnvDiffForDirIsTheDotenvDirectory(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "nested", "deeper")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("PROBE=hello\nTOKEN='x y'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	resp, err := provider{}.EnvDiff(t.Context(), &pluginapi.EnvDiffRequest{Cwd: sub})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetForDir() != root {
		t.Errorf("for_dir = %q, want the .env's directory %q", resp.GetForDir(), root)
	}
	if resp.GetSet()["PROBE"] != "hello" || resp.GetSet()["TOKEN"] != "x y" {
		t.Errorf("set = %+v", resp.GetSet())
	}
	if len(resp.GetUnset()) != 0 {
		t.Errorf("a .env cannot unset, but proposed unset of %v", resp.GetUnset())
	}
}

// Everything empty answers with an empty response, never an error: the
// shell carries on, degradation is the contract.
func TestEnvDiffEmptyStates(t *testing.T) {
	empty := func(t *testing.T, req *pluginapi.EnvDiffRequest) {
		t.Helper()
		resp, err := provider{}.EnvDiff(t.Context(), req)
		if err != nil {
			t.Fatalf("EnvDiff returned an RPC error: %v", err)
		}
		if resp.GetForDir() != "" || len(resp.GetSet()) != 0 {
			t.Errorf("expected an empty proposal, got %+v", resp)
		}
	}

	t.Run("no cwd", func(t *testing.T) {
		empty(t, &pluginapi.EnvDiffRequest{})
	})
	t.Run("no .env anywhere", func(t *testing.T) {
		empty(t, &pluginapi.EnvDiffRequest{Cwd: cleanDir(t)})
	})
	t.Run("comments only", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("# nothing here\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		empty(t, &pluginapi.EnvDiffRequest{Cwd: dir})
	})
	t.Run("oversized file refused, not truncated", func(t *testing.T) {
		dir := t.TempDir()
		big := "FOO=bar\n" + strings.Repeat("#", maxDotenvSize)
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(big), 0o600); err != nil {
			t.Fatal(err)
		}
		empty(t, &pluginapi.EnvDiffRequest{Cwd: dir})
	})
}

// The plugin claims exactly what it serves: ENV, derived from the same
// struct that registers the service.
func TestDescribeClaimsEnvOnly(t *testing.T) {
	p := newPlugin()
	resp, err := p.Info.Describe(t.Context(), &pluginapi.DescribeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	caps := resp.GetCapabilities()
	if len(caps) != 1 || caps[0] != pluginapi.Capability_CAPABILITY_ENV {
		t.Errorf("capabilities = %v, want exactly [CAPABILITY_ENV]", caps)
	}
	if resp.GetName() != "koi-dotenv" {
		t.Errorf("name = %q", resp.GetName())
	}
}
