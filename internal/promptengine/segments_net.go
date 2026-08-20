package promptengine

import (
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ip and vpn_ip (#132, category 2): inside this package's one rule —
// enumerating interfaces forks nothing, dials nothing, blocks on
// nothing — and still not done inline, because the rule admits it and
// the budget does not. One enumeration costs ~51µs on darwin
// (Interfaces() plus Addrs() per interface is N+1 routing-table
// sysctls), more than the entire lean prompt it would join; upstream
// itself computes this table in an async worker rather than per prompt.
// A short TTL makes the amortized cost a map lookup: addresses change
// when a VPN comes up or the network switches — on the order of
// minutes — and a few seconds of staleness on a segment showing your
// own address is invisible.
//
// public_ip and nordvpn stay out: those genuinely dial, and whether a
// prompt should be doing background network I/O at all is a policy
// decision #132 keeps open on purpose.

const netIfaceTTL = 5 * time.Second

type ifaceIP struct{ name, ip string }

var (
	netIfaceMu    sync.Mutex
	netIfaceAt    time.Time
	netIfacePairs []ifaceIP
	// listNetIfaces is the seam: tests substitute a fixed table, since a
	// test asserting against the machine's real interfaces would pass or
	// fail with the Wi-Fi.
	listNetIfaces = realNetIfaces
)

func cachedNetIfaces() []ifaceIP {
	netIfaceMu.Lock()
	defer netIfaceMu.Unlock()
	if netIfacePairs != nil && time.Since(netIfaceAt) < netIfaceTTL {
		return netIfacePairs
	}
	pairs := listNetIfaces()
	if pairs == nil {
		// Remember an empty answer too; a machine with no addresses
		// should not pay the enumeration on every prompt for nothing.
		pairs = []ifaceIP{}
	}
	netIfacePairs, netIfaceAt = pairs, time.Now()
	return netIfacePairs
}

// realNetIfaces mirrors the table upstream builds from `ip -4 a` or
// ifconfig: interfaces that are up, each contributing its first IPv4
// address, in enumeration order. IPv4 only — upstream's parsers match
// `inet [0-9.]+` and nothing else.
func realNetIfaces() []ifaceIP {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []ifaceIP
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if v4 := ipnet.IP.To4(); v4 != nil {
				out = append(out, ifaceIP{name: ifc.Name, ip: v4.String()})
				break
			}
		}
	}
	return out
}

// matchIfaces is upstream's selection: the regex is anchored around the
// whole interface *name* (`^(re)$`), and matches keep enumeration
// order. A pattern that does not compile selects nothing — bad values
// degrade, never break the prompt.
func matchIfaces(pattern string, pairs []ifaceIP) []ifaceIP {
	if pattern == "" {
		return nil
	}
	re, err := regexp.Compile("^(" + pattern + ")$")
	if err != nil {
		return nil
	}
	var out []ifaceIP
	for _, p := range pairs {
		if re.MatchString(p.name) {
			out = append(out, p)
		}
	}
	return out
}

func init() {
	register("ip", renderIP)
	register("vpn_ip", renderVPNIP)
}

func renderIP(cfg *Config, _ *Context) (Rendered, bool) {
	// Upstream's effective default is `.*` — the first up interface with
	// an IPv4 address wins unless IP_INTERFACE narrows it.
	matched := matchIfaces(cfg.Str("IP_INTERFACE", ".*"), cachedNetIfaces())
	if len(matched) == 0 {
		return Rendered{}, false
	}
	return Rendered{
		Content: matched[0].ip,
		Icon:    decodeEscapes(cfg.Icon("ip", "", "")),
	}, true
}

// defaultVPNInterfaces is upstream's default as of 5.3-era master:
// wireguard, gpd, anything-tun, tailscale, zerotier.
const defaultVPNInterfaces = `(gpd|wg|(.*tun)|tailscale)[0-9]*|(zt.*)`

func renderVPNIP(cfg *Config, _ *Context) (Rendered, bool) {
	matched := matchIfaces(cfg.Str("VPN_IP_INTERFACE", defaultVPNInterfaces), cachedNetIfaces())
	if len(matched) == 0 {
		return Rendered{}, false
	}
	ips := []string{matched[0].ip}
	if cfg.Bool("VPN_IP_SHOW_ALL", false) {
		ips = ips[:0]
		for _, m := range matched {
			ips = append(ips, m.ip)
		}
	}
	// Upstream renders one segment per address under SHOW_ALL; this
	// engine renders one segment, so the addresses are joined — the same
	// ribbon, one fewer separator.
	return Rendered{
		Content: strings.Join(ips, " "),
		Icon:    decodeEscapes(cfg.Icon("vpn_ip", "", "")),
	}, true
}
