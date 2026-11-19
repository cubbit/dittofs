package bootstrap

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

// linkPort is one port of a service: the port's name, its number and its
// lower case protocol, the three parts the kubelet makes a link from.
type linkPort struct {
	name  string
	port  int
	proto string
}

// kubeletServices models the services whose variables a pod sees: the ones
// the operator makes for a custom resource, with each port the operator
// sets, and the operator's own metrics service, which a pod sees when it
// runs in the operator's namespace. Every name starts with the pod's own
// prefix, so the mangled name is the variable name's first part. The
// headless service has no address, so no variable is written for it and
// it is absent.
var kubeletServices = []struct {
	name  string
	ports []linkPort
}{
	{
		name: "dittofs-api",
		ports: []linkPort{
			{"api", 8080, "tcp"},
		},
	},
	{
		name: "dittofs-metrics",
		ports: []linkPort{
			{"metrics", 9090, "tcp"},
		},
	},
	{
		name: "dittofs-operator-controller-manager-metrics-service",
		ports: []linkPort{
			{"https", 8443, "tcp"},
		},
	},
	{
		name: "dittofs-adapter-nfs",
		ports: []linkPort{
			{"adapter-nfs", 2049, "tcp"},
			{"adapter-nfs-udp", 2049, "udp"},
			{"portmap", 111, "tcp"},
			{"portmap-udp", 111, "udp"},
			{"mdns", 5353, "udp"},
		},
	},
}

// kubeletEnv writes the environment of one service the way the kubelet
// does: the name upper cased with a hyphen as an underscore, then the
// host, the first port's number and link, and per port its named service
// port and the protocol, link, port and address of its numbered form.
func kubeletEnv(service, ip string, ports []linkPort) map[string]string {
	prefix := kubeletName(service)

	env := map[string]string{
		prefix + "_SERVICE_HOST": ip,
		prefix + "_SERVICE_PORT": strconv.Itoa(ports[0].port),
		prefix + "_PORT":         kubeletLink(ip, ports[0]),
	}

	for _, p := range ports {
		form := prefix + "_PORT_" + strconv.Itoa(p.port) + "_" + strings.ToUpper(p.proto)

		env[prefix+"_SERVICE_PORT_"+kubeletName(p.name)] = strconv.Itoa(p.port)
		env[form] = kubeletLink(ip, p)
		env[form+"_PROTO"] = p.proto
		env[form+"_PORT"] = strconv.Itoa(p.port)
		env[form+"_ADDR"] = ip
	}

	return env
}

// kubeletName upper cases s and turns a hyphen into an underscore: the
// kubelet's mangling of a service or port name into a variable name.
func kubeletName(s string) string {
	return strings.ToUpper(strings.ReplaceAll(s, "-", "_"))
}

// kubeletLink is the link value of one port: the protocol, the host, in
// brackets when it is an IPv6 address, and the port.
func kubeletLink(ip string, p linkPort) string {
	return p.proto + "://" + net.JoinHostPort(ip, strconv.Itoa(p.port))
}

// Every variable a pod gets for the custom resource's services is a link,
// on both IP families.
func TestServiceLinksOfACRPass(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		ip   string
	}{
		{"ipv4", "10.96.0.11"},
		{"ipv6", "fd00::a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := map[string]string{}

			for _, svc := range kubeletServices {
				for name, value := range kubeletEnv(svc.name, tc.ip, svc.ports) {
					env[name] = value
				}
			}

			// Three variables a service always gets, plus five per port:
			// the api one makes 8, the metrics one 8, the operator's
			// metrics one 8 and the adapter's five ports 28. That is 52
			// with the portmapper on, 42 without: its port 111 is
			// published only when the portmapper is on.
			if len(env) != 52 {
				t.Fatalf("the set a pod gets: got %d variables, want 52", len(env))
			}

			for name, value := range env {
				if !serviceLink(name, value) {
					t.Errorf("serviceLink(%q, %q): got false, want true", name, value)
				}
			}
		})
	}
}

