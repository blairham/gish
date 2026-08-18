package pluginsdk_test

import (
	"slices"
	"sort"
	"testing"

	pluginapi "github.com/blairham/gish/pkg/pluginapi/v1"
	pluginsdk "github.com/blairham/gish/pkg/pluginsdk/v1"
)

// The SDK half of the #168 ABI freeze.
//
// internal/pluginhost/abi_test.go snapshots the wire format. This file
// snapshots the things a plugin binary depends on that are *not* in the
// protos — the handshake values it must present to be launched at all, and
// the service names both ends dispense by. Changing either one breaks every
// compiled plugin exactly as surely as renaming a field, and neither is
// covered by a proto descriptor.
//
// These tests exist because the promise in #188 is only worth making if it
// is mechanical: a binary built against v1 keeps working across gish
// releases without a rebuild.

func TestHandshakeIsFrozen(t *testing.T) {
	// Written out rather than compared against the package variable, so the
	// test fails when the value changes instead of moving with it.
	if got := pluginsdk.Handshake.MagicCookieKey; got != "KOI_PLUGIN" {
		t.Errorf("MagicCookieKey = %q, want KOI_PLUGIN", got)
	}
	if got := pluginsdk.Handshake.MagicCookieValue; got != "koi.plugin.v1" {
		t.Errorf("MagicCookieValue = %q, want koi.plugin.v1", got)
	}
	if got := pluginsdk.Handshake.ProtocolVersion; got != 1 {
		t.Errorf("ProtocolVersion = %d, want 1.\n"+
			"Bumping this refuses every plugin built against v1. It goes with a v2 proto "+
			"package and a new SDK path (pkg/pluginsdk/v2), not with an edit here.", got)
	}
}

func TestServiceNamesAreFrozen(t *testing.T) {
	want := []string{"ai", "command", "completion", "env", "history", "info", "prompt", "theme"}

	got := []string{
		pluginsdk.ServiceAI, pluginsdk.ServiceCommand, pluginsdk.ServiceCompletion,
		pluginsdk.ServiceEnv, pluginsdk.ServiceHistory, pluginsdk.ServiceInfo,
		pluginsdk.ServicePrompt, pluginsdk.ServiceTheme,
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("service names = %v, want %v.\n"+
			"Both ends of go-plugin key on these strings: a rename means an installed "+
			"plugin's service can no longer be dispensed.", got, want)
	}

	hostKeys := make([]string, 0, len(pluginsdk.HostMap()))
	for k := range pluginsdk.HostMap() {
		hostKeys = append(hostKeys, k)
	}
	sort.Strings(hostKeys)
	if !slices.Equal(hostKeys, want) {
		t.Errorf("HostMap keys = %v, want %v — the host must be able to dispense "+
			"every service a plugin may serve", hostKeys, want)
	}
}

// everything implements every service, so the SDK's coverage of the proto
// capability enum can be checked rather than assumed.
type everything struct {
	pluginapi.UnimplementedPluginInfoServer
	pluginapi.UnimplementedCompletionProviderServer
	pluginapi.UnimplementedPromptSegmentProviderServer
	pluginapi.UnimplementedHistoryBackendServer
	pluginapi.UnimplementedCommandProviderServer
	pluginapi.UnimplementedThemeProviderServer
	pluginapi.UnimplementedEnvProviderServer
	pluginapi.UnimplementedAIProviderServer
}

func fullPlugin() pluginsdk.Plugin {
	e := &everything{}
	return pluginsdk.Plugin{
		Info: e, Completion: e, Prompt: e, History: e,
		Command: e, Theme: e, Env: e, AI: e,
	}
}

// A capability the protos define but the SDK cannot serve is a capability no
// plugin author can implement — the gap is silent, because the host simply
// never dispatches to it. This is the test that makes adding one to
// common.proto surface here instead of in a plugin author's bug report.
func TestEveryServableCapabilityHasAField(t *testing.T) {
	served := fullPlugin()
	have := pluginsdk.Capabilities(served)

	values := pluginapi.Capability(0).Descriptor().Values()
	for i := range values.Len() {
		v := values.Get(i)
		c := pluginapi.Capability(v.Number())
		switch c {
		case pluginapi.Capability_CAPABILITY_UNSPECIFIED:
			continue
		case pluginapi.Capability_CAPABILITY_EVENTS:
			// ShellEvents (#83) is defined and allocated, but the host does
			// not serve it. When it lands, it gets a Plugin field and this
			// exemption goes away.
			continue
		}
		if !slices.Contains(have, c) {
			t.Errorf("%s has no pluginsdk.Plugin field — plugin authors cannot serve it", v.Name())
		}
	}
}

// The nil-Impl trap, closed by construction: registering a service with no
// implementation behind it leaves a door the host can open onto a nil
// pointer. Before #188, gish-git and gish-carapace both did exactly that.
func TestServeMapRegistersOnlyImplementedServices(t *testing.T) {
	e := &everything{}
	p := pluginsdk.Plugin{Info: e, Prompt: e}

	got := make([]string, 0, len(pluginsdk.ServeMap(p)))
	for k := range pluginsdk.ServeMap(p) {
		got = append(got, k)
	}
	sort.Strings(got)

	want := []string{pluginsdk.ServiceInfo, pluginsdk.ServicePrompt}
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("ServeMap registered %v, want %v — an unimplemented service must not be dispensable", got, want)
	}
}

// Describe and the dispenser map have to agree, or the host gates on a
// capability whose service was never registered (dead but announced), or
// dispatches to one that was never announced (registered but unreachable).
func TestCapabilitiesAgreeWithServeMap(t *testing.T) {
	e := &everything{}
	p := pluginsdk.Plugin{Info: e, Completion: e, Theme: e}

	registered := pluginsdk.ServeMap(p)
	byCapability := map[pluginapi.Capability]string{
		pluginapi.Capability_CAPABILITY_COMPLETION:     pluginsdk.ServiceCompletion,
		pluginapi.Capability_CAPABILITY_PROMPT_SEGMENT: pluginsdk.ServicePrompt,
		pluginapi.Capability_CAPABILITY_HISTORY:        pluginsdk.ServiceHistory,
		pluginapi.Capability_CAPABILITY_COMMAND:        pluginsdk.ServiceCommand,
		pluginapi.Capability_CAPABILITY_THEME:          pluginsdk.ServiceTheme,
		pluginapi.Capability_CAPABILITY_ENV:            pluginsdk.ServiceEnv,
		pluginapi.Capability_CAPABILITY_AI:             pluginsdk.ServiceAI,
	}

	for _, c := range pluginsdk.Capabilities(p) {
		svc, ok := byCapability[c]
		if !ok {
			t.Fatalf("Capabilities reported %v, which maps to no service", c)
		}
		if _, ok := registered[svc]; !ok {
			t.Errorf("%v is announced but %q is not registered", c, svc)
		}
	}

	// ...and nothing beyond info is registered without being announced.
	announced := len(pluginsdk.Capabilities(p))
	if got := len(registered) - 1; got != announced {
		t.Errorf("registered %d services beyond info, announced %d capabilities", got, announced)
	}
}
