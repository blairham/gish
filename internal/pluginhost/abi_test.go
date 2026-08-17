package pluginhost_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/blairham/gish/internal/pluginhost"
	pluginapi "github.com/blairham/gish/pkg/pluginapi/v1"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// The version-independent ABI, enforced (#168).
//
// This is the documented reason nushell's plugin ecosystem never formed,
// and it is not the wire format:
//
//	"the plugin interface requires strict version matching… which sort
//	 of kills the idea that I'm going to distribute these"
//
// Their top plugin has 85 stars, with out-of-process plugins in any
// language — the same architecture as ours. What failed was
// distribution, and the fix is a promise a plugin author can rely on: a
// binary built against plugin/v1 keeps working across gish releases
// without a rebuild.
//
// "Frozen-additive" was a habit written in AGENTS.md. This makes it a
// mechanism: the shape of every v1 message is snapshotted, and a change
// that would break a compiled plugin fails here rather than in
// somebody's shell six months from now.
//
// Additions pass. That is the whole point of the word "additive".

const abiSnapshot = "testdata/plugin_v1_abi.txt"

func TestPluginV1ABIIsFrozenAdditive(t *testing.T) {
	current := describeV1()

	want, err := os.ReadFile(abiSnapshot)
	if os.IsNotExist(err) {
		writeSnapshot(t, current)
		t.Fatalf("wrote the initial ABI snapshot to %s — commit it", abiSnapshot)
	}
	if err != nil {
		t.Fatal(err)
	}

	recorded := parseSnapshot(string(want))
	var broken []string
	for name, field := range recorded {
		now, ok := current[name]
		if !ok {
			broken = append(broken, fmt.Sprintf("%s was removed", name))
			continue
		}
		if now != field {
			broken = append(broken, fmt.Sprintf("%s changed\n    was: %s\n    now: %s", name, field, now))
		}
	}
	if len(broken) > 0 {
		sort.Strings(broken)
		t.Fatalf("plugin/v1 is frozen-additive and this breaks a compiled plugin:\n  %s\n\n"+
			"Renames, type changes, field-number changes and removals need a v2 package and a "+
			"Handshake.ProtocolVersion bump — a plugin built last year has to keep working.",
			strings.Join(broken, "\n  "))
	}

	// Additions are fine, and worth noticing: the snapshot should be
	// refreshed so the next change is measured against the truth.
	if len(current) > len(recorded) {
		if os.Getenv("UPDATE_ABI_SNAPSHOT") != "" {
			writeSnapshot(t, current)
			return
		}
		var added []string
		for name := range current {
			if _, ok := recorded[name]; !ok {
				added = append(added, name)
			}
		}
		sort.Strings(added)
		t.Logf("plugin/v1 grew %d field(s) — additive, which is allowed:\n  %s\n"+
			"Refresh with UPDATE_ABI_SNAPSHOT=1 go test ./internal/pluginhost/",
			len(added), strings.Join(added, "\n  "))
	}
}

// describeV1 walks every registered message in the v1 package and
// records what a compiled plugin depends on: a field's number, its wire
// type, and whether it repeats. Names are included because a rename is
// a source break for every plugin author even when the wire is
// unchanged.
func describeV1() map[string]string {
	out := map[string]string{}
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if !strings.HasPrefix(string(fd.Package()), "gish.plugin.v1") {
			return true
		}
		messages := fd.Messages()
		for i := range messages.Len() {
			describeMessage(out, messages.Get(i))
		}
		services := fd.Services()
		for i := range services.Len() {
			svc := services.Get(i)
			methods := svc.Methods()
			for j := range methods.Len() {
				m := methods.Get(j)
				out[fmt.Sprintf("%s/%s", svc.FullName(), m.Name())] = fmt.Sprintf(
					"rpc(%s,%s,client_stream=%v,server_stream=%v)",
					m.Input().FullName(), m.Output().FullName(), m.IsStreamingClient(), m.IsStreamingServer())
			}
		}
		return true
	})
	return out
}

