package history

import (
	"bytes"
	"strings"
	"testing"
)

// Output redaction keeps the log and removes the secret. Dropping the
// whole thing — the posture for command lines — would destroy the
// case blocks exists for.
func TestRedactOutputKeepsTheLogAndRemovesTheSecret(t *testing.T) {
	log := []byte("compiling...\nexport GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz012345\nbuild failed: missing symbol\n")

	got, spans := RedactOutput(log)
	if spans == 0 {
		t.Fatal("no redaction happened")
	}
	if bytes.Contains(got, []byte("ghp_abcdefghijklmnopqrstuvwxyz012345")) {
		t.Errorf("the token survived: %s", got)
	}
	for _, keep := range []string{"compiling...", "build failed: missing symbol"} {
		if !bytes.Contains(got, []byte(keep)) {
			t.Errorf("redaction ate %q, which is the part worth keeping:\n%s", keep, got)
		}
	}
	if !bytes.Contains(got, []byte("[redacted:")) {
		t.Errorf("redaction left no marker, so the log reads as pristine:\n%s", got)
	}
}

// Clean output is returned untouched and reports nothing, so a caller
// can distinguish "nothing to hide" from "hidden something".
func TestRedactOutputLeavesCleanLogsAlone(t *testing.T) {
	log := []byte("ok\nall tests passed\n")
	got, spans := RedactOutput(log)
	if spans != 0 {
		t.Errorf("clean log reported %d redactions", spans)
	}
	if !bytes.Equal(got, log) {
		t.Errorf("clean log was rewritten: %q", got)
	}
}

// Every rule that guards command lines must also guard output; a rule
// that only ran on one of the two would be a hole nobody could see.
func TestRedactOutputCoversEveryRule(t *testing.T) {
	samples := map[string]string{
		"aws-access-key-id": "AKIAIOSFODNN7EXAMPLE",
		"github-token":      "ghp_abcdefghijklmnopqrstuvwxyz012345",
		"github-pat":        "github_pat_abcdefghijklmnopqrstuvwxyz",
		"slack-token":       "xoxb-1234567890-abcdefghij",
		"stripe-key":        "sk_live_abcdefghijklmnop",
		"google-api-key":    "AIzaSyA1234567890abcdefghijklmnopqrstuv",
		"bearer-token":      "Bearer abcdefghijklmnopqrstuvwxyz123456",
		// Assembled from parts: a contiguous literal trips the repo's
		// own detect-private-key hook, which is working as intended.
		"private-key":           "-----" + "BEGIN RSA PRIVATE" + " KEY-----",
		"credential-assignment": "api_key=supersecretvalue123",
	}
	for name, sample := range samples {
		t.Run(name, func(t *testing.T) {
			got, spans := RedactOutput([]byte("before " + sample + " after"))
			if spans == 0 {
				t.Fatalf("rule %s did not fire on %q", name, sample)
			}
			if bytes.Contains(got, []byte(sample)) {
				t.Errorf("%s survived: %s", name, got)
			}
			if !bytes.Contains(got, []byte("before ")) || !bytes.Contains(got, []byte(" after")) {
				t.Errorf("surrounding output was destroyed: %s", got)
			}
		})
	}
	if len(samples) != len(scrubRules) {
		t.Errorf("%d samples for %d rules — a rule has no output coverage", len(samples), len(scrubRules))
	}
}

// Multiple secrets in one log are all removed, not just the first.
func TestRedactOutputRemovesEveryOccurrence(t *testing.T) {
	log := []byte("a ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa b ghp_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb c")
	got, spans := RedactOutput(log)
	if spans < 2 {
		t.Errorf("redacted %d spans, want at least 2", spans)
	}
	if strings.Contains(string(got), "ghp_") {
		t.Errorf("a token survived: %s", got)
	}
}
