package netlogon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/oiweiwei/go-msrpc/dcerpc"
	"github.com/oiweiwei/go-msrpc/msrpc/dcetypes"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
	nlssp "github.com/oiweiwei/go-msrpc/ssp/netlogon"
	"golang.org/x/net/dns/dnsmessage"
)

// netlogonTower is the tower an endpoint mapper answers for NETLOGON on
// ncacn_ip_tcp port 49665.
func netlogonTower() *dcetypes.Tower {
	return dcetypes.FloorsToTower([]*dcetypes.Floor{
		{Protocol: uint8(dcetypes.ProtocolUUID), UUID: logon.LogonSyntaxUUID, VersionMajor: 1, Data: []byte{0, 0}},
		{Protocol: uint8(dcetypes.ProtocolUUID), UUID: dcerpc.TransferNDR, VersionMajor: 2, Data: []byte{0, 0}},
		{Protocol: uint8(dcetypes.ProtocolRPC_CO), Data: []byte{0, 0}},
		{Protocol: uint8(dcetypes.ProtocolTCP), Data: []byte{0xc2, 0x01}},
		{Protocol: uint8(dcetypes.ProtocolIP), Data: []byte{10, 0, 0, 1}},
	})
}

// A tower naming NETLOGON on ncacn_ip_tcp answers its port; a nil tower, or
// one whose floor is shorter than its protocol's data, names none rather
// than panicking.
func TestNetlogonEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tower *dcetypes.Tower
		port  string
		ok    bool
	}{
		{"NETLOGON on tcp", netlogonTower(), "49665", true},
		{"nil tower", nil, "", false},
		{"empty TCP floor", &dcetypes.Tower{TowerOctetString: []byte{1, 0, 1, 0, 7, 0, 0}}, "", false},
		{"empty named-pipe floor", &dcetypes.Tower{TowerOctetString: []byte{1, 0, 1, 0, 0x10, 0, 0}}, "", false},
	} {
		port, ok := netlogonEndpoint(tc.tower)
		if port != tc.port || ok != tc.ok {
			t.Errorf("%s: netlogonEndpoint = %q, %v; want %q, %v", tc.name, port, ok, tc.port, tc.ok)
		}
	}
}

// FuzzTower reads a tower's octets as the endpoint mapper's reply carries
// them. It never panics, a port it answers is a TCP port number, and the
// tower re-encoded from its floors answers the same.
func FuzzTower(f *testing.F) {
	f.Add(netlogonTower().TowerOctetString)
	f.Add([]byte{1, 0, 1, 0, 7, 0, 0})
	f.Add([]byte{1, 0, 1, 0, 0x10, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		tower := &dcetypes.Tower{TowerOctetString: b}
		port, ok := netlogonEndpoint(tower)
		if !ok {
			return
		}
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || strconv.FormatUint(n, 10) != port {
			t.Fatalf("netlogonEndpoint(%x) = %q, want a decimal TCP port", b, port)
		}
		again := dcetypes.FloorsToTower(tower.Floors())
		if port2, ok2 := netlogonEndpoint(again); port2 != port || !ok2 {
			t.Fatalf("netlogonEndpoint(%x) = %q, re-encoded %x = %q, %v", b, port, again.TowerOctetString, port2, ok2)
		}
	})
}

// serveSRV answers every query on a UDP socket on 127.0.0.1:0 with srvs, in
// the order given, and returns its address and the names it was asked.
func serveSRV(t *testing.T, srvs ...dnsmessage.SRVResource) (string, <-chan string) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	asked := make(chan string, 16)
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var q dnsmessage.Message
			if err := q.Unpack(buf[:n]); err != nil || len(q.Questions) != 1 {
				continue
			}
			asked <- q.Questions[0].Name.String()
			resp := dnsmessage.Message{
				Header:    dnsmessage.Header{ID: q.ID, Response: true, Authoritative: true},
				Questions: q.Questions,
			}
			if q.Questions[0].Type == dnsmessage.TypeSRV {
				for _, s := range srvs {
					resp.Answers = append(resp.Answers, dnsmessage.Resource{
						Header: dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Type: dnsmessage.TypeSRV,
							Class: dnsmessage.ClassINET, TTL: 60},
						Body: &s,
					})
				}
			}
			out, err := resp.Pack()
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(out, from)
		}
	}()
	return pc.LocalAddr().String(), asked
}