func describeMessage(out map[string]string, md protoreflect.MessageDescriptor) {
	fields := md.Fields()
	for i := range fields.Len() {
		f := fields.Get(i)
		out[fmt.Sprintf("%s.%s", md.FullName(), f.Name())] = fmt.Sprintf(
			"%d:%s%s", f.Number(), f.Kind(), cardinality(f))
	}
	nested := md.Messages()
	for i := range nested.Len() {
		if nested.Get(i).IsMapEntry() {
			continue
		}
		describeMessage(out, nested.Get(i))
	}
	enums := md.Enums()
	for i := range enums.Len() {
		describeEnum(out, enums.Get(i))
	}
}

func describeEnum(out map[string]string, ed protoreflect.EnumDescriptor) {
	values := ed.Values()
	for i := range values.Len() {
		v := values.Get(i)
		out[fmt.Sprintf("%s.%s", ed.FullName(), v.Name())] = fmt.Sprintf("enum:%d", v.Number())
	}
}

func cardinality(f protoreflect.FieldDescriptor) string {
	switch {
	case f.IsMap():
		return "[map]"
	case f.IsList():
		return "[repeated]"
	default:
		return ""
	}
}

func writeSnapshot(t *testing.T, described map[string]string) {
	t.Helper()
	names := make([]string, 0, len(described))
	for name := range described {
		names = append(names, name)
	}
	slices.Sort(names)

	var b strings.Builder
	b.WriteString("# gish.plugin.v1 ABI snapshot — generated by internal/pluginhost/abi_test.go.\n")
	b.WriteString("# Frozen-additive: entries may be added, never changed or removed.\n")
	b.WriteString("# Refresh after an addition with: UPDATE_ABI_SNAPSHOT=1 go test ./internal/pluginhost/\n")
	for _, name := range names {
		fmt.Fprintf(&b, "%s %s\n", name, described[name])
	}
	if err := os.MkdirAll(filepath.Dir(abiSnapshot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abiSnapshot, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func parseSnapshot(data string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, shape, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		out[name] = shape
	}
	return out
}

// The other half of the promise (#168): **a plugin can never block a
// keystroke.**
//
// nushell's plugin protocol has no timeout enforcement at all, so a slow
// plugin blocks the shell. Ours cannot, by construction — every outbound
// call carries a deadline — and nobody else advertises this, because
// nobody else can: an add-on that hooks zsh's ZLE runs *in* the shell's
// process and has no such boundary to enforce.
//
// A guarantee that is not tested is a claim, so this hangs a plugin
// deliberately and measures what it costs.
func TestAHangingPluginCannotBlockTheShell(t *testing.T) {
	h := newHost(t)
	provs := h.PromptProviders(context.Background())
	if len(provs) != 1 {
		t.Fatalf("providers = %d", len(provs))
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), pluginhost.DefaultRenderBudget)
	defer cancel()
	_, err := provs[0].Client.Render(ctx, &pluginapi.RenderRequest{SegmentId: "hang", EventSeq: h.NextSeq()})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a hanging plugin answered, which means the fixture is not hanging")
	}
	// The budget, plus room for a loaded CI runner to schedule the
	// goroutine that gives up. What is being asserted is the *order of
	// magnitude*: bounded, not "eventually".
	if limit := pluginhost.DefaultRenderBudget * 4; elapsed > limit {
		t.Errorf("a hanging plugin held the caller for %v, past the %v budget (limit %v)",
			elapsed, pluginhost.DefaultRenderBudget, limit)
	}

	// And the shell is still usable afterwards: the next call to the
	// same plugin gets an answer.
	okCtx, okCancel := context.WithTimeout(context.Background(), pluginhost.DefaultRenderBudget)
	defer okCancel()
	resp, err := provs[0].Client.Render(okCtx, &pluginapi.RenderRequest{SegmentId: "test", EventSeq: h.NextSeq()})
	if err != nil || resp.GetText() != "fixture-segment" {
		t.Errorf("after the hang, a normal render gave %v / %v", resp, err)
	}
}