// serviceLinkRows is the shapes table: for each form, the value the
// kubelet writes and values it never writes. The fuzz corpus seeds itself
// from these rows too.
var serviceLinkRows = []struct {
	name   string
	value  string
	accept bool
}{
	// A link: the protocol, an IP with brackets only around IPv6, a port.
	{"DITTOFS_METRICS_PORT", "tcp://10.0.0.5:9090", true},
	{"DITTOFS_METRICS_PORT", "udp://10.0.0.5:9090", true},
	{"DITTOFS_API_PORT", "sctp://[fd00::a]:80", true},
	{"DITTOFS_API_PORT_2049_TCP", "tcp://10.0.0.5:2049", true},
	// A name with no service part, for a service named after the program.
	{"DITTOFS_PORT", "tcp://10.0.0.5:80", true},
	{"DITTOFS_SERVICE_HOST", "10.0.0.5", true},
	{"DITTOFS_PORT", "8080", false},
	{"DITTOFS_SERVICE_HOST", "tcp://10.0.0.5:80", false},
	// Values the kubelet never writes into a link.
	{"DITTOFS_METRICS_PORT", "tcp://10.0.0.5", false},
	{"DITTOFS_METRICS_PORT", "tcp://10.0.0.5:0", false},
	{"DITTOFS_METRICS_PORT", "tcp://10.0.0.5:65536", false},
	{"DITTOFS_METRICS_PORT", "tcp://svc:80", false},
	{"DITTOFS_METRICS_PORT", "tcp://fd00::a:9090", false},
	{"DITTOFS_METRICS_PORT", "http://10.0.0.5:80", false},
	{"DITTOFS_METRICS_PORT", "tcp://10.0.0.5:80/", false},
	{"DITTOFS_METRICS_PORT", "tcp://10.0.0.5:80 ", false},
	{"DITTOFS_METRICS_PORT", "tcp://[fe80::1%eth0]:80", false},
	{"DITTOFS_METRICS_PORT", "tcp://[::ffff:10.0.0.5]:80", false},
	{"DITTOFS_METRICS_PORT", "::ffff:10.0.0.5", false},
	{"DITTOFS_METRICS_PORT", "tcp://[10.0.0.5]:80", false},
	{"DITTOFS_METRICS_PORT", "tcp://[fd00::a]", false},
	{"DITTOFS_METRICS_PORT", "tcp://[fd00::a]:80/", false},
	{"DITTOFS_METRICS_PORT", "tcp://[fd00::a]x:80", false},
	{"DITTOFS_METRICS_PORT", "tcp://[fd00::a]x80", false},
	{"DITTOFS_METRICS_PORT", "tcp://[fd00::a:80", false},
	{"DITTOFS_METRICS_PORT", "tcp://]:[", false},
	{"DITTOFS_METRICS_PORT", "tcp:// 10.0.0.5:80", false},
	{"DITTOFS_METRICS_PORT", "TCP://10.0.0.5:80", false},
	{"DITTOFS_METRICS_PORT", "tcp://10.0.0.5:08080", false},
	{"DITTOFS_METRICS_PORT", "", false},
	// Names no real service makes, whatever the value.
	{"DITTOFS_PORT_X_TCP", "tcp://10.0.0.5:80", false},
	{"DITTOFS_X_PORT_80_HTTP", "tcp://10.0.0.5:80", false},
	{"DITTOFS_metrics_PORT", "tcp://10.0.0.5:9090", false},
	{"DITTOFS_METRICS_port", "tcp://10.0.0.5:9090", false},
	{"DITTOFS_ME TRICS_PORT", "tcp://10.0.0.5:9090", false},
	// A retired or misspelled name keeps its refusal with a plain value.
	{"DITTOFS_METRICS_PORT", "9091", false},
	{"DITTOFS_CONTROLPLANE_PORT", "8080", false},
	{"DITTOFS_NFS_PORT", "2049", false},
	{"DITTOFS_NFS_PORT", "s3cr3t", false},
	{"DITTOFS_API_PORT", "8080", false},
	{"DITTOFS_API_SERVICE_PORT", "http", false},
	// A host and an address: an IP, bare, no port, no brackets, no zone.
	{"DITTOFS_API_SERVICE_HOST", "10.0.0.5", true},
	{"DITTOFS_API_SERVICE_HOST", "fd00::a", true},
	{"DITTOFS_API_SERVICE_HOST", "10.0.0.5:80", false},
	{"DITTOFS_API_SERVICE_HOST", "[fd00::a]", false},
	{"DITTOFS_API_SERVICE_HOST", "localhost", false},
	{"DITTOFS_API_SERVICE_HOST", "::ffff:10.0.0.5", false},
	{"DITTOFS_API_SERVICE_HOST", "fe80::1%eth0", false},
	{"DITTOFS_API_SERVICE_HOST", " 10.0.0.5", false},
	{"DITTOFS_API_SERVICE_HOST", "10.0.0.5 ", false},
	// Only the shape is checked: the loopback and unspecified addresses and
	// a spelling that is not the canonical one still pass.
	{"DITTOFS_API_SERVICE_HOST", "127.0.0.1", true},
	{"DITTOFS_API_SERVICE_HOST", "::1", true},
	{"DITTOFS_API_SERVICE_HOST", "::", true},
	{"DITTOFS_API_SERVICE_HOST", "fd00:0:0::a", true},
	{"DITTOFS_API_PORT_80_TCP_ADDR", "10.0.0.5", true},
	{"DITTOFS_API_PORT_80_TCP_ADDR", "fd00::a", true},
	{"DITTOFS_API_PORT_80_TCP_ADDR", "10.0.0.5:80", false},
	{"DITTOFS_API_PORT_80_TCP_ADDR", "[fd00::a]", false},
	{"DITTOFS_API_PORT_80_TCP_ADDR", "localhost", false},
	{"DITTOFS_API_PORT_80_TCP_ADDR", "::ffff:10.0.0.5", false},
	{"DITTOFS_API_PORT_80_TCP_ADDR", "fe80::1%eth0", false},
	// A port number: 1 to 65535, digits only.
	{"DITTOFS_API_SERVICE_PORT", "1", true},
	{"DITTOFS_API_SERVICE_PORT", "65535", true},
	{"DITTOFS_API_SERVICE_PORT", "0", false},
	{"DITTOFS_API_SERVICE_PORT", "65536", false},
	{"DITTOFS_API_SERVICE_PORT", "08080", false},
	{"DITTOFS_API_SERVICE_PORT", "-1", false},
	{"DITTOFS_API_SERVICE_PORT", "80a", false},
	{"DITTOFS_API_SERVICE_PORT", "01", false},
	{"DITTOFS_API_SERVICE_PORT", "+80", false},
	{"DITTOFS_API_SERVICE_PORT", "1_0", false},
	{"DITTOFS_API_SERVICE_PORT", "", false},
	{"DITTOFS_API_SERVICE_PORT_", "80", false},
	{"DITTOFS_ADAPTER_NFS_SERVICE_PORT_MDNS", "5353", true},
	{"DITTOFS_ADAPTER_NFS_SERVICE_PORT_MDNS", "mdns", false},
	{"DITTOFS_API_PORT_2049_TCP", "2049", false},
	{"DITTOFS_ADAPTER_NFS_PORT_111_UDP_PORT", "111", true},
	{"DITTOFS_ADAPTER_NFS_PORT_111_UDP_PORT", "08080", false},
	// A name that parses as more than one form is a link when any form
	// fits the value.
	{"DITTOFS_OPERATOR_CONTROLLER_MANAGER_METRICS_SERVICE_PORT", "8443", true},
	{"DITTOFS_X_SERVICE_PORT", "tcp://10.0.0.5:80", true},
	// A protocol word, lower case.
	{"DITTOFS_API_PORT_2049_TCP_PROTO", "tcp", true},
	{"DITTOFS_API_PORT_2049_UDP_PROTO", "udp", true},
	{"DITTOFS_API_PORT_2049_SCTP_PROTO", "sctp", true},
	{"DITTOFS_API_PORT_2049_TCP_PROTO", "TCP", false},
	{"DITTOFS_API_PORT_2049_TCP_PROTO", "http", false},
	{"DITTOFS_API_PORT_2049_TCP_PROTO", "", false},
}

