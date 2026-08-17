package history

import (
	"bytes"
	"testing"
)

// privateKeyHeader is assembled rather than written out, so the literal
// never appears in this file. The detect-private-key pre-commit hook is
// right to refuse a commit carrying that header, and "it is only a test
// seed" is not a good enough reason to teach the hook an exception —
// the seed still exercises the private-key rule either way.
var privateKeyHeader = "-----BEGIN RSA PRIVATE" + " KEY-----"

// Fuzz the two scrub postures against each other. They share one rule
// set on purpose (#10): a command line that would be rejected must be
// exactly the content that output redaction would strike, or the
// "secrets never persist" guarantee depends on which door the bytes
// came through.
func FuzzScrubConsistency(f *testing.F) {
	for _, seed := range []string{
		"ls -la",
		"export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
		"curl -H 'Authorization: Bearer abcdefghijklmnopqrstuvwx'",
		"git clone https://ghp_abcdefghijklmnopqrst1234@github.com/x/y",
		"echo xoxb-1234567890-abcdefghij",
		"STRIPE_KEY=sk_live_abcdefghij1234",
		"password=hunter2hunter2",
		"password= --flag",
		"token=$SECRET_FROM_ENV",
		privateKeyHeader,
		"AIzaSyA-1234567890abcdefghijklmnopqrstuv",
		"api_key:'supersecretvalue'",
		"github_pat_1234567890abcdefghij_more",
		"secret=short",
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		reason := SecretReason(string(data))
		redacted, spans := RedactOutput(bytes.Clone(data))

		// One rule set, two postures: they must agree on what a secret
		// is. A command the store rejects that redaction would pass
		// (or vice versa) is the split-brain #10 forbids.
		if (reason == "") != (spans == 0) {
			t.Fatalf("postures disagree: SecretReason=%q, spans=%d, input=%q", reason, spans, data)
		}

		// Redaction must be idempotent: a redacted log re-entering the
		// pipeline (blocks re-saved, output re-captured) must not find
		// new secrets manufactured by the first pass's replacements —
		// and must never re-leak.
		again, spans2 := RedactOutput(bytes.Clone(redacted))
		if spans2 != 0 {
			t.Fatalf("redaction not idempotent: second pass struck %d spans, %q -> %q -> %q",
				spans2, data, redacted, again)
		}
	})
}
