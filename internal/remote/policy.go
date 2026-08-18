package remote

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Policy: whether to drop a binary on a given host.
//
// The default is ask-once-per-host, and the reason is good citizenship
// rather than caution theater. Auto-hijacking `ssh` would drop
// executables on servers people do not own — that trips file-integrity
// monitoring (Tripwire, Wazuh) and violates change control at plenty of
// shops. So `koi ssh` is explicit, `ssh` is never shadowed by default,
// and the first visit to a host asks.

// Bring modes, the values of KOI_SSH_BRING.
const (
	BringAsk    = "ask"
	BringAlways = "always"
	BringNever  = "never"
)

// hostDecisions is the remembered per-host answer file.
type hostDecisions struct {
	Hosts map[string]bool `json:"hosts"`
}

// DecisionsPath is where remembered per-host answers live. This is
// derived preference state, not security state: a corrupt file resets to
// "ask again", which is the safe direction.
func DecisionsPath() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "koi", "ssh-hosts.json")
}

func loadDecisions() hostDecisions {
	d := hostDecisions{Hosts: map[string]bool{}}
	path := DecisionsPath()
	if path == "" {
		return d
	}
	data, err := os.ReadFile(path) //nolint:gosec // our own state file
	if err != nil {
		return d
	}
	if err := json.Unmarshal(data, &d); err != nil || d.Hosts == nil {
		return hostDecisions{Hosts: map[string]bool{}}
	}
	return d
}

func saveDecisions(d hostDecisions) error {
	path := DecisionsPath()
	if path == "" {
		return errors.New("no data directory")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename, as everywhere else koi persists state.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Decide answers whether to bring koi to host. mode is KOI_SSH_BRING;
// ask consults the remembered answer first and only then the user, via
// prompt (which a test supplies). A non-terminal session never asks —
// an unattended `koi ssh` in a script must not block on a question.
func Decide(host, mode string, interactive bool, prompt func(string) (bool, error)) (bring, remembered bool, err error) {
	switch mode {
	case BringNever:
		return false, true, nil
	case BringAlways:
		return true, true, nil
	}
	d := loadDecisions()
	if answer, ok := d.Hosts[host]; ok {
		return answer, true, nil
	}
	if !interactive || prompt == nil {
		// Unasked and unanswerable: do the conservative thing and leave
		// the host untouched. Nothing is remembered, so an interactive
		// visit later still gets to ask.
		return false, false, nil
	}
	answer, err := prompt(host)
	if err != nil {
		return false, false, err
	}
	d.Hosts[host] = answer
	if err := saveDecisions(d); err != nil {
		// Failing to remember is not a reason to fail the session.
		return answer, false, nil
	}
	return answer, true, nil
}

// Forget drops the remembered answer for host, so the next visit asks.
func Forget(host string) error {
	d := loadDecisions()
	if _, ok := d.Hosts[host]; !ok {
		return nil
	}
	delete(d.Hosts, host)
	return saveDecisions(d)
}

// AskOnTerminal is the default prompt: one question, default no. The
// default matters — someone typing `koi ssh prod` while half awake
// should not drop a binary on prod by hitting Enter.
func AskOnTerminal(in io.Reader, out io.Writer) func(string) (bool, error) {
	return func(host string) (bool, error) {
		fmt.Fprintf(out, "koi: bring koi to %s? It copies one file to a cache dir\n", host)
		fmt.Fprintf(out, "      under your home there and nothing else. [y/N/never] ")
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(out)
			return false, nil
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true, nil
		default:
			return false, nil
		}
	}
}
