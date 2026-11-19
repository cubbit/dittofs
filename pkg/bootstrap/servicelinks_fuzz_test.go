package bootstrap

import (
	"net"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// oracleForm is one form spelled here apart from serviceLink: the
// pattern of its name, and a judge of its value written from the standard
// library's parsers instead of the code's own helpers.
type oracleForm struct {
	name  *regexp.Regexp
	value func(string) bool
}

// oracleForms is the name half of the shapes, as patterns, each with its
// independent value judge. The fuzz target holds serviceLink against
// them: a pair is accepted exactly when the name fits a form and the
// value fits that same form.
var oracleForms = []oracleForm{
	{regexp.MustCompile(`^DITTOFS_([A-Z0-9_]*_)?SERVICE_HOST$`), oracleIP},
	{regexp.MustCompile(`^DITTOFS_([A-Z0-9_]*_)?PORT_[0-9]+_(TCP|UDP|SCTP)_ADDR$`), oracleIP},
	{regexp.MustCompile(`^DITTOFS_([A-Z0-9_]*_)?SERVICE_PORT$`), oraclePort},
	{regexp.MustCompile(`^DITTOFS_([A-Z0-9_]*_)?SERVICE_PORT_[A-Z0-9_]+$`), oraclePort},
	{regexp.MustCompile(`^DITTOFS_([A-Z0-9_]*_)?PORT$`), oracleLink},
	{regexp.MustCompile(`^DITTOFS_([A-Z0-9_]*_)?PORT_[0-9]+_(TCP|UDP|SCTP)$`), oracleLink},
	{regexp.MustCompile(`^DITTOFS_([A-Z0-9_]*_)?PORT_[0-9]+_(TCP|UDP|SCTP)_PORT$`), oraclePort},
	{regexp.MustCompile(`^DITTOFS_([A-Z0-9_]*_)?PORT_[0-9]+_(TCP|UDP|SCTP)_PROTO$`), oracleProto},
}

// oracleIP judges an address with net.ParseIP: no zone (the parser refuses
// one), and no IPv4 written with a colon, which is an IPv4 mapped onto
// IPv6.
func oracleIP(v string) bool {
	ip := net.ParseIP(v)
	if ip == nil {
		return false
	}

	return !strings.Contains(v, ":") || ip.To4() == nil
}

// oraclePort judges a port with strconv.Atoi: 1 to 65535, and a round
// trip that leaves no sign and no leading zero.
func oraclePort(v string) bool {
	n, err := strconv.Atoi(v)
	if err != nil {
		return false
	}

	return n >= 1 && n <= 65535 && strconv.Itoa(n) == v
}

// oracleProto judges a protocol word against the fixed list.
func oracleProto(v string) bool {
	return v == "tcp" || v == "udp" || v == "sctp"
}

// oracleLink judges a link by splitting it the standard way: scheme, then
// host and port with net.SplitHostPort, an IPv4 host bare and an IPv6
// host in brackets.
func oracleLink(v string) bool {
	scheme, rest, ok := strings.Cut(v, "://")
	if !ok || !oracleProto(scheme) {
		return false
	}

	host, port, err := net.SplitHostPort(rest)
	if err != nil || !oraclePort(port) || !oracleIP(host) {
		return false
	}

	return strings.HasPrefix(rest, "[") == strings.Contains(host, ":")
}

// oracleAccepts reports whether some form fits both the name and the
// value.
func oracleAccepts(name, value string) bool {
	for _, form := range oracleForms {
		if form.name.MatchString(name) && form.value(value) {
			return true
		}
	}

	return false
}

// Any name and value: no panic however odd the input, the same pair always
// the same answer, and the answer equals the independent oracle's.
func FuzzServiceLink(f *testing.F) {
	for _, svc := range kubeletServices {
		for _, ip := range []string{"10.96.0.11", "fd00::a"} {
			for name, value := range kubeletEnv(svc.name, ip, svc.ports) {
				f.Add(name, value)
			}
		}
	}

	for _, row := range serviceLinkRows {
		f.Add(row.name, row.value)
	}

	// Names and values no service of the kubelet can produce.
	for _, name := range []string{
		"", "DITTOFS_", "dittofs_api_port", "DITTOFS_\x00_PORT",
		"DITTOFS_Ä_PORT", "DITTOFS_API_PORT",
	} {
		f.Add(name, "tcp://10.0.0.5:80\x00")
	}

	f.Fuzz(func(t *testing.T, name, value string) {
		got := serviceLink(name, value)

		if serviceLink(name, value) != got {
			t.Fatalf("serviceLink(%q, %q): not stable on the same input", name, value)
		}

		if want := oracleAccepts(name, value); got != want {
			t.Fatalf("serviceLink(%q, %q): got %v, the oracle says %v", name, value, got, want)
		}
	})
}
