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

// RedactOutput removes credential-shaped spans from captured command
// output (#99 stage 3), returning the cleaned bytes and how many spans
// it replaced.
//
// The posture is deliberately *different* from the one above, and the
// difference is the whole point of having a second function rather than
// reusing the first.
//
// A command line that matches is never written at all. That is
// proportionate: the line is short, and if it carries a token then the
// token is essentially the whole command — there is nothing of value
// left once it is removed.
//
// Output is the opposite shape. It can be hundreds of kilobytes of
// build log, and the case blocks exists for is "show me what that build
// printed". Discarding all of it because one line echoed a token would
// destroy exactly the thing the user wanted, so matching spans are
// replaced and everything around them is kept.
//
// The count is returned so a caller can say the output was redacted
// rather than presenting a doctored log as if it were pristine.
func RedactOutput(out []byte) (redacted []byte, spans int) {
	for _, rule := range scrubRules {
		out = rule.re.ReplaceAllFunc(out, func(match []byte) []byte {
			spans++
			return []byte("[redacted:" + rule.name + "]")
		})
	}
	return out, spans
}
