package bootstrap

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// serviceLinkForm is one shape a service link pair can have: the form a
// name ends in, and the value that goes with it.
type serviceLinkForm struct {
	name  *regexp.Regexp
	value func(string) bool
}

// serviceLinkForms holds every form of RFC 13 Appendix C's service link
// row, each coupled to the shape of its value. A name can end in more
// than one form for different service parts, and any of them that fits
// the value decides. The name part of a form is the program's DITTOFS_
// prefix, then an optional service part, upper case with a
// hyphen as an underscore, empty for a service named after the program
// itself, then the form's own ending, all in the exact case the kubelet
// writes.
var serviceLinkForms = []serviceLinkForm{
	{
		name:  regexp.MustCompile(`^DITTOFS_([A-Z0-9_]*_)?SERVICE_HOST$`),
		value: isKubeletIP,
	},
	{
		name:  regexp.MustCompile(`^DITTOFS_([A-Z0-9_]*_)?PORT_[0-9]+_(TCP|UDP|SCTP)_ADDR$`),
		value: isKubeletIP,
	},
	{
		name:  regexp.MustCompile(`^DITTOFS_([A-Z0-9_]*_)?SERVICE_PORT$`),
		value: isKubeletPort,
	},
	{
		name:  regexp.MustCompile(`^DITTOFS_([A-Z0-9_]*_)?SERVICE_PORT_[A-Z0-9_]+$`),
		value: isKubeletPort,
	},
	{
		name:  regexp.MustCompile(`^DITTOFS_([A-Z0-9_]*_)?PORT$`),
		value: isKubeletLink,
	},
	{
		name:  regexp.MustCompile(`^DITTOFS_([A-Z0-9_]*_)?PORT_[0-9]+_(TCP|UDP|SCTP)$`),
		value: isKubeletLink,
	},
	{
		name:  regexp.MustCompile(`^DITTOFS_([A-Z0-9_]*_)?PORT_[0-9]+_(TCP|UDP|SCTP)_PORT$`),
		value: isKubeletPort,
	},
	{
		name:  regexp.MustCompile(`^DITTOFS_([A-Z0-9_]*_)?PORT_[0-9]+_(TCP|UDP|SCTP)_PROTO$`),
		value: isKubeletProto,
	},
}

// serviceLink reports whether the kubelet could have written the pair: a
// name that ends in one of RFC 13 Appendix C's service link forms, with
// a value of that form's shape, matched to the byte, IPv6 in brackets
// inside a link and bare in an address, exact case.
//
// decision: the value decides, not the name. The kubelet writes these
// variables for every service with an address, so a pair that fits a form
// is treated as a variable the program did not choose, and a name that
// fits a form with any other value keeps its refusal: a retired name such
// as the v1 metrics port's cannot hide behind its shape while holding a
// plain port number. Only the shape is checked, as RFC 13 asks, not the
// exact value: an IPv6 address in any spelling, the unspecified address,
// a port number in a name with no range, and a protocol word that
// differs from the one the name ends in all pass.
func serviceLink(name, value string) bool {
	for _, form := range serviceLinkForms {
		if form.name.MatchString(name) && form.value(value) {
			return true
		}
	}

	return false
}

// isKubeletIP reports whether v is the address a service link holds as
// the kubelet writes it: an IPv4 address, or a plain IPv6 one, no
// brackets, no port, no zone and no IPv4 mapped onto IPv6. Cluster
// addresses are none of the last two. An octet with a
// leading zero fails netip.ParseAddr, so it is refused: only the canonical
// spelling passes.
func isKubeletIP(v string) bool {
	addr, err := netip.ParseAddr(v)
	if err != nil {
		return false
	}

	return addr.Zone() == "" && !addr.Is4In6()
}

// isKubeletPort reports whether v is a port number as the kubelet writes
// it: 1 to 65535, digits only, without a leading zero.
func isKubeletPort(v string) bool {
	if len(v) > 1 && v[0] == '0' {
		return false
	}

	port, err := strconv.ParseUint(v, 10, 16)
	if err != nil {
		return false
	}

	return port > 0
}

// isKubeletProto reports whether v is a service link's protocol word, in
// the lower case the kubelet writes.
func isKubeletProto(v string) bool {
	switch v {
	case "tcp", "udp", "sctp":
		return true
	}

	return false
}

// isKubeletLink reports whether v is a service link as the kubelet writes
// it: a lower case protocol, an IPv4 address bare or an IPv6 one in
// brackets, and a port number. A zone, an IPv4 mapped onto IPv6, an IPv4
// address in brackets and an IPv6 address without brackets are not what the
// kubelet writes.
func isKubeletLink(v string) bool {
	scheme, hostPort, ok := strings.Cut(v, "://")
	if !ok || !isKubeletProto(scheme) {
		return false
	}

	bracketed := strings.HasPrefix(hostPort, "[")

	var host, port string

	if bracketed {
		end := strings.Index(hostPort, "]")
		if end < 0 {
			return false
		}

		host = hostPort[1:end]

		rest := hostPort[end+1:]
		if !strings.HasPrefix(rest, ":") {
			return false
		}

		port = rest[1:]
	} else {
		var cut bool

		host, port, cut = strings.Cut(hostPort, ":")
		if !cut {
			return false
		}
	}

	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}

	// Without brackets the host has no colon, so only an IPv4 address
	// can have parsed.
	if bracketed && (addr.Zone() != "" || addr.Is4In6() || addr.Is4()) {
		return false
	}

	return isKubeletPort(port)
}
