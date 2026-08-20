package repl

// KOI_TRACE_JSON is the structured counterpart to `set -x` (#474): when it
// names a file, every simple command the session executes appends one JSON
// object — position, unexpanded text, expanded argv, exit status, timing —
// leaving the command's own stdout and stderr untouched. It is a koi
// surface, not a bash one, which is why it is an environment variable and
// not a `set -o` name: the `set -o` listing is compared against bash's and
// a new row there would be a visible divergence in every compatible script.
//
// The variable is read once, at session start. A script exporting it
// mid-run changes its children (which read it at their own start), never
// the running session — the same rule an interactive toggle would blur.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

const traceJSONVar = "KOI_TRACE_JSON"

// jsonTraceOptions returns the RunnerOption installing the JSON trace
// hook when KOI_TRACE_JSON names a writable file, and nothing otherwise.
// An unopenable path costs one warning and no tracing — a debugging aid
// must never be why the shell refuses to start.
func jsonTraceOptions() []interp.RunnerOption {
	path := os.Getenv(traceJSONVar)
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "koi: %s: %v\n", traceJSONVar, err)
		return nil
	}
	sink := &jsonTraceSink{w: f}
	return []interp.RunnerOption{interp.TraceHook(sink.record)}
}

// jsonTraceSink serializes trace events to one writer. The mutex is
// load-bearing: pipeline stages trace concurrently, and half a JSON line
// interleaved with another is worse than no trace at all.
type jsonTraceSink struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *jsonTraceSink) record(ev interp.TraceEvent) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	// A full disk must not fail the command being traced.
	_, _ = s.w.Write(b)
}
