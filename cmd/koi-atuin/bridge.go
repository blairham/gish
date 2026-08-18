package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The atuin CLI adapter. Everything that knows atuin's command-line
// shape lives here, and every fact in it was read off atuin's own
// clap definitions rather than guessed — the two that would corrupt
// data silently are called out below.

// atuinBin is the binary to drive. A missing atuin is not an error: the
// plugin describes itself, serves nothing, and the shell's local history
// keeps working — the same posture koi-carapace takes.
const atuinBin = "atuin"

// recordSep and fieldSep frame search output. atuin's --print0
// terminates each *result* with a NUL, which exists precisely because
// commands can contain newlines; a tab separates fields within a
// record, and a command containing a tab only costs us a mis-split
// field, never a mis-split record.
const (
	recordSep = "\x00"
	fieldSep  = "\t"
)

// searchFormat asks for exactly the fields koi's HistoryEntry carries.
// Field order matters: command comes last so that a tab inside a command
// cannot shift any other field.
const searchFormat = "{exit}\t{time}\t{directory}\t{command}"

type bridge struct {
	// once guards the availability probe: LookPath every call would be a
	// syscall per keystroke on the ctrl-r path.
	once      sync.Once
	available bool
	path      string
}

func (b *bridge) resolve() (string, bool) {
	b.once.Do(func() {
		p, err := exec.LookPath(atuinBin)
		b.path, b.available = p, err == nil
	})
	return b.path, b.available
}

// run executes atuin with a deadline inherited from the caller's
// context. WaitDelay bounds the pipe drain after a kill: killing a
// process on deadline does not make Wait return on deadline if a
// grandchild still holds the write end.
func (b *bridge) run(ctx context.Context, stdinEnv []string, args ...string) ([]byte, error) {
	path, ok := b.resolve()
	if !ok {
		return nil, errNoAtuin
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(os.Environ(), stdinEnv...)
	cmd.WaitDelay = 250 * time.Millisecond
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("atuin %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// start opens a history record and returns atuin's id for it.
//
// The command travels in ATUIN_COMMAND_LINE rather than argv:
// --command-from-env exists in atuin precisely because a command line
// "does not need escaping and is more compatible between OS and
// shells". A shell's history is exactly the corpus full of quotes,
// newlines, and backslashes, so the escaping-free path is the only
// correct one here.
func (b *bridge) start(ctx context.Context, command string) (string, error) {
	out, err := b.run(ctx, []string{"ATUIN_COMMAND_LINE=" + command},
		"history", "start", "--command-from-env")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("atuin history start returned no id")
	}
	return id, nil
}

// end closes the record with its status and duration.
//
// **atuin's --duration is nanoseconds**, and koi's HistoryEntry carries
// milliseconds. This conversion is the single most corruptible line in
// the bridge: getting it wrong by 10^6 makes every command in the user's
// synced history read as instant, and nothing would ever error.
func (b *bridge) end(ctx context.Context, id string, exitCode int32, durationMs int64) error {
	_, err := b.run(ctx, nil, endArgs(id, exitCode, durationMs)...)
	return err
}

// endArgs builds the argv, split out so the unit conversion is testable
// without an atuin to run.
func endArgs(id string, exitCode int32, durationMs int64) []string {
	args := []string{"history", "end", id, "--exit", strconv.Itoa(int(exitCode))}
	if durationMs > 0 {
		// ms → ns. A zero duration is omitted rather than sent as 0:
		// koi not having measured a duration is different from the
		// command having taken no time, and atuin fills in its own.
		args = append(args, "--duration", strconv.FormatInt(durationMs*1_000_000, 10))
	}
	return args
}

// searchResult is one row parsed back out of atuin.
type searchResult struct {
	Command  string
	Cwd      string
	ExitCode int32
	// StartedUnixMs is 0 when atuin's time string did not parse; the
	// picker already treats that as "no age known" and shows the row
	// without an age rather than dropping it.
	StartedUnixMs int64
}

// search asks atuin for matches.
//
// --filter-mode global is deliberate: atuin's configured default may
// scope results to the current session or host, and a user who installed
// this bridge wants the machine-spanning history that is atuin's whole
// reason for existing. --include-duplicates is *not* passed — atuin
// dedupes by default, and a ctrl-r list is more useful deduped.
func (b *bridge) search(ctx context.Context, query, cwd string, limit int, prefixOnly bool) ([]searchResult, error) {
	args := []string{
		"search",
		"--limit", strconv.Itoa(limit),
		"--format", searchFormat,
		"--print0",
		"--filter-mode", "global",
	}
	if prefixOnly {
		// Up-arrow semantics: match the beginning of the command, which
		// atuin spells as its prefix search mode.
		args = append(args, "--search-mode", "prefix")
	}
	if cwd != "" {
		// Not a filter — atuin ranks by locality when it knows the cwd,
		// and koi's picker shows the directory column regardless.
		args = append(args, "--cwd", cwd)
	}
	if query != "" {
		// Terminate option parsing: a query beginning with '-' is a
		// perfectly ordinary thing to search for.
		args = append(args, "--", query)
	}

	out, err := b.run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	return parseSearch(string(out)), nil
}

// parseSearch turns atuin's NUL-terminated records into entries. It is
// deliberately forgiving: a row it cannot read is skipped, never fatal.
// Search feeds an interactive picker, and one malformed record must not
// cost the user the other forty.
func parseSearch(out string) []searchResult {
	var results []searchResult
	for _, rec := range strings.Split(out, recordSep) {
		rec = strings.Trim(rec, "\n")
		if rec == "" {
			continue
		}
		// SplitN with the field count: the command is last and keeps any
		// tabs it contains.
		parts := strings.SplitN(rec, fieldSep, 4)
		if len(parts) < 4 {
			continue
		}
		r := searchResult{Cwd: parts[2], Command: parts[3]}
		if code, err := strconv.Atoi(strings.TrimSpace(parts[0])); err == nil {
			r.ExitCode = int32(code)
		}
		r.StartedUnixMs = parseAtuinTime(strings.TrimSpace(parts[1]))
		if r.Command == "" {
			continue
		}
		results = append(results, r)
	}
	return results
}

// atuinTimeLayouts covers the shapes atuin's {time} takes. Its exact
// rendering follows the user's own time_format and timezone settings, so
// this is best-effort by design: an unparsed timestamp costs the age
// column on that row and nothing else, which is a far better failure
// than refusing the row or guessing a time.
var atuinTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999 -0700",
	"2006-01-02 15:04:05 -0700",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
}

func parseAtuinTime(s string) int64 {
	if s == "" {
		return 0
	}
	for _, layout := range atuinTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}
