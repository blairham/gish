package repl

import (
	"bufio"
	"os"
	"strings"
)

// The four `compgen -A` actions that answer from outside the shell
// (#606): `user`, `group`, `service` and `hostname`.
//
// The other six the issue named are koi's own tables and are answered
// from them; these read the system, and the question the issue asked was
// whether koi should read it at all. It should, and the reason is that
// bash's answer here is not bash's opinion — `compgen -u` is
// `getpwent(3)` and `compgen -A hostname` is `$HOSTFILE`, so the honest
// answer is the same database, not a refusal saying koi does not look.
// A refusal would also be a *worse* answer than the empty list it
// replaced: `_usergroup` in bash-completion calls `compgen -u` and would
// then print a diagnostic into the middle of someone's completion.
//
// koi reads the POSIX database *files* rather than calling libc, which
// is a divergence worth stating: on a box whose users come from a
// directory service — every macOS install, LDAP or SSSD on Linux —
// getpwent answers with more names than /etc/passwd holds (265 against
// 132 on the darwin machine this was measured on). So koi's list is a
// subset of bash's there and equal to it on a files-only box, which is
// what the tests assert rather than equality. Calling getpwent would
// mean cgo, and Go's os/user has no enumeration to borrow.
//
// The failure shape is bash's and was measured: an unreadable or missing
// file generates nothing and exits 1, which is exactly what bash answers
// for `HOSTFILE=/nope`. Nothing is printed, because a completion
// function is the caller.

// passwdFile, groupFile and servicesFile are the databases. Named
// constants so a test can say which file an answer came from.
const (
	passwdFile   = "/etc/passwd"
	groupFile    = "/etc/group"
	servicesFile = "/etc/services"
	hostsFile    = "/etc/hosts"
)

// dbNames reads the first colon-separated field of every data line in a
// POSIX database file — the name column of both passwd and group — in
// file order, keeping duplicates.
//
// Order and duplicates are deliberate: bash generates candidates in the
// order its source hands them over and de-duplicates nothing (#613), and
// `compgen -A service` on this machine answers 9869 names of which 4969
// are distinct, because /etc/services lists a name once per protocol.
func dbNames(path string) []string {
	f, err := os.Open(path) //nolint:gosec // a fixed system database path
	if err != nil {
		return nil
	}
	defer f.Close() //nolint:errcheck // read-only
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		// A `#` comment and a blank line are the two things every one of
		// these files allows; `+`/`-` NIS compat lines are names in bash
		// too, since getpwent hands them over as it finds them.
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		name, _, _ := strings.Cut(trimmed, ":")
		if name = strings.TrimRight(name, " \t"); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// serviceNames reads /etc/services: whitespace-separated, and the name
// is the first field of each entry line.
//
// The aliases after the port are deliberately not listed, which was
// measured rather than assumed — `nicname 43/tcp whois` puts `nicname`
// in bash's listing twice and `whois` not at all, because getservent
// reports one name per entry and bash prints that one.
//
// An entry needs a `port/proto` field after the name, which is not
// pedantry: darwin's /etc/services has 20 reserved-port lines with no
// name at all (`\t\t1023/tcp\t# Reserved`), and reading the first field
// blindly answers `1023/tcp` as a service name — twenty candidates
// getservent never reports.
func serviceNames() []string {
	f, err := os.Open(servicesFile)
	if err != nil {
		return nil
	}
	defer f.Close() //nolint:errcheck // read-only
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if !strings.Contains(fields[1], "/") {
			continue // no port/proto: this line names no service
		}
		out = append(out, fields[0])
	}
	return out
}

// hostNames reads the host database `compgen -A hostname` answers from:
// $HOSTFILE when it is set and non-empty, /etc/hosts otherwise.
//
// The parse is readline's `snarf_hosts`, and its one surprising rule was
// measured rather than read: the first field of a line is skipped only
// when it **starts with a digit**. So `1.2.3.4 host` lists `host` alone
// and `::1 localhost` lists *both* `::1` and `localhost` — an IPv6
// address is a hostname candidate in bash — as is `abc.def x`'s first
// field. A `#` at the start of a field ends the line, while `a#b` is one
// name.
//
// bash caches this list for the session and appends to it when HOSTFILE
// changes, so a second file's names arrive *after* the first file's and
// the first file's stay. koi re-reads instead: the file is small, it is
// on no keystroke path here, and a stale cache would answer with hosts
// the user has deleted. That divergence is stated rather than copied.
func hostNames(env func(string) string) []string {
	path := hostsFile
	if h := env("HOSTFILE"); h != "" {
		path = h
	}
	f, err := os.Open(path) //nolint:gosec // the path is the caller's HOSTFILE, as in bash
	if err != nil {
		return nil
	}
	defer f.Close() //nolint:errcheck // read-only
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		for i, field := range fields {
			if strings.HasPrefix(field, "#") {
				break
			}
			if i == 0 && field[0] >= '0' && field[0] <= '9' {
				continue // the address
			}
			out = append(out, field)
		}
	}
	return out
}
