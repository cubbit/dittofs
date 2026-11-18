package netlogon

import (
	"reflect"
	"runtime"
	"testing"

	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
	"github.com/oiweiwei/go-msrpc/ndr"
	nlssp "github.com/oiweiwei/go-msrpc/ssp/netlogon"

	"github.com/cubbit/dittofs/pkg/auth/sid"
)

// dsid is a dtyp.SID of authority 5 and subs.
func dsid(subs ...uint32) *dtyp.SID {
	return &dtyp.SID{Revision: 1, SubAuthorityCount: uint8(len(subs)),
		IDAuthority: &dtyp.SIDIDAuthority{Value: []byte{0, 0, 0, 0, 0, 5}}, SubAuthority: subs}
}

func psid(s string) sid.SID { return *sid.ParseSIDMust(s) }

// samInfo4 is a SamInfo4 as a controller answers alice: two domain groups,
// one extra SID and a 16-byte session key.
func samInfo4() *logon.ValidationSAMInfo4 {
	return &logon.ValidationSAMInfo4{
		UserID:        1103,
		UserFlags:     0x20,
		LogonDomainID: dsid(21, 1, 2, 3),
		GroupCount:    2,
		GroupIDs:      []*logon.GroupMembership{{RelativeID: 513, Attributes: 7}, {RelativeID: 1104, Attributes: 7}},
		UserSessionKey: &logon.UserSessionKey{Data: []*logon.CypherBlock{
			{Data: []byte{1, 2, 3, 4, 5, 6, 7, 8}}, {Data: []byte{9, 10, 11, 12, 13, 14, 15, 16}}}},
		SIDCount:  1,
		ExtraSIDs: []*logon.SIDAndAttributes{{SID: dsid(32, 545), Attributes: 0x10}},
	}
}

