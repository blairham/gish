package repl

// The MCP surface (#473): `config mcp on` makes the interactive session
// serve its own live state — aliases, functions, options, jobs, cwd,
// history — as read-only MCP tools on a per-session socket, and
// `koi mcp` is the stdio proxy an agent harness configures. Off by
// default: a listening socket is a surface the user should ask for,
// even a same-user-only one.
//
// The snapshot is taken at the prompt, like session recording (#103)
// and for the same reason: the prompt is both when the state is
// meaningful and when the runner is idle, which is what makes reading
// it race-free without the interpreter growing locks.

import (
	"fmt"
	"maps"
	"os"

	"github.com/blairham/koi-shell/internal/history"
	"github.com/blairham/koi-shell/internal/jobs"
	"github.com/blairham/koi-shell/internal/mcpserve"
	"github.com/blairham/koi-shell/internal/shell/interp"
)

type mcpManager struct {
	server *mcpserve.Server
	path   string
	jobs   func() []jobs.JobInfo
}

func newMCPManager(table *jobs.Table) *mcpManager {
	return &mcpManager{jobs: table.Snapshot}
}

// atPrompt starts or stops the listener as KOI_MCP changes — re-read
// each prompt so `config mcp on` takes effect without a restart, the
// KOI_BLOCKS pattern — and refreshes the snapshot clients see.
func (m *mcpManager) atPrompt(runner *interp.Runner) {
	on := shellVar(runner, "KOI_MCP", "off") == "on"
	switch {
	case on && m.server == nil:
		path := mcpserve.SocketPath()
		ln, err := mcpserve.Listen(path)
		if err != nil {
			// One warning, no retry loop: the variable is still on, so
			// without remembering the attempt here every prompt would
			// repeat this line.
			fmt.Fprintf(os.Stderr, "koi: mcp: %v\n", err)
			m.server = mcpserve.New(nil, Version) // mark tried; never listens
			return
		}
		m.server, m.path = mcpserve.New(m.historySearch(), Version), path
		go m.server.Serve(ln)
	case !on && m.server != nil:
		m.close()
		return
	case m.server == nil || m.path == "":
		return
	}
	m.server.Update(mcpSnapshot(runner, m.jobs()))
}

// historySearch adapts the session store; nil when the session has no
// history, which the server reports honestly per tool call.
func (m *mcpManager) historySearch() func(string, int) []history.Entry {
	if historyStore == nil {
		return nil
	}
	return historyStore.SearchEntries
}

func (m *mcpManager) close() {
	if m.server == nil {
		return
	}
	m.server.Close()
	if m.path != "" {
		_ = os.Remove(m.path)
	}
	m.server, m.path = nil, ""
}

// mcpSnapshot clones what the tools answer from. Function bodies stay
// as their parse trees — immutable once defined, rendered only when a
// client asks — so the per-prompt cost is map clones, not printing.
func mcpSnapshot(runner *interp.Runner, jobList []jobs.JobInfo) *mcpserve.Snapshot {
	return &mcpserve.Snapshot{
		Cwd:       runner.Dir,
		DirStack:  runner.DirStack(),
		Aliases:   runner.Aliases(),
		Functions: maps.Clone(runner.Funcs),
		Options:   runner.Options(),
		Jobs:      jobList,
	}
}