// nameVariantForms holds one valid name and value for each of the eight
// forms: the service part, the form's ending, the value, and the byte
// that, added at the end of the name, makes no form (a form with a named
// port takes any upper case byte there, so it takes a lower case one).
var nameVariantForms = []struct {
	suffix   string
	value    string
	endByte  string
	numbered bool
}{
	{"SERVICE_HOST", "10.0.0.5", "X", false},
	{"PORT_80_TCP_ADDR", "10.0.0.5", "X", true},
	{"SERVICE_PORT", "80", "X", false},
	{"SERVICE_PORT_HTTP", "80", "x", false},
	{"PORT", "tcp://10.0.0.5:80", "X", false},
	{"PORT_80_TCP", "tcp://10.0.0.5:80", "X", true},
	{"PORT_80_TCP_PORT", "80", "X", true},
	{"PORT_80_TCP_PROTO", "tcp", "X", true},
}

// For each form a link name can take, the value the kubelet writes fits
// and the values it never writes are refused, with the name's service
// part, suffix, port number and protocol checked letter by letter.
func TestServiceLinkShapes(t *testing.T) {
	t.Parallel()

	for _, form := range nameVariantForms {
		name := "DITTOFS_API_" + form.suffix

		accepted := []string{
			name,
			"DITTOFS_" + form.suffix,
			"DITTOFS_API2_" + form.suffix,
			// An empty service segment: the kubelet does not write it, the
			// shape check accepts it and the value decides.
			"DITTOFS__" + form.suffix,
		}

		if form.numbered {
			accepted = append(accepted,
				"DITTOFS_API_"+strings.Replace(form.suffix, "TCP", "UDP", 1),
				"DITTOFS_API_"+strings.Replace(form.suffix, "TCP", "SCTP", 1),
			)
		}

		for _, ok := range accepted {
			if !serviceLink(ok, form.value) {
				t.Fatalf("serviceLink(%q, %q): got false, want true", ok, form.value)
			}
		}

		variants := map[string]string{
			"one byte at the end":            name + form.endByte,
			"one byte at the start":          "X" + name,
			"whole name lower case":          strings.ToLower(name),
			"service part lower":             "DITTOFS_api_" + form.suffix,
			"suffix lower case":              "DITTOFS_API_" + strings.ToLower(form.suffix),
			"a space in the middle":          "DITTOFS_A PI_" + form.suffix,
			"no underscore after the prefix": "DITTOFSAPI_" + form.suffix,
			"service part runs into suffix":  "DITTOFS_API" + form.suffix,
		}

		if form.numbered {
			variants["number not digits"] = "DITTOFS_API_" + strings.Replace(
				form.suffix,
				"80",
				"X",
				1,
			)
			variants["number empty"] = "DITTOFS_API_" + strings.Replace(form.suffix, "80", "", 1)
			variants["protocol http"] = "DITTOFS_API_" + strings.Replace(
				form.suffix,
				"TCP",
				"HTTP",
				1,
			)
			variants["no protocol"] = "DITTOFS_API_" + strings.Replace(form.suffix, "TCP", "", 1)
		}

		for what, variant := range variants {
			t.Run(form.suffix+"/"+what, func(t *testing.T) {
				t.Parallel()

				if serviceLink(variant, form.value) {
					t.Errorf("serviceLink(%q, %q): got true, want false", variant, form.value)
				}
			})
		}
	}

	for _, row := range serviceLinkRows {
		t.Run(row.name+"="+row.value, func(t *testing.T) {
			t.Parallel()

			// A crash on odd input is a failure of this row, not of the run.
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("serviceLink(%q, %q): panicked: %v", row.name, row.value, r)
				}
			}()

			got := serviceLink(row.name, row.value)

			if got != row.accept {
				t.Errorf("serviceLink: got %v, want %v", got, row.accept)
			}
		})
	}
}
