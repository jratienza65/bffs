package transfer

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// LinkAddr is one address of a local interface together with the on-link
// prefix it belongs to. IPv6 link-local addresses carry their zone (the
// interface name) so they can be bound and compared.
type LinkAddr struct {
	Addr   netip.Addr
	Prefix netip.Prefix
	Iface  string
}

// LANOptions tunes what counts as "the local network".
type LANOptions struct {
	// AllowLoopback admits the loopback interface and loopback peers
	// (tests, same-machine trials).
	AllowLoopback bool
	// AllowRouted skips the on-link test only: a peer must still be a
	// private or link-local address and is never one of the denylisted
	// overlay ranges.
	AllowRouted bool
	// Iface restricts LANAddrs to one interface by name.
	Iface string
}

// denylist holds ranges that are never "the local network" even when they
// are reachable and private-looking: CGNAT/Tailscale IPv4 and the Tailscale
// IPv6 ULA block.
var denylist = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// iface is the slice of net.Interface LANAddrs needs, so the filter can be
// unit-tested on synthetic data without touching real interfaces.
type iface struct {
	Name  string
	Flags net.Flags
	Addrs []net.Addr
}

// LANAddrs enumerates the addresses A may bind and B may dial from: up,
// non-loopback (unless AllowLoopback), non-point-to-point (utun*/VPN)
// interfaces, skipping awdl*/llw*, keeping private and link-local addresses
// with their prefix. IPv4 entries come first, then IPv6.
func LANAddrs(o LANOptions) ([]LinkAddr, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("listing network interfaces: %w", err)
	}
	var in []iface
	for _, ifi := range ifs {
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		in = append(in, iface{Name: ifi.Name, Flags: ifi.Flags, Addrs: addrs})
	}
	return lanAddrsFrom(in, o), nil
}

func lanAddrsFrom(ifs []iface, o LANOptions) []LinkAddr {
	var v4, v6 []LinkAddr
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 {
			continue
		}
		loop := ifi.Flags&net.FlagLoopback != 0
		if loop && !o.AllowLoopback {
			continue
		}
		if ifi.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		if strings.HasPrefix(ifi.Name, "awdl") || strings.HasPrefix(ifi.Name, "llw") {
			continue
		}
		if o.Iface != "" && ifi.Name != o.Iface {
			continue
		}
		for _, a := range ifi.Addrs {
			la, ok := linkAddr(a, ifi.Name, o.AllowLoopback)
			if !ok {
				continue
			}
			if la.Addr.Is4() {
				v4 = append(v4, la)
			} else {
				v6 = append(v6, la)
			}
		}
	}
	return append(v4, v6...)
}

// linkAddr converts one interface address to a LinkAddr, or reports false
// when it is not a LAN address.
func linkAddr(a net.Addr, ifname string, allowLoopback bool) (LinkAddr, bool) {
	var ip netip.Addr
	var bits int
	switch v := a.(type) {
	case *net.IPNet:
		var ok bool
		ip, ok = netip.AddrFromSlice(v.IP)
		if !ok {
			return LinkAddr{}, false
		}
		ip = ip.Unmap()
		bits, _ = v.Mask.Size()
		if len(v.Mask) == 16 && ip.Is4() {
			bits -= 96
		}
	case *net.IPAddr:
		var ok bool
		ip, ok = netip.AddrFromSlice(v.IP)
		if !ok {
			return LinkAddr{}, false
		}
		ip = ip.Unmap()
		bits = ip.BitLen()
	default:
		p, err := netip.ParsePrefix(a.String())
		if err != nil {
			return LinkAddr{}, false
		}
		ip = p.Addr().Unmap()
		bits = p.Bits()
		if p.Addr().Is4In6() {
			bits -= 96
		}
	}
	if bits < 0 || bits > ip.BitLen() {
		return LinkAddr{}, false
	}
	switch {
	case ip.IsLoopback():
		if !allowLoopback {
			return LinkAddr{}, false
		}
	case ip.IsPrivate(), ip.IsLinkLocalUnicast():
	default:
		return LinkAddr{}, false
	}
	prefix := netip.PrefixFrom(ip.WithZone(""), bits).Masked()
	if ip.Is6() && ip.IsLinkLocalUnicast() {
		ip = ip.WithZone(ifname)
	}
	return LinkAddr{Addr: ip, Prefix: prefix, Iface: ifname}, true
}

// IsLAN decides whether peer may take part in a transfer. IPv4-mapped forms
// are unmapped first; multicast and unspecified addresses are refused; the
// denylist is refused even with AllowRouted; loopback needs AllowLoopback;
// everything else must be private or link-local and, unless AllowRouted, lie
// inside one of local's prefixes (link-local: with the same zone).
func IsLAN(peer netip.Addr, local []LinkAddr, o LANOptions) bool {
	if !peer.IsValid() {
		return false
	}
	peer = peer.Unmap()
	if peer.IsMulticast() || peer.IsUnspecified() || peer.IsInterfaceLocalMulticast() {
		return false
	}
	bare := peer.WithZone("")
	for _, d := range denylist {
		if d.Contains(bare) {
			return false
		}
	}
	if peer.IsLoopback() {
		return o.AllowLoopback
	}
	if !peer.IsPrivate() && !peer.IsLinkLocalUnicast() {
		return false
	}
	if o.AllowRouted {
		return true
	}
	linkLocal6 := peer.Is6() && peer.IsLinkLocalUnicast()
	for _, l := range local {
		if !l.Prefix.IsValid() {
			continue
		}
		if linkLocal6 && peer.Zone() != l.Addr.Zone() {
			continue
		}
		if l.Prefix.Contains(bare) {
			return true
		}
	}
	return false
}