// Lookup asks the DNS server it is given for the domain's DC locator record
// and answers its controllers by priority, whatever order the server sent
// them in.
func TestLookupAsksTheDNSServerForTheLocatorRecord(t *testing.T) {
	srv, asked := serveSRV(t,
		dnsmessage.SRVResource{Priority: 20, Weight: 100, Port: 389, Target: dnsmessage.MustNewName("dc2.corp.example.")},
		dnsmessage.SRVResource{Priority: 10, Weight: 100, Port: 389, Target: dnsmessage.MustNewName("dc1.corp.example.")},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := NewDialer((&net.Dialer{}).DialContext).Lookup(ctx, "CORP.EXAMPLE", srv)
	if err != nil {
		t.Fatalf("Lookup through %s: %v", srv, err)
	}
	if want := []string{"dc1.corp.example", "dc2.corp.example"}; !slices.Equal(got, want) {
		t.Errorf("Lookup = %v, want %v, by priority", got, want)
	}
	if name := <-asked; name != "_ldap._tcp.dc._msdcs.corp.example." {
		t.Errorf("the DNS server was asked %q, want _ldap._tcp.dc._msdcs.corp.example.", name)
	}
}

// newCert is a certificate naming hosts, names or addresses, signed by parent, or a CA when
// parent is nil.
func newCert(t *testing.T, parent *tls.Certificate, hosts ...string) (*tls.Certificate, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	signer, signKey := tmpl, any(k)
	if parent == nil {
		tmpl.IsCA, tmpl.BasicConstraintsValid, tmpl.KeyUsage = true, true, x509.KeyUsageCertSign
	} else {
		signer, signKey = parent.Leaf, parent.PrivateKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &k.PublicKey, signKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k, Leaf: leaf},
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// A create reaches the pinned controller's address over TLS verified against
// the record's trust roots: a certificate those roots signed for another host
// is refused at the handshake, and no byte of the bind is sent; one they
// signed for the address is accepted and the bind sent.
func TestCreateVerifiesThePinnedController(t *testing.T) {
	ca, caPEM := newCert(t, nil)
	for _, tc := range []struct {
		name   string
		hosts  []string
		accept bool
	}{
		{"another host's certificate", []string{"dc.other.example"}, false},
		{"the address's certificate", []string{"127.0.0.1"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaf, _ := newCert(t, ca, tc.hosts...)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			read := make(chan int64, 1)
			go func() {
				raw, err := ln.Accept()
				if err != nil {
					read <- -1
					return
				}
				srv := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{*leaf}})
				if srv.Handshake() != nil {
					_ = raw.Close()
					read <- 0
					return
				}
				// The bind's first record is all a refused handshake would
				// have kept from being sent.
				n, _ := srv.Read(make([]byte, 1))
				_ = raw.Close()
				read <- int64(n)
			}()
			_, port, _ := net.SplitHostPort(ln.Addr().String())

			rec := directory.Record{Address: "ldaps://dc.corp.example:" + port, Base: "DC=corp,DC=example",
				TrustRoots: caPEM, Realm: "CORP.EXAMPLE"}
			dc, err := NewDialer((&net.Dialer{}).DialContext).Dial(context.Background(), rec, "127.0.0.1")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = dc.Create(ctx, identity.JoinCredential{User: "admin", Password: []byte("pw")},
				Computer{Name: "HOST1", DNSHostName: "host1.corp.example"}, []byte("Machine1!"))
			_ = ln.Close() // a Create that never connected leaves Accept to fail
			n := <-read
			var name x509.HostnameError
			switch {
			case tc.accept && n <= 0:
				t.Errorf("Create to 127.0.0.1 with its certificate: the server read %d bytes, want the bind (err %v)", n, err)
			case !tc.accept && !errors.As(err, &name):
				t.Errorf("Create to 127.0.0.1 with a certificate for dc.other.example: err %v, want an x509.HostnameError", err)
			case !tc.accept && n != 0:
				t.Errorf("Create to 127.0.0.1 with a certificate for dc.other.example: the server read %d bytes, want a refused handshake", n)
			}
		})
	}
}

// logonServer drops a port and answers an IP address as given, and a host
// name's first label in upper case.
func TestLogonServer(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"10.0.0.1", `\\10.0.0.1`},
		{"10.0.0.1:135", `\\10.0.0.1`},
		{"dc1.example.com", `\\DC1`},
		{"dc1.example.com:135", `\\DC1`},
		{"fd00::1", `\\fd00::1`},
		{"[fd00::1]:135", `\\fd00::1`},
	} {
		if got := logonServer(tc.addr); got != tc.want {
			t.Errorf("logonServer(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

// A SetPassword that fails spends the channel: the next answers errSpent
// without dialling the controller again.
func TestSetPasswordSpent(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close() // nothing listens: the association's dial fails
	machine, err := machineCredential("NODE", "EXAMPLE.COM", []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	c := &channel{dc: &dc{dial: (&net.Dialer{}).DialContext}, binding: "ncacn_ip_tcp:127.0.0.1[" + port + "]",
		cfg: &nlssp.Config{Capabilities: offered, Credential: machine}, machine: machine}
	ctx := context.Background()
	if err := c.SetPassword(ctx, []byte("pw")); err == nil || errors.Is(err, errSpent) {
		t.Fatalf("first SetPassword = %v, want the dial's error", err)
	}
	if err := c.SetPassword(ctx, []byte("pw")); !errors.Is(err, errSpent) {
		t.Errorf("SetPassword after a failed one = %v, want errSpent", err)
	}
}
