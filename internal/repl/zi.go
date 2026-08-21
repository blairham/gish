package repl

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/plugmgr"
)

// ziCallHandler intercepts `zi` commands before execution and replaces
// the argv — upstream zi-go's shim protocol eliminated by construction:
// install work happens here in the handler, and a load becomes
// `source <payload>`, which the interpreter runs in the live shell so
// plugin functions, variables, and PATH changes persist.
//
// The manager is created lazily so shells that never use zi never grow
// a ~/.zi-go directory. mgr is the plugmgr.Manager contract — the swap
// point for a replacement manager (#23, #11).
func ziCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	var (
		once   sync.Once
		mgr    plugmgr.Manager
		mgrErr error
	)
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "zi" {
			return next(ctx, args)
		}
		hc := interp.HandlerCtx(ctx)
		once.Do(func() {
			var z *plugmgr.Zi
			if z, mgrErr = plugmgr.NewZi(hc.Stderr); mgrErr == nil {
				mgr = z
			}
		})
		if mgrErr != nil {
			hc.Errf("zi: %v\n", mgrErr)
			return []string{"false"}, nil
		}
		return runZi(mgr, hc, args[1:]), nil
	}
}

// passthroughCall is the CallHandler identity, used when no other
// rewriter is stacked underneath.
func passthroughCall(_ context.Context, args []string) ([]string, error) {
	return args, nil
}

const ziUsage = `usage: zi <command> [args]

  ice <ices...>        buffer ice modifiers for the next load/snippet
  load|light <spec>    install if needed and load a plugin (user/repo or URL)
  snippet <spec>       install if needed and load a snippet (URL or OMZ:: alias)
  update [target]      update one object, or everything
  delete <target>      remove an installed object
  list|status          show installed objects

Ices use Zi spelling: zi ice wait"1" lucid from"gh-r" pick"bin/fzf"
(wait is accepted but loads run immediately until idle hooks land)`

//nolint:gocognit // one flat subcommand dispatch reads better unsplit
func runZi(mgr plugmgr.Manager, hc interp.HandlerContext, args []string) []string {
	fail := func(err error) []string {
		hc.Errf("zi: %v\n", err)
		return []string{"false"}
	}
	if len(args) == 0 {
		printZiHelp(hc.Stdout)
		return []string{"true"}
	}
	if args[0] != "migrate" {
		ziDeprecationNotice(hc)
	}
	switch args[0] {
	case "migrate":
		return ziMigrate(mgr, hc)
	case "ice":
		if err := mgr.SetIces(args[1:]); err != nil {
			return fail(err)
		}
		return []string{"true"}
	case "load", "light":
		if len(args) != 2 {
			return fail(errors.New("usage: zi load <user/repo | url | name>"))
		}
		payload, err := mgr.Load(args[1])
		if err != nil {
			return fail(err)
		}
		return []string{"source", payload}
	case "snippet":
		if len(args) != 2 {
			return fail(errors.New("usage: zi snippet <url | OMZ:: alias>"))
		}
		payload, err := mgr.Snippet(args[1])
		if err != nil {
			return fail(err)
		}
		return []string{"source", payload}
	case "update":
		target := ""
		if len(args) > 1 {
			target = args[1]
		}
		if err := ziUpdate(mgr, target, hc); err != nil {
			return fail(err)
		}
		return []string{"true"}
	case "delete":
		if len(args) != 2 {
			return fail(errors.New("usage: zi delete <target>"))
		}
		if err := mgr.Delete(args[1]); err != nil {
			return fail(err)
		}
		return []string{"true"}
	case "list", "status":
		if err := ziList(mgr, hc); err != nil {
			return fail(err)
		}
		return []string{"true"}
	case "help", "-h", "--help":
		printZiHelp(hc.Stdout)
		return []string{"true"}
	default:
		return fail(fmt.Errorf("unknown command %q\n%s", args[0], ziUsage))
	}
}
