package history

import "testing"

func TestScrubReasonCatchesSecrets(t *testing.T) {
	t.Parallel()

	// Fake tokens are assembled by concatenation so that GitHub's push
	// protection (which scans raw blob text) doesn't flag the test data
	// for the very scanner it exercises.
	secret := []string{
		"aws configure set aws_access_key_id AKIA" + "IOSFODNN7EXAMPLE",
		"export GITHUB_TOKEN=ghp_" + "abcdefghijklmnopqrstuvwxyz123456",
		"curl -H 'Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6'",
		"echo xoxb-" + "123456789012-abcdefghijklmnop",
		"stripe listen --api-key sk_live_" + "abcdefghij1234",
		"export API_KEY=super-secret-value-1234",
		"mysql -u root --password=hunter2hunter2",
		`echo "-----BEGIN RSA PRIVATE ` + `KEY-----" > key.pem`,
		"gcloud auth activate --key AIza" + "SyA1234567890abcdefghijklmnopqrstuv",
	}
	for _, cmd := range secret {
		if scrubReason(cmd) == "" {
			t.Errorf("not caught: %q", cmd)
		}
	}
}

func TestScrubReasonAllowsNormalCommands(t *testing.T) {
	t.Parallel()

	clean := []string{
		"git commit -m 'rotate the api key handling'",
		"make test",
		"echo $GITHUB_TOKEN",            // variable reference, not a value
		"export API_KEY=$VAULT_API_KEY", // expansion, not a literal
		"export TOKEN=$(vault read x)",  // command substitution
		"grep -r password= --include=*.go .",
		"man git-credential",
		"echo secret",
	}
	for _, cmd := range clean {
		if reason := scrubReason(cmd); reason != "" {
			t.Errorf("false positive (%s): %q", reason, cmd)
		}
	}
}

func TestAppendSkipsSecrets(t *testing.T) {
	t.Parallel()

	s := openStore(t)
	skip, err := s.Append(Entry{Command: "export API_KEY=super-secret-value-1234"})
	if err != nil {
		t.Fatal(err)
	}
	if skip != SkipSecret {
		t.Fatalf("skip = %v, want SkipSecret", skip)
	}
	if _, ok := s.Match("", 0); ok {
		t.Error("secret command reached the store")
	}

	skip, err = s.Append(Entry{Command: "echo fine"})
	if err != nil || skip != SkipNone {
		t.Fatalf("clean append = %v, %v", skip, err)
	}
}

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/h.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
