package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAWSHome builds a ~/.aws tree with config, credentials, and an
// SSO token cache entry.
func fakeAWSHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	aws := filepath.Join(home, ".aws")
	if err := os.MkdirAll(filepath.Join(aws, "sso", "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `# comment
[default]
region = us-east-1

[profile prod]
region = eu-west-1
sso_session = corp

[profile staging]
sso_start_url = https://legacy.awsapps.com/start

[sso-session corp]
sso_start_url = https://corp.awsapps.com/start
`
	if err := os.WriteFile(filepath.Join(aws, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	creds := "[creds-only]\naws_access_key_id = AKIAFAKEFAKEFAKEFAKE\n"
	if err := os.WriteFile(filepath.Join(aws, "credentials"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	token := `{"startUrl":"https://corp.awsapps.com/start","expiresAt":"` +
		time.Now().Add(45*time.Minute).UTC().Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(aws, "sso", "cache", "abc.json"), []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestLoadConfig(t *testing.T) {
	t.Parallel()

	home := fakeAWSHome(t)
	cfg := newLoader(home).Load()

	names := cfg.ProfileNames()
	want := []string{"creds-only", "default", "prod", "staging"}
	if len(names) != 4 || names[0] != want[0] || names[3] != want[3] {
		t.Errorf("profiles = %q, want %q", names, want)
	}
	if cfg.Profiles["prod"].Region != "eu-west-1" || cfg.Profiles["prod"].SSOSession != "corp" {
		t.Errorf("prod = %+v", cfg.Profiles["prod"])
	}
	if got := cfg.startURL("prod"); got != "https://corp.awsapps.com/start" {
		t.Errorf("prod start url via session = %q", got)
	}
	if got := cfg.startURL("staging"); got != "https://legacy.awsapps.com/start" {
		t.Errorf("staging inline start url = %q", got)
	}
}

func TestSegmentText(t *testing.T) {
	t.Parallel()

	home := fakeAWSHome(t)
	cfg := newLoader(home).Load()
	now := time.Now()

	// Explicit profile with SSO: profile@region plus freshness.
	got := segmentText(cfg, home, map[string]string{"AWS_PROFILE": "prod"}, now)
	if got != "aws:prod@eu-west-1 sso:44m" && got != "aws:prod@eu-west-1 sso:45m" {
		t.Errorf("segment = %q", got)
	}
	// Env region beats profile region.
	got = segmentText(cfg, home, map[string]string{"AWS_PROFILE": "prod", "AWS_REGION": "us-west-2"}, now)
	if !contains(got, "@us-west-2") {
		t.Errorf("env region not honored: %q", got)
	}
	// Expired token shows the failure, not a countdown.
	got = segmentText(cfg, home, map[string]string{"AWS_PROFILE": "prod"}, now.Add(2*time.Hour))
	if !contains(got, "sso✗") {
		t.Errorf("expired token not marked: %q", got)
	}
	// No explicit profile but a configured default: renders.
	if got = segmentText(cfg, home, nil, now); got != "aws:default@us-east-1" {
		t.Errorf("default segment = %q", got)
	}
	// No AWS setup at all: silent.
	empty := newLoader(t.TempDir()).Load()
	if got = segmentText(empty, t.TempDir(), nil, now); got != "" {
		t.Errorf("unconfigured box rendered %q", got)
	}
}

func TestCompleteLine(t *testing.T) {
	t.Parallel()

	cfg := newLoader(fakeAWSHome(t)).Load()

	got := completeLine(cfg, "aws s3 ls --profile ")
	if len(got) != 4 {
		t.Fatalf("all profiles expected: %+v", got)
	}
	got = completeLine(cfg, "aws s3 ls --profile pr")
	if len(got) != 1 || got[0].GetValue() != "prod" {
		t.Errorf("prefix filter = %+v", got)
	}
	if got = completeLine(cfg, "aws ec2 describe-instances --region eu-w"); len(got) != 3 {
		t.Errorf("region prefixes = %+v", got)
	}
	// Not an aws line, or no flag: stay quiet.
	if got = completeLine(cfg, "git commit --profile x"); got != nil {
		t.Errorf("non-aws line completed: %+v", got)
	}
	if got = completeLine(cfg, "aws s3 ls "); got != nil {
		t.Errorf("flagless position completed: %+v", got)
	}
}

func TestFindProfileFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aws-profile"), []byte("# team default\nprod eu-west-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, profile, region := findProfileFile(nested)
	if dir != root || profile != "prod" || region != "eu-west-1" {
		t.Errorf("findProfileFile = %q %q %q", dir, profile, region)
	}
	if dir, _, _ := findProfileFile(t.TempDir()); dir != "" {
		t.Errorf("no file should mean no proposal, got %q", dir)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
