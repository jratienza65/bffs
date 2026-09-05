package transfer

import (
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
)

func testLocal() []LinkAddr {
	return []LinkAddr{
		{Addr: netip.MustParseAddr("10.0.0.5"), Prefix: netip.MustParsePrefix("10.0.0.0/24"), Iface: "en0"},
		{Addr: netip.MustParseAddr("192.168.1.70"), Prefix: netip.MustParsePrefix("192.168.1.64/26"), Iface: "en1"},
		{Addr: netip.MustParseAddr("fd00::10"), Prefix: netip.MustParsePrefix("fd00::/64"), Iface: "en0"},
		{Addr: netip.MustParseAddr("fe80::abcd%en0"), Prefix: netip.MustParsePrefix("fe80::/64"), Iface: "en0"},
	}
}

func TestIsLAN(t *testing.T) {
	local := testLocal()
	cases := []struct {
		peer string
		o    LANOptions
		want bool
	}{
		{"10.0.0.1", LANOptions{}, true},
		{"192.168.1.77", LANOptions{}, true},
		{"192.168.1.2", LANOptions{}, false}, // private but on no local prefix
		{"10.8.0.5", LANOptions{}, false},    // private, routed
		{"172.16.0.1", LANOptions{}, false},  // private, not on-link
		{"172.32.0.1", LANOptions{}, false},  // public
		{"100.64.1.1", LANOptions{}, false},  // CGNAT / Tailscale
		{"100.64.1.1", LANOptions{AllowRouted: true}, false},
		{"fd7a:115c:a1e0::1", LANOptions{}, false}, // Tailscale ULA
		{"fd7a:115c:a1e0::1", LANOptions{AllowRouted: true}, false},
		{"fd00::1", LANOptions{}, true},
		{"fd00:1::1", LANOptions{}, false},
		{"fe80::1%en0", LANOptions{}, true},
		{"fe80::1%en1", LANOptions{}, false},
		{"fe80::1", LANOptions{}, false}, // no zone: cannot be matched to an interface
		{"2001:db8::1", LANOptions{}, false},
		{"::ffff:192.168.1.77", LANOptions{}, true}, // IPv4-mapped is unmapped first
		{"::ffff:203.0.113.5", LANOptions{}, false},
		{"127.0.0.1", LANOptions{}, false},
		{"127.0.0.1", LANOptions{AllowLoopback: true}, true},
		{"::1", LANOptions{}, false},
		{"::1", LANOptions{AllowLoopback: true}, true},
		{"10.8.0.5", LANOptions{AllowRouted: true}, true},
		{"172.16.0.1", LANOptions{AllowRouted: true}, true},
		{"172.32.0.1", LANOptions{AllowRouted: true}, false},
		{"2001:db8::1", LANOptions{AllowRouted: true}, false},
		{"224.0.0.1", LANOptions{AllowRouted: true, AllowLoopback: true}, false},
		{"ff02::1%en0", LANOptions{AllowRouted: true, AllowLoopback: true}, false},
		{"0.0.0.0", LANOptions{AllowRouted: true, AllowLoopback: true}, false},
		{"::", LANOptions{AllowRouted: true, AllowLoopback: true}, false},
		{"169.254.1.1", LANOptions{}, false},                 // IPv4 link-local, no local prefix
		{"169.254.1.1", LANOptions{AllowRouted: true}, true}, // link-local is admitted when routed
	}
	for _, tc := range cases {
		t.Run(tc.peer, func(t *testing.T) {
			peer := netip.MustParseAddr(tc.peer)
			if got := IsLAN(peer, local, tc.o); got != tc.want {
				t.Fatalf("IsLAN(%s, %+v) = %v, want %v", tc.peer, tc.o, got, tc.want)
			}
		})
	}
	if IsLAN(netip.Addr{}, local, LANOptions{AllowLoopback: true, AllowRouted: true}) {
		t.Fatal("zero Addr must be refused")
	}
	if IsLAN(netip.MustParseAddr("10.0.0.1"), nil, LANOptions{}) {
		t.Fatal("no local prefixes: nothing is on-link")
	}
}

func ipnet(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	ip, _, _ := net.ParseCIDR(s)
	n.IP = ip
	return n
}

