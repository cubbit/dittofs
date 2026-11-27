package netlogon

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"reflect"
	"runtime"
	"testing"
	"unicode/utf16"

	"github.com/oiweiwei/go-msrpc/dcerpc"
	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
	"github.com/oiweiwei/go-msrpc/ndr"
	"github.com/oiweiwei/go-msrpc/ssp/credential"
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

// A SID longer than MS-DTYP allows is refused, not truncated, and so is a
// LogonDomainID whose user or group SID would be.
func TestReplyFromRefusesLongSID(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(v *logon.ValidationSAMInfo4)
		ok   bool
	}{
		{"16-sub-authority extra SID", func(v *logon.ValidationSAMInfo4) { v.ExtraSIDs[0].SID = dsid(make([]uint32, 16)...) }, false},
		{"15-sub-authority LogonDomainID", func(v *logon.ValidationSAMInfo4) { v.LogonDomainID = dsid(make([]uint32, 15)...) }, false},
		{"14-sub-authority LogonDomainID", func(v *logon.ValidationSAMInfo4) { v.LogonDomainID = dsid(make([]uint32, 14)...) }, true},
	} {
		v := samInfo4()
		tc.edit(v)
		if _, err := replyFrom(v); (err == nil) != tc.ok {
			t.Errorf("%s: replyFrom error = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// The machine credential is keyed by the password's NT hash, MS-NLMP
// 4.2.2.1.2's for "Password".
func TestMachineCredentialNTHash(t *testing.T) {
	c, err := machineCredential("NODE", "EXAMPLE.COM", []byte("Password"))
	if err != nil {
		t.Fatal(err)
	}
	h, ok := c.(credential.NTHash)
	if !ok {
		t.Fatalf("machineCredential = %T, want an NT hash credential", c)
	}
	if got, want := hex.EncodeToString(h.NTHash()), "a4f49c406510bdcab6824ee7c30fd852"; got != want {
		t.Errorf("NT hash of \"Password\" = %s, want %s", got, want)
	}
}

// utf16le encodes as utf16.Encode over the password's runes does, invalid
// bytes as U+FFFD and astral runes as surrogate pairs, in a buffer it never
// grows past twice the password's length.
func TestUTF16LE(t *testing.T) {
	for _, in := range []string{"", "Password", "p\u00e4ssw\u00f6rd", "\u5bc6\u7801", "\U0001F600x", "\xff\xfe", "a\xe2\x82"} {
		var want []byte
		for _, u := range utf16.Encode([]rune(in)) {
			want = binary.LittleEndian.AppendUint16(want, u)
		}
		got := utf16le([]byte(in))
		if !bytes.Equal(got, want) {
			t.Errorf("utf16le(%q) = %x, want %x", in, got, want)
		}
		if cap(got) != 2*len(in) {
			t.Errorf("utf16le(%q)'s capacity = %d, want %d: the buffer grew", in, cap(got), 2*len(in))
		}
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

// samLogonExStub is NetrLogonSamLogonEx's reply stub carrying info, a
// SamInfo4 as ndr.Marshal encodes it: the union's switch and the pointer to
// it, info, then Authoritative, ExtraFlags and a zero Return.
func samLogonExStub(info []byte) []byte {
	stub := append([]byte{6, 0, 0, 0, 5, 0, 0, 0}, info...)
	return append(stub, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
}

// decodeReply decodes stub into resp as invoke's operation does under
// go-msrpc: dcerpc.NewBody starts the decode on a goroutine of its own, the
// stub is handed to it as one fragment, and the body is closed as
// Invoke closes it on return.
func decodeReply(stub []byte, resp response) error {
	op := &guardedOp{req: &logon.SAMLogonExRequest{}, resp: resp, done: make(chan struct{})}
	body := dcerpc.NewBody(context.Background(), op, &dcerpc.Presentation{TransferSyntax: dcerpc.TransferNDRSyntaxV2_0}, false)
	_, _ = body.DecodeFrom(stub, ndr.DefaultDataRepresentation, len(stub))
	body.Close()
	<-op.done
	return op.err
}

// A reply decodes on go-msrpc's goroutine into what the controller sent, and
// one go-msrpc's decoder panics on, or one cut short, is an error rather
// than the end of the process.
func TestInvokeDecode(t *testing.T) {
	good, err := ndr.Marshal(samInfo4())
	if err != nil {
		t.Fatal(err)
	}
	crasher := []byte("00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000\x00\x00\x00\x010000000000000000\x00\x00\x00\x01")
	for _, tc := range []struct {
		name string
		stub []byte
		ok   bool
	}{
		{"SamInfo4", samLogonExStub(good), true},
		{"aliased pointers", samLogonExStub(crasher), false},
		{"cut short", samLogonExStub(good)[:100], false},
	} {
		out := &logon.SAMLogonExResponse{}
		err := decodeReply(tc.stub, out)
		if (err == nil) != tc.ok {
			t.Errorf("%s: decode error = %v, want ok=%v", tc.name, err, tc.ok)
			continue
		}
		if !tc.ok {
			continue
		}
		v, _ := out.ValidationInformation.GetValue().(*logon.ValidationSAMInfo4)
		if v == nil {
			t.Fatalf("%s: decoded %T, want SamInfo4", tc.name, out.ValidationInformation.GetValue())
		}
		got, err := replyFrom(v)
		want, _ := replyFrom(samInfo4())
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Reply = %+v, %v; want %+v", tc.name, got, err, want)
		}
	}
}

// FuzzSamInfo4 decodes a SamInfo4 off the wire inside NetrLogonSamLogonEx's
// reply, through the path Logon decodes it on: go-msrpc's decoding goroutine,
// under the operation invoke sends. It never panics, allocates at most a
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
		v, err := decodeSamInfo4(b)
		if err != nil {
			return
		}
		rep, repErr := replyFrom(v)
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
		again, err2 := ndr.Marshal(v)
		if err2 != nil {
			return // a value go-msrpc cannot encode back, such as a count its array disagrees with
		}
		w, err := decodeSamInfo4(again)
		if err != nil {
			t.Fatalf("re-encoded %x does not decode: %v", again, err)
		}
		rep2, err3 := replyFrom(w)
		if (repErr == nil) != (err3 == nil) {
			t.Fatalf("replyFrom = %v, re-encoded %v; want both nil or both errors", repErr, err3)
		}
		if repErr == nil && !reflect.DeepEqual(rep, rep2) {
			t.Fatalf("Reply %+v, re-encoded %+v", rep, rep2)
		}
	})
}

// decodeSamInfo4 is the SamInfo4 b encodes, decoded inside a
// NetrLogonSamLogonEx reply by decodeReply.
func decodeSamInfo4(b []byte) (*logon.ValidationSAMInfo4, error) {
	out := &logon.SAMLogonExResponse{}
	if err := decodeReply(samLogonExStub(b), out); err != nil {
		return nil, err
	}
	v, _ := out.ValidationInformation.GetValue().(*logon.ValidationSAMInfo4)
	if v == nil {
		return nil, errors.New("no SamInfo4")
	}
	return v, nil
}

// fixCount sets s's count to its array's length, as the encoder needs.
func fixCount(s *dtyp.SID) {
	if s != nil {
		s.SubAuthorityCount = uint8(len(s.SubAuthority))
	}
}
