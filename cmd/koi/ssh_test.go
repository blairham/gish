package main

import (
	"slices"
	"testing"
)

// `koi ssh` has to be a transparent front for ssh: anything koi does
// not claim is passed through untouched. The failure that matters is
// getting the *host* wrong — `koi ssh -o ConnectTimeout=1 web` reading
// "ConnectTimeout=1" as the hostname would connect to nothing and, worse,
// remember a decision for a host that does not exist.
func TestParseSSHArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantHost string
		wantSSH  []string
		wantFlag func(sshInvocation) bool
	}{
		{
			name:     "bare host",
			args:     []string{"web-1"},
			wantHost: "web-1",
		},
		{
			name:     "user@host",
			args:     []string{"deploy@web-1"},
			wantHost: "deploy@web-1",
		},
		{
			name:     "option with a separate value does not become the host",
			args:     []string{"-o", "ConnectTimeout=1", "web-1"},
			wantHost: "web-1",
			wantSSH:  []string{"-o", "ConnectTimeout=1"},
		},
		{
			name:     "port and identity",
			args:     []string{"-p", "2222", "-i", "~/.ssh/id_ed25519", "web-1"},
			wantHost: "web-1",
			wantSSH:  []string{"-p", "2222", "-i", "~/.ssh/id_ed25519"},
		},
		{
			name:     "jump host",
			args:     []string{"-J", "bastion", "web-1"},
			wantHost: "web-1",
			wantSSH:  []string{"-J", "bastion"},
		},
		{
			name:     "attached value carries its own argument",
			args:     []string{"-p2222", "web-1"},
			wantHost: "web-1",
			wantSSH:  []string{"-p2222"},
		},
		{
			name:     "valueless flags stay valueless",
			args:     []string{"-v", "-4", "web-1"},
			wantHost: "web-1",
			wantSSH:  []string{"-v", "-4"},
		},
		{
			name:     "koi flags are claimed, not forwarded",
			args:     []string{"--uninstall", "web-1"},
			wantHost: "web-1",
			wantFlag: func(i sshInvocation) bool { return i.uninstall },
		},
		{
			name:     "koi flags mix with ssh flags",
			args:     []string{"--ephemeral", "-o", "User=root", "web-1"},
			wantHost: "web-1",
			wantSSH:  []string{"-o", "User=root"},
			wantFlag: func(i sshInvocation) bool { return i.ephemeral },
		},
		{
			name:     "forget",
			args:     []string{"--forget", "web-1"},
			wantHost: "web-1",
			wantFlag: func(i sshInvocation) bool { return i.forget },
		},
		{
			name: "no host",
			args: []string{"-v"},
			// host stays empty; runSSH turns that into usage + exit 2.
			wantSSH: []string{"-v"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSSHArgs(tt.args)
			if got.host != tt.wantHost {
				t.Errorf("host = %q, want %q", got.host, tt.wantHost)
			}
			if !slices.Equal(got.sshArgs, tt.wantSSH) {
				t.Errorf("sshArgs = %q, want %q", got.sshArgs, tt.wantSSH)
			}
			if tt.wantFlag != nil && !tt.wantFlag(got) {
				t.Errorf("koi flag not set from %q", tt.args)
			}
		})
	}
}

// A koi flag must never be forwarded to ssh, which would reject it.
func TestKoiFlagsAreNotForwardedToSSH(t *testing.T) {
	got := parseSSHArgs([]string{"--uninstall", "--ephemeral", "--forget", "host"})
	for _, a := range got.sshArgs {
		switch a {
		case "--uninstall", "--ephemeral", "--forget":
			t.Errorf("%s was forwarded to ssh", a)
		}
	}
}
