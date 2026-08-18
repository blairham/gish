package repl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/history"
	"github.com/blairham/koi-shell/internal/migrate"
)

// The `migrate` command (#160): import an existing bash or zsh setup.
//
// It previews by default and writes only with --apply, for the same
// reason `trust` asks before applying an env diff: the thing being
// imported came from a file the user has not read in years, and the
// report is what makes adopting it a decision rather than a leap.
//
// Nothing here executes the rc files. See internal/migrate.

const migrateUsage = `usage: migrate [--apply] [--history] [--force]

  migrate             read your bash/zsh setup and print what would be imported
  migrate --apply     write the koi rc file (refuses to clobber an existing one)
  migrate --history   also import your shell history (secrets are dropped)
  migrate --force     overwrite an existing koi rc file

Nothing is executed: your rc files are parsed, and anything that does
not translate is listed with the reason.`

func migrateCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "migrate" {
			return next(ctx, args)
		}
		hc := interp.HandlerCtx(ctx)
		if err := RunMigrate(hc.Stdout, hc.Stderr, args[1:]); err != nil {
			fmt.Fprintln(hc.Stderr, "migrate:", err)
			return []string{"false"}, nil
		}
		return []string{"true"}, nil
	}
}

// RunMigrate is the whole command, exported so `koi migrate` reaches
// it without a shell session — the case where someone is evaluating
// koi before they have started using it, which is exactly when this
// command matters most.
func RunMigrate(out, errOut io.Writer, args []string) error {
	apply, importHistory, force := false, false, false
	for _, a := range args {
		switch a {
		case "--apply", "-a":
			apply = true
		case "--history":
			importHistory = true
		case "--force", "-f":
			force = true
		case "--help", "-h", "help":
			fmt.Fprintln(out, migrateUsage)
			return nil
		default:
			return fmt.Errorf("unknown option %q\n%s", a, migrateUsage)
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	plan, err := migrate.Detect(home)
	if err != nil {
		return err
	}
	fmt.Fprint(out, plan.Report())

	if !apply {
		fmt.Fprintln(out, "\nnothing written. Run `migrate --apply` to write the rc file"+
			historyHint(plan, importHistory))
		return nil
	}

	path, err := migrateRCPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists — read it first, then `migrate --apply --force` to replace it", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(plan.KoiRC()), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nwrote %s\n", path)

	if importHistory {
		n, skipped, err := importShellHistory(plan)
		if err != nil {
			fmt.Fprintln(errOut, "migrate: history:", err)
		} else {
			fmt.Fprintf(out, "imported %d history entries (%d skipped: duplicates and possible secrets)\n", n, skipped)
		}
	}
	return nil
}

func historyHint(plan *migrate.Plan, importHistory bool) string {
	if plan.HistoryIn == "" || importHistory {
		return ""
	}
	return ", or `migrate --apply --history` to bring your history too"
}

// migrateRCPath is where an imported config goes: the XDG rc, which is
// the one koi creates for `config` too.
func migrateRCPath() (string, error) {
	if p := os.Getenv("KOI_RC"); p != "" {
		return p, nil
	}
	confHome := os.Getenv("XDG_CONFIG_HOME")
	if confHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		confHome = filepath.Join(home, ".config")
	}
	return filepath.Join(confHome, "koi", "koirc"), nil
}

// importShellHistory copies the old shell's history into koi's store.
//
// It goes through Store.Append rather than writing JSONL directly, so
// the #10 secret rules apply to imported commands exactly as they do to
// typed ones. A history file is the single most likely place for an
// exported token to be sitting, and an importer that bypassed the
// scrubber would be the one path that puts one back on disk.
func importShellHistory(plan *migrate.Plan) (imported, skipped int, err error) {
	if plan.HistoryIn == "" {
		return 0, 0, errors.New("no history file found")
	}
	data, err := os.ReadFile(plan.HistoryIn) //nolint:gosec // the user's own history, on request
	if err != nil {
		return 0, 0, err
	}
	path, err := history.DefaultPath()
	if err != nil {
		return 0, 0, err
	}
	store, err := history.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer store.Close() //nolint:errcheck // entries are flushed per append

	for _, e := range migrate.ParseHistory(string(data)) {
		started := e.UnixSec * 1000
		if e.UnixSec == 0 {
			// No timestamp in the source format. Zero would sort these
			// to the beginning of time, which is honest but useless;
			// the import moment is the best fact available.
			started = time.Now().UnixMilli()
		}
		skip, aerr := store.Append(history.Entry{
			Command:       e.Command,
			StartedUnixMs: started,
			DurationMs:    e.DurationMs,
			SessionID:     "migrated",
		})
		switch {
		case aerr != nil:
			return imported, skipped, aerr
		case skip != history.SkipNone:
			skipped++
		default:
			imported++
		}
	}
	return imported, skipped, nil
}