// The reply maps the user and the domain groups under LogonDomainID, keeps
// every extra SID and the session key as they came, and checks nothing a
// sign-in decides on, so the sign-in can refuse each.
func TestReplyFrom(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(v *logon.ValidationSAMInfo4)
		want Reply
	}{
		{"alice", func(*logon.ValidationSAMInfo4) {}, Reply{
			User: psid("S-1-5-21-1-2-3-1103"), UserFlags: 0x20, LogonDomain: psid("S-1-5-21-1-2-3"),
			Groups:     []SIDAttr{{psid("S-1-5-21-1-2-3-513"), 7}, {psid("S-1-5-21-1-2-3-1104"), 7}, {psid("S-1-5-32-545"), 0x10}},
			SessionKey: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		}},
		{"no LogonDomainID", func(v *logon.ValidationSAMInfo4) { v.LogonDomainID = nil; v.ExtraSIDs = nil }, Reply{
			UserFlags: 0x20, Groups: []SIDAttr{{sid.SID{}, 7}, {sid.SID{}, 7}},
			SessionKey: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		}},
		{"no session key", func(v *logon.ValidationSAMInfo4) {
			v.UserSessionKey = nil
			v.GroupIDs, v.ExtraSIDs = nil, nil
		}, Reply{User: psid("S-1-5-21-1-2-3-1103"), UserFlags: 0x20, LogonDomain: psid("S-1-5-21-1-2-3")}},
		{"15-byte session key", func(v *logon.ValidationSAMInfo4) {
			v.UserSessionKey.Data[1].Data = v.UserSessionKey.Data[1].Data[:7]
			v.GroupIDs, v.ExtraSIDs = nil, nil
		}, Reply{User: psid("S-1-5-21-1-2-3-1103"), UserFlags: 0x20, LogonDomain: psid("S-1-5-21-1-2-3"),
			SessionKey: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}}},
		{"empty extra SID", func(v *logon.ValidationSAMInfo4) {
			v.GroupIDs = nil
			v.ExtraSIDs = []*logon.SIDAndAttributes{{SID: nil, Attributes: 4}}
			v.UserSessionKey = nil
		}, Reply{User: psid("S-1-5-21-1-2-3-1103"), UserFlags: 0x20, LogonDomain: psid("S-1-5-21-1-2-3"),
			Groups: []SIDAttr{{sid.SID{}, 4}}}},
	} {
		v := samInfo4()
		tc.edit(v)
		got, err := replyFrom(v)
		if err != nil {
			t.Errorf("%s: replyFrom: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: replyFrom = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// A SID longer than MS-DTYP allows is refused, not truncated.
func TestReplyFromRefusesLongSID(t *testing.T) {
	v := samInfo4()
	v.ExtraSIDs[0].SID = dsid(make([]uint32, 16)...)
	if _, err := replyFrom(v); err == nil {
		t.Errorf("replyFrom with a 16-sub-authority extra SID = nil error, want it refused")
	}
}

// NetrServerAuthenticate3 is offered no RC4, and a reply whose flags lack
// AES refuses the channel.
func TestNegotiateFlags(t *testing.T) {
	if offered&nlssp.CapRC4 != 0 {
		t.Errorf("offered flags 0x%08x hold RC4", uint32(offered))
	}
	if offered&nlssp.CapAES_SHA2 == 0 {
		t.Errorf("offered flags 0x%08x lack AES", uint32(offered))
	}
	if err := checkNegotiated(uint32(offered)); err != nil {
		t.Errorf("checkNegotiated(offered) = %v, want nil", err)
	}
	if err := checkNegotiated(uint32(offered &^ nlssp.CapAES_SHA2)); err == nil {
		t.Errorf("checkNegotiated without AES = nil, want the channel refused")
	}
	if err := checkNegotiated(uint32(offered&^nlssp.CapAES_SHA2 | nlssp.CapRC4)); err == nil {
		t.Errorf("checkNegotiated with RC4 and no AES = nil, want the channel refused")
	}
}

// FuzzSamInfo4 decodes a SamInfo4 off the wire as NetrLogonSamLogonEx's reply
// carries it, under the guard Logon decodes under, and maps it to a Reply. It
// never panics, allocates at most a
// constant times the input's length, and a decoded value re-encoded by
// go-msrpc's NDR encoder decodes to the same Reply.
func FuzzSamInfo4(f *testing.F) {
	full, err := ndr.Marshal(samInfo4())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(full)
	empty, err := ndr.Marshal(&logon.ValidationSAMInfo4{})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(empty)
	f.Fuzz(func(t *testing.T, b []byte) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		start := ms.TotalAlloc
		var v logon.ValidationSAMInfo4
		if err := decoded(func() error { return ndr.Unmarshal(b, &v) }); err != nil {
			return
		}
		rep, err := replyFrom(&v)
		runtime.ReadMemStats(&ms)
		if used, limit := ms.TotalAlloc-start, 64*uint64(len(b))+64<<10; used > limit {
			t.Fatalf("decoding %d bytes allocated %d, want at most %d", len(b), used, limit)
		}
		// The counts are redundant with the arrays the Reply reads, and the
		// encoder writes as many elements as the count says.
		v.GroupCount, v.SIDCount = uint32(len(v.GroupIDs)), uint32(len(v.ExtraSIDs))
		fixCount(v.LogonDomainID)
		for _, x := range v.ExtraSIDs {
			if x != nil {
				fixCount(x.SID)
			}
		}
		again, err2 := ndr.Marshal(&v)
		if err2 != nil {
			return // a value go-msrpc cannot encode back, such as a count its array disagrees with
		}
		var w logon.ValidationSAMInfo4
		if err := decoded(func() error { return ndr.Unmarshal(again, &w) }); err != nil {
			t.Fatalf("re-encoded %x does not decode: %v", again, err)
		}
		rep2, err3 := replyFrom(&w)
		if (err == nil) != (err3 == nil) {
			t.Fatalf("replyFrom = %v, re-encoded %v; want both nil or both errors", err, err3)
		}
		if err == nil && !reflect.DeepEqual(rep, rep2) {
			t.Fatalf("Reply %+v, re-encoded %+v", rep, rep2)
		}
	})
}

// fixCount sets s's count to its array's length, as the encoder needs.
func fixCount(s *dtyp.SID) {
	if s != nil {
		s.SubAuthorityCount = uint8(len(s.SubAuthority))
	}
}
