package main

import "testing"

func TestSplitCandidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, in, cmd, why string
	}{
		{"clean", "du -sh *\n# sizes of everything\n", "du -sh *", "sizes of everything"},
		{"fenced despite instructions", "```sh\nls -la\n```\n", "ls -la", ""},
		{"dollar prompt stripped", "$ make test\n", "make test", ""},
		{"no command", "\n\n", "", ""},
		{"rationale only after command", "# stray comment\nls\n# real rationale\n", "ls", "real rationale"},
	}
	for _, tt := range tests {
		cmd, why := splitCandidate(tt.in)
		if cmd != tt.cmd || why != tt.why {
			t.Errorf("%s: splitCandidate = %q / %q, want %q / %q", tt.name, cmd, why, tt.cmd, tt.why)
		}
	}
}
