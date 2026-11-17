package entity

import (
	"net/netip"
	"testing"
)

// A source is the client address, an IPv4-mapped one as IPv4 and any other
// IPv6 one by its /64; loopback is a source like any other; an address that
// is not valid is 0.0.0.0.
func TestSourceKey(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"192.0.2.1", "192.0.2.1/32"},
		{"::ffff:192.0.2.1", "192.0.2.1/32"},
		{"2001:db8::1", "2001:db8::/64"},
		{"2001:db8::ffff:1:2", "2001:db8::/64"},
		{"2001:db8:0:1::1", "2001:db8:0:1::/64"},
		{"fe80::1%eth0", "fe80::/64"},
		{"127.0.0.1", "127.0.0.1/32"},
		{"127.0.0.6", "127.0.0.6/32"},
		{"::ffff:127.0.0.1", "127.0.0.1/32"},
		{"::1", "::/64"},
		{"", "0.0.0.0/32"},
	} {
		var addr netip.Addr
		if tc.addr != "" {
			addr = netip.MustParseAddr(tc.addr)
		}
		if got := SourceKey(addr); got.String() != tc.want {
			t.Errorf("source key of %q: got %v, want %s", tc.addr, got, tc.want)
		}
	}
}
