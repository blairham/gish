package history

import "regexp"

// Secret scrubbing (#10): a command that looks like it carries a
// credential is never written to the history file — the same posture as
// ignorespace, applied automatically. Rules are gitleaks-style but
// deliberately compact: high-signal token shapes plus a generic
// assignment pattern. HistoryBackend plugins only ever receive entries
// that passed this gate.
var scrubRules = []struct {
	name string
	re   *regexp.Regexp
}{
	{"aws-access-key-id", regexp.MustCompile(`\b(A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}\b`)},
	{"github-token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)},
	{"github-pat", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z-]{10,}`)},
	{"stripe-key", regexp.MustCompile(`\b[sr]k_(live|test)_[A-Za-z0-9]{10,}\b`)},
	{"google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"bearer-token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{20,}`)},
	{"private-key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	// Generic KEY=value assignments. The value must hug the separator —
	// `password= --flag` is not an assignment — and values that are shell
	// expansions ($VAR, $(cmd)) are references, not secrets.
	{"credential-assignment", regexp.MustCompile(`(?i)[\w-]*(api[_-]?key|secret|token|passw(or)?d)[\w-]*[=:]['"]?[^$\s'"][^\s'"]{7,}`)},
}

// SecretReason reports the first scrub rule a command matches, or "" if
// it is clean.
//
// Exported so that anything else persisting a command line applies the
// same gate the history store does. The #10 guarantee is "a
// secret-bearing command is never recorded", and that has to mean every
// place a command is recorded — a second store with its own idea of
// what is safe is how the guarantee quietly stops being true.
func SecretReason(command string) string { return scrubReason(command) }

// scrubReason reports the first matching rule name, or "" for a clean
// command.
func scrubReason(command string) string {
	for _, rule := range scrubRules {
		if rule.re.MatchString(command) {
			return rule.name
		}
	}
	return ""
}
