package promptengine

import (
	"testing"
	"time"
)

// The ip and vpn_ip segments (#132, category 2). The interface table is
// substituted, because a test asserting against the machine's real
// interfaces would pass or fail with the Wi-Fi; realNetIfaces itself
// gets one smoke check that only asserts it does not panic.

// withNetIfaces installs a fixed interface table and resets the TTL
// cache around it, so tests neither see each other's tables nor a
// previous test's cached answer.
func withNetIfaces(t *testing.T, pairs []ifaceIP) {
	t.Helper()
	old := listNetIfaces
	listNetIfaces = func() []ifaceIP { return pairs }
	netIfaceMu.Lock()
	netIfacePairs, netIfaceAt = nil, time.Time{}
	netIfaceMu.Unlock()
	t.Cleanup(func() {
		listNetIfaces = old
		netIfaceMu.Lock()
		netIfacePairs, netIfaceAt = nil, time.Time{}
		netIfaceMu.Unlock()
	})
}

func TestIPSegmentShowsFirstMatch(t *testing.T) {
	withNetIfaces(t, []ifaceIP{
		{"lo0", "127.0.0.1"},
		{"en0", "192.168.1.20"},
		{"utun3", "10.8.0.2"},
	})
	cfg := Preset("lean")

	// The default matches everything, so enumeration order decides —
	// exactly upstream's $ips[1].
	got, ok := renderIP(cfg, sampleContext())
	if !ok || got.Content != "127.0.0.1" {
		t.Errorf("default IP_INTERFACE: got %q ok=%v, want 127.0.0.1", got.Content, ok)
	}

	// Narrowed, the regex is anchored around the whole name: `en.*`
	// must select en0, and `n0` must select nothing.
	cfg.Set("POWERLEVEL9K_IP_INTERFACE", "en.*")
	if got, ok = renderIP(cfg, sampleContext()); !ok || got.Content != "192.168.1.20" {
		t.Errorf("IP_INTERFACE=en.*: got %q ok=%v, want 192.168.1.20", got.Content, ok)
	}
	cfg.Set("POWERLEVEL9K_IP_INTERFACE", "n0")
	if _, ok = renderIP(cfg, sampleContext()); ok {
		t.Error("an unanchored fragment matched — the name regex must be anchored as upstream anchors it")
	}
}

func TestIPSegmentHidesWithNoMatch(t *testing.T) {
	withNetIfaces(t, nil)
	if _, ok := renderIP(Preset("lean"), sampleContext()); ok {
		t.Error("no interfaces, yet the segment rendered")
	}
}

// A regex that does not compile selects nothing: bad values degrade,
// never break the prompt.
func TestIPSegmentDegradesOnBadPattern(t *testing.T) {
	withNetIfaces(t, []ifaceIP{{"en0", "192.168.1.20"}})
	cfg := Preset("lean")
	cfg.Set("POWERLEVEL9K_IP_INTERFACE", "en[")
	if _, ok := renderIP(cfg, sampleContext()); ok {
		t.Error("an uncompilable pattern rendered a segment instead of degrading")
	}
}

func TestVPNIPSegment(t *testing.T) {
	withNetIfaces(t, []ifaceIP{
		{"en0", "192.168.1.20"},
		{"utun3", "10.8.0.2"},
		{"wg0", "10.9.0.7"},
	})
	cfg := Preset("lean")

	// The default interface set is upstream's: utun matches, en0 does
	// not, and without SHOW_ALL only the first match renders.
	got, ok := renderVPNIP(cfg, sampleContext())
	if !ok || got.Content != "10.8.0.2" {
		t.Errorf("default vpn_ip: got %q ok=%v, want 10.8.0.2", got.Content, ok)
	}

	cfg.Set("POWERLEVEL9K_VPN_IP_SHOW_ALL", "true")
	if got, ok = renderVPNIP(cfg, sampleContext()); !ok || got.Content != "10.8.0.2 10.9.0.7" {
		t.Errorf("SHOW_ALL: got %q ok=%v, want both addresses", got.Content, ok)
	}
}

func TestVPNIPHidesWithoutVPN(t *testing.T) {
	withNetIfaces(t, []ifaceIP{{"en0", "192.168.1.20"}})
	if _, ok := renderVPNIP(Preset("lean"), sampleContext()); ok {
		t.Error("no VPN interface up, yet vpn_ip rendered")
	}
}

// The cache is the point (#132): within the TTL the lister runs once,
// however many segments read it; after the TTL it runs again.
func TestNetIfaceCacheAmortises(t *testing.T) {
	calls := 0
	old := listNetIfaces
	listNetIfaces = func() []ifaceIP {
		calls++
		return []ifaceIP{{"en0", "192.168.1.20"}}
	}
	netIfaceMu.Lock()
	netIfacePairs, netIfaceAt = nil, time.Time{}
	netIfaceMu.Unlock()
	t.Cleanup(func() {
		listNetIfaces = old
		netIfaceMu.Lock()
		netIfacePairs, netIfaceAt = nil, time.Time{}
		netIfaceMu.Unlock()
	})

	cfg := Preset("lean")
	for range 5 {
		renderIP(cfg, sampleContext())
		renderVPNIP(cfg, sampleContext())
	}
	if calls != 1 {
		t.Errorf("lister ran %d times inside one TTL window, want 1", calls)
	}

	// Age the cache past the TTL: the next read must refresh.
	netIfaceMu.Lock()
	netIfaceAt = time.Now().Add(-netIfaceTTL - time.Second)
	netIfaceMu.Unlock()
	renderIP(cfg, sampleContext())
	if calls != 2 {
		t.Errorf("lister ran %d times after the TTL expired, want 2", calls)
	}
}

// realNetIfaces runs against whatever this machine has: the only
// portable claims are that it does not panic and that anything it
// returns is a well-formed IPv4 with a name.
func TestRealNetIfacesSmoke(t *testing.T) {
	t.Parallel()
	for _, p := range realNetIfaces() {
		if p.name == "" || p.ip == "" {
			t.Errorf("malformed pair: %+v", p)
		}
	}
}
