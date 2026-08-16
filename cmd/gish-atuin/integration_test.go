//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blairham/gish/pkg/pluginapi"
)

// The unit tests above cover parsing; this covers the part that only
// breaks when the bridge actually invokes something — the argv it
// builds, and the environment it passes the command in. A stub atuin
// records both, so the contract is checked without installing atuin and
// without touching any real history.

// stubAtuin installs a fake atuin on PATH and returns the path to its
// invocation log.
func stubAtuin(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "invocations.log")

	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + log + "\n" +
		"[ -n \"$ATUIN_COMMAND_LINE\" ] && printf 'ENV:%s\\n' \"$ATUIN_COMMAND_LINE\" >> " + log + "\n" +
		script + "\n"
	bin := filepath.Join(dir, "atuin")
	if err := os.WriteFile(bin, []byte(body), 0o700); err != nil { //nolint:gosec // stub in a temp dir
		t.Fatal(err)
	}
	// PATH is the only thing redirected: the stub never reads or writes
	// a real atuin database, and t.Setenv restores PATH afterwards.
	t.Setenv("PATH", dir)
	return log
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test temp path
	if err != nil {
		return ""
	}
	return string(data)
}

// A command full of quotes and newlines must reach atuin byte-for-byte.
// This is why the bridge uses --command-from-env: argv escaping across
// shells and platforms is exactly the thing that would corrupt a shell
// history, and atuin added that flag to avoid it.
func TestAppendPassesCommandThroughEnvironment(t *testing.T) {
	log := stubAtuin(t, `case "$1 $2" in "history start") echo fake-id-123 ;; esac`)

	nasty := "grep -r 'it'\\''s \"quoted\"' . |\n  awk '{print $1}'"
	b := backend{bridge: &bridge{}}
	resp, err := b.Append(t.Context(), &pluginapi.AppendRequest{
		Entry: &pluginapi.HistoryEntry{Command: nasty, ExitCode: 0, DurationMs: 2500},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !resp.GetStored() {
		t.Error("Append reported not stored")
	}

	out := readLog(t, log)
	if !strings.Contains(out, "ENV:"+nasty) {
		t.Errorf("command did not travel intact through ATUIN_COMMAND_LINE:\n%s", out)
	}
	if !strings.Contains(out, "--command-from-env") {
		t.Errorf("start did not use --command-from-env:\n%s", out)
	}
	// 2500ms must become 2500000000ns.
	if !strings.Contains(out, "--duration 2500000000") {
		t.Errorf("duration not in nanoseconds:\n%s", out)
	}
	if !strings.Contains(out, "history end fake-id-123") {
		t.Errorf("end did not use the id start returned:\n%s", out)
	}
}

func TestSearchReturnsEntriesFromAtuin(t *testing.T) {
	stubAtuin(t, `case "$1" in search)
	printf '0\t2026-08-16T10:30:00Z\t/srv/prod\tkubectl get pods\000'
	printf '1\t2026-08-16T09:00:00Z\t/srv/prod\tterraform apply\000' ;; esac`)

	b := backend{bridge: &bridge{}}
	stream := &captureStream{ctx: t.Context()}
	if err := b.Search(&pluginapi.SearchRequest{Query: "kube", Limit: 10}, stream); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(stream.batches) != 1 || !stream.batches[0].GetFinal() {
		t.Fatalf("want one final batch, got %+v", stream.batches)
	}
	entries := stream.batches[0].GetEntries()
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].GetCommand() != "kubectl get pods" || entries[0].GetCwd() != "/srv/prod" {
		t.Errorf("first entry = %+v", entries[0])
	}
	if entries[1].GetExitCode() != 1 {
		t.Errorf("exit code not carried: %+v", entries[1])
	}
}

// Search asks for the machine-spanning view explicitly: atuin's
// configured filter mode may scope results to this host or session,
// which would defeat the only reason to install the bridge.
func TestSearchAsksForGlobalScope(t *testing.T) {
	log := stubAtuin(t, `case "$1" in search) : ;; esac`)

	b := backend{bridge: &bridge{}}
	if err := b.Search(&pluginapi.SearchRequest{Query: "x", Limit: 5}, &captureStream{ctx: t.Context()}); err != nil {
		t.Fatal(err)
	}
	out := readLog(t, log)
	if !strings.Contains(out, "--filter-mode global") {
		t.Errorf("search did not request global scope:\n%s", out)
	}
	if !strings.Contains(out, "--print0") {
		t.Errorf("search did not request NUL-separated records:\n%s", out)
	}
}

// A query starting with '-' is an ordinary thing to search for and must
// not be read as a flag.
func TestSearchTerminatesOptionParsing(t *testing.T) {
	log := stubAtuin(t, `case "$1" in search) : ;; esac`)

	b := backend{bridge: &bridge{}}
	if err := b.Search(&pluginapi.SearchRequest{Query: "--version", Limit: 5}, &captureStream{ctx: t.Context()}); err != nil {
		t.Fatal(err)
	}
	if out := readLog(t, log); !strings.Contains(out, "-- --version") {
		t.Errorf("query was not separated from flags:\n%s", out)
	}
}

// A broken atuin yields an empty final batch, never an RPC error: the
// host merges what backends return and falls back to local history, so
// the user gets gish's own ctrl-r rather than a broken one.
func TestSearchDegradesWhenAtuinFails(t *testing.T) {
	stubAtuin(t, `exit 1`)

	b := backend{bridge: &bridge{}}
	stream := &captureStream{ctx: t.Context()}
	if err := b.Search(&pluginapi.SearchRequest{Query: "x", Limit: 5}, stream); err != nil {
		t.Fatalf("a failing atuin produced an RPC error: %v", err)
	}
	if len(stream.batches) != 1 || len(stream.batches[0].GetEntries()) != 0 {
		t.Errorf("want one empty final batch, got %+v", stream.batches)
	}
}

func TestAppendDeclinesWhenAtuinFails(t *testing.T) {
	stubAtuin(t, `exit 1`)

	b := backend{bridge: &bridge{}}
	resp, err := b.Append(t.Context(), &pluginapi.AppendRequest{
		Entry: &pluginapi.HistoryEntry{Command: "echo hi"},
	})
	if err != nil {
		t.Fatalf("a failing atuin produced an RPC error: %v", err)
	}
	if resp.GetStored() {
		t.Error("reported stored when atuin failed")
	}
}

func TestAppendIgnoresEmptyCommands(t *testing.T) {
	log := stubAtuin(t, `case "$1 $2" in "history start") echo id ;; esac`)

	b := backend{bridge: &bridge{}}
	if _, err := b.Append(t.Context(), &pluginapi.AppendRequest{
		Entry: &pluginapi.HistoryEntry{Command: ""},
	}); err != nil {
		t.Fatal(err)
	}
	if out := readLog(t, log); out != "" {
		t.Errorf("an empty command reached atuin:\n%s", out)
	}
}

// captureStream stands in for the gRPC server stream so Search can be
// driven directly, without a plugin host or a socket.
type captureStream struct {
	pluginapi.HistoryBackend_SearchServer
	ctx     context.Context
	batches []*pluginapi.SearchBatch
}

func (s *captureStream) Context() context.Context { return s.ctx }

func (s *captureStream) Send(b *pluginapi.SearchBatch) error {
	s.batches = append(s.batches, b)
	return nil
}
