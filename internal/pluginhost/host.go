// Package pluginhost manages tier-2 native plugins: resident subprocesses
// speaking gRPC via hashicorp/go-plugin (the same architecture as the
// cloudctl/understudy/chaos-lab tools).
//
// Lifecycle: plugins are launched lazily on first use, kept resident for
// the session, and every host→plugin call carries a deadline so a slow
// plugin degrades (stale prompt segment, missing completions) instead of
// blocking a keystroke.
//
// This is the host half only. The half a plugin author needs — the handshake,
// the dispensers, Serve — is published as pkg/pluginsdk (#188); discovery,
// crash healing, and the command index stay in here, unpublished, so the
// lifecycle remains free to change while the contract does not.
package pluginhost

import pluginsdk "github.com/blairham/gish/pkg/pluginsdk/v1"

// Handshake gates plugin startup. It lives in pkg/pluginsdk, where plugin
// authors can reach it; this is the host's name for the same value.
var Handshake = pluginsdk.Handshake

// PluginMap names every capability service a plugin process may serve. The
// host needs all of them because it learns from Describe which to dispense —
// a plugin registers only what it implements.
var PluginMap = pluginsdk.HostMap()