func TestLANAddrsFromFilters(t *testing.T) {
	ifs := []iface{
		{Name: "lo0", Flags: net.FlagUp | net.FlagLoopback, Addrs: []net.Addr{ipnet("127.0.0.1/8"), ipnet("::1/128")}},
		{Name: "en0", Flags: net.FlagUp | net.FlagBroadcast | net.FlagMulticast, Addrs: []net.Addr{
			ipnet("fe80::1/64"),
			ipnet("192.168.1.10/24"),
			ipnet("2001:db8::10/64"),
			ipnet("fd12:3456::10/64"),
		}},
		{Name: "en5", Flags: net.FlagBroadcast, Addrs: []net.Addr{ipnet("10.9.9.9/24")}}, // down
		{Name: "utun3", Flags: net.FlagUp | net.FlagPointToPoint, Addrs: []net.Addr{ipnet("100.100.1.1/32"), ipnet("10.8.0.2/24")}},
		{Name: "awdl0", Flags: net.FlagUp | net.FlagBroadcast, Addrs: []net.Addr{ipnet("fe80::2/64")}},
		{Name: "llw0", Flags: net.FlagUp | net.FlagBroadcast, Addrs: []net.Addr{ipnet("fe80::3/64")}},
		{Name: "en1", Flags: net.FlagUp | net.FlagBroadcast, Addrs: []net.Addr{ipnet("10.0.0.5/24"), &net.IPAddr{IP: net.ParseIP("172.16.5.5")}}},
	}
	got := lanAddrsFrom(ifs, LANOptions{})
	want := []LinkAddr{
		{Addr: netip.MustParseAddr("192.168.1.10"), Prefix: netip.MustParsePrefix("192.168.1.0/24"), Iface: "en0"},
		{Addr: netip.MustParseAddr("10.0.0.5"), Prefix: netip.MustParsePrefix("10.0.0.0/24"), Iface: "en1"},
		{Addr: netip.MustParseAddr("172.16.5.5"), Prefix: netip.MustParsePrefix("172.16.5.5/32"), Iface: "en1"},
		{Addr: netip.MustParseAddr("fe80::1%en0"), Prefix: netip.MustParsePrefix("fe80::/64"), Iface: "en0"},
		{Addr: netip.MustParseAddr("fd12:3456::10"), Prefix: netip.MustParsePrefix("fd12:3456::/64"), Iface: "en0"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %+v, want %+v", i, got[i], want[i])
		}
	}

	loop := lanAddrsFrom(ifs, LANOptions{AllowLoopback: true, Iface: "lo0"})
	if len(loop) != 2 || loop[0].Addr != netip.MustParseAddr("127.0.0.1") || loop[0].Prefix != netip.MustParsePrefix("127.0.0.0/8") || loop[1].Addr != netip.MustParseAddr("::1") {
		t.Fatalf("loopback entries: %+v", loop)
	}
	if only := lanAddrsFrom(ifs, LANOptions{Iface: "en1"}); len(only) != 2 || only[0].Iface != "en1" {
		t.Fatalf("Iface restriction: %+v", only)
	}
	if none := lanAddrsFrom(ifs, LANOptions{Iface: "nope"}); len(none) != 0 {
		t.Fatalf("unknown Iface: %+v", none)
	}

	// The zone is kept for binding and matching, and the on-link check
	// works against the produced prefixes.
	if !IsLAN(netip.MustParseAddr("fe80::9%en0"), got, LANOptions{}) {
		t.Fatal("fe80::9 on en0 should be on-link for en0")
	}
	if IsLAN(netip.MustParseAddr("fe80::9%en1"), got, LANOptions{}) {
		t.Fatal("fe80::9 on en1 must not match en0's link-local")
	}
	if !IsLAN(netip.MustParseAddr("192.168.1.77"), got, LANOptions{}) {
		t.Fatal("192.168.1.77 should be on-link")
	}
}

func TestLANAddrsLiveProbe(t *testing.T) {
	if os.Getenv("BFFS_LIVE_PROBE") != "1" {
		t.Skip("set BFFS_LIVE_PROBE=1 to probe this machine's interfaces")
	}
	got, err := LANAddrs(LANOptions{})
	if err != nil {
		t.Fatal(err)
	}
	private := 0
	for _, la := range got {
		if strings.HasPrefix(la.Iface, "utun") {
			t.Errorf("point-to-point interface %s leaked: %+v", la.Iface, la)
		}
		if la.Addr.Is4() && la.Addr.IsPrivate() {
			private++
		}
		if !la.Prefix.IsValid() {
			t.Errorf("entry without a prefix: %+v", la)
		}
		if la.Addr.Is6() && la.Addr.IsLinkLocalUnicast() && la.Addr.Zone() == "" {
			t.Errorf("link-local without zone: %+v", la)
		}
		t.Logf("%s %s (%s)", la.Iface, la.Addr, la.Prefix)
	}
	if private == 0 {
		t.Fatal("no RFC 1918 address found")
	}
}
