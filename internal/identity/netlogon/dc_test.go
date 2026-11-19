package netlogon

import (
	"strconv"
	"testing"

	"github.com/oiweiwei/go-msrpc/dcerpc"
	"github.com/oiweiwei/go-msrpc/msrpc/dcetypes"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
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
