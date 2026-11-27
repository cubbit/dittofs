package identitytest

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"time"
	"unicode/utf16"

	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/iana/chksumtype"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/pkg/auth/sid"
)

// PAC buffer types (MS-PAC 2.4) the realm writes.
const (
	PACLogonInfo       = 1
	PACServerSignature = 6
	PACKDCSignature    = 7
	PACClientInfo      = 10
)

// PACBuffer is one buffer of a PAC: its type and its bytes.
type PACBuffer struct {
	Type uint32
	Data []byte
}

// GroupRID is a group membership: a RID under a domain SID, and its
// attributes (MS-PAC 2.2.2).
type GroupRID struct {
	RID, Attributes uint32
}

// SIDAttrs is a SID, written as "S-1-5-…", and its attributes.
type SIDAttrs struct {
	SID        string
	Attributes uint32
}

// LogonInfo is what a logon-information buffer says of its user
// (MS-PAC 2.5): every field the PAC's SIDs are read from, the rest zero.
type LogonInfo struct {
	Name, Domain   string
	DomainSID      string // the logon domain's SID, under which UserRID and Groups lie
	UserRID        uint32
	Groups         []GroupRID
	ExtraSIDs      []SIDAttrs
	ResourceDomain string // the resource groups' domain SID; none
	ResourceGroups []GroupRID
	// ResourceFlagClear leaves UserFlags' resource-groups bit clear, though
	// ResourceDomain and ResourceGroups are written.
	ResourceFlagClear bool
}

// PACOpts is what a PAC carries. Every zero field takes the default its
// comment names.
type PACOpts struct {
	Service string // the service name whose key signs the PAC
	Etype   int32  // that key's encryption type; AES256
	// Key signs the PAC instead of the realm's key of Service.
	Key      types.EncryptionKey
	Client   string    // PAC_CLIENT_INFO's name; "alice"
	AuthTime time.Time // PAC_CLIENT_INFO's time: the ticket's authentication time
	Logon    []LogonInfo
	// ServerType is the server signature's type, and the checksum its value
	// is computed with: a keyed one under Key, or under the realm's key of
	// Service of the checksum's own encryption type when Key's differs; the
	// keyed checksum of the key's encryption type.
	ServerType int32
	// ServerLen is the server signature value's length; the checksum's, 4
	// for CRC32 and 12 for the others. The checksum fills its first bytes,
	// the rest zero.
	ServerLen int
	// Extra are buffers laid out after the signatures, as given; the server
	// signature covers them as it covers every byte.
	Extra []PACBuffer
}

// PAC is a PAC as a domain controller issues it: a logon-information buffer
// for each of o.Logon, in order, the client information, the server
// signature under the service key over the whole PAC with both signature
// values zeroed, and the KDC signature over the server signature's value,
// under a key of its own (MS-PAC 2.8).
func (r *Realm) PAC(o PACOpts) []byte {
	if o.Etype == 0 {
		o.Etype = etypeID.AES256_CTS_HMAC_SHA1_96
	}
	if o.Key.KeyType == 0 {
		o.Key = r.Key(o.Service, o.Etype)
	}
	if o.Client == "" {
		o.Client = "alice"
	}
	et, err := crypto.GetEtype(o.Key.KeyType)
	if err != nil {
		panic(err)
	}
	if o.ServerType == 0 {
		o.ServerType = et.GetHashID()
	}
	if o.ServerLen == 0 {
		o.ServerLen = 12
		if o.ServerType == chksumtype.CRC32 {
			o.ServerLen = 4
		}
	}
	var bufs []PACBuffer
	for _, l := range o.Logon {
		bufs = append(bufs, PACBuffer{PACLogonInfo, l.marshal()})
	}
	name := utf16le(o.Client)
	ci := binary.LittleEndian.AppendUint64(nil, filetime(o.AuthTime))
	ci = binary.LittleEndian.AppendUint16(ci, uint16(len(name)))
	bufs = append(bufs, PACBuffer{PACClientInfo, append(ci, name...)})
	kdc := mustKey(etypeID.AES256_CTS_HMAC_SHA1_96)
	ket, _ := crypto.GetEtype(kdc.KeyType)
	srv := len(bufs)
	bufs = append(bufs,
		PACBuffer{PACServerSignature, append(le32(uint32(o.ServerType)), make([]byte, o.ServerLen)...)},
		PACBuffer{PACKDCSignature, append(le32(uint32(ket.GetHashID())), make([]byte, 12)...)})
	bufs = append(bufs, o.Extra...)
	b, offs := layout(bufs)
	sk := o.Key
	if ce, err := crypto.GetChksumEtype(o.ServerType); err == nil && ce.GetETypeID() != sk.KeyType {
		sk = r.Key(o.Service, ce.GetETypeID())
	}
	copy(b[offs[srv]+4:], checksum(o.ServerType, sk, b))
	ksum, err := ket.GetChecksumHash(kdc.KeyValue, b[offs[srv]+4:offs[srv]+4+o.ServerLen], keyusage.KERB_NON_KERB_CKSUM_SALT)
	if err != nil {
		panic(err)
	}
	copy(b[offs[srv+1]+4:], ksum)
	return b
}

// checksum is b's checksum of type typ: CRC32 unkeyed (RFC 3961 §6.1.3),
// any other the keyed checksum gokrb5 knows for typ, computed under key.
func checksum(typ int32, key types.EncryptionKey, b []byte) []byte {
	if typ == chksumtype.CRC32 {
		return le32(^crc32.Update(^uint32(0), crc32.IEEETable, b))
	}
	et, err := crypto.GetChksumEtype(typ)
	if err != nil {
		panic(err)
	}
	sum, err := et.GetChecksumHash(key.KeyValue, b, keyusage.KERB_NON_KERB_CKSUM_SALT)
	if err != nil {
		panic(err)
	}
	return sum
}

// EncodePAC lays bufs out as a PAC, as given, signing nothing: the header,
// the buffer array, then each buffer at an offset aligned to 8, zero-padded.
func EncodePAC(bufs []PACBuffer) []byte {
	b, _ := layout(bufs)
	return b
}

func layout(bufs []PACBuffer) ([]byte, []int) {
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(bufs)))
	b = binary.LittleEndian.AppendUint32(b, 0)
	off := align8(len(b) + 16*len(bufs))
	offs := make([]int, len(bufs))
	for i, x := range bufs {
		offs[i] = off
		b = binary.LittleEndian.AppendUint32(b, x.Type)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(x.Data)))
		b = binary.LittleEndian.AppendUint64(b, uint64(off))
		off = align8(off + len(x.Data))
	}
	out := make([]byte, off)
	copy(out, b)
	for i, x := range bufs {
		copy(out[offs[i]:], x.Data)
	}
	return out, offs
}

func align8(n int) int { return (n + 7) &^ 7 }

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

// filetime is t in 100-nanosecond intervals since 1601 (MS-DTYP 2.3.3).
func filetime(t time.Time) uint64 { return uint64(t.Unix())*1e7 + 116444736000000000 }

// ndr writes NDR's little-endian transfer syntax (MS-RPCE 2.2.5), every
// field aligned to 4 bytes as the logon information's are.
type ndr struct {
	bytes.Buffer
	ref uint32
}

func (w *ndr) u16(v uint16) { _ = binary.Write(w, binary.LittleEndian, v) }
func (w *ndr) u32(v uint32) { _ = binary.Write(w, binary.LittleEndian, v) }

// ptr writes a unique pointer's referent ID, or null when absent.
func (w *ndr) ptr(present bool) {
	if !present {
		w.u32(0)
		return
	}
	w.ref += 4
	w.u32(0x00020000 + w.ref)
}

func (w *ndr) pad4() {
	for w.Len()%4 != 0 {
		w.WriteByte(0)
	}
}

// str writes an RPC_UNICODE_STRING's fixed part.
func (w *ndr) str(s string) {
	n := uint16(2 * len(utf16.Encode([]rune(s))))
	w.u16(n)
	w.u16(n)
	w.ptr(true)
}

// strData writes an RPC_UNICODE_STRING's deferred characters.
func (w *ndr) strData(s string) {
	u := utf16.Encode([]rune(s))
	w.u32(uint32(len(u)))
	w.u32(0)
	w.u32(uint32(len(u)))
	w.Write(utf16le(s))
	w.pad4()
}

// sid writes an RPC_SID, its conformant count first.
func (w *ndr) sid(s string) {
	p := sid.ParseSIDMust(s)
	w.u32(uint32(p.SubAuthorityCount))
	sid.EncodeSID(&w.Buffer, p)
}

func (w *ndr) groups(gs []GroupRID) {
	w.u32(uint32(len(gs)))
	for _, g := range gs {
		w.u32(g.RID)
		w.u32(g.Attributes)
	}
}

// marshal is l as KERB_VALIDATION_INFO, in the type serialization version 1
// of MS-RPCE 2.2.6, the form a PAC's logon-information buffer takes.
func (l LogonInfo) marshal() []byte {
	var w ndr
	w.u32(0x00020000) // the top-level pointer's referent
	w.Write(make([]byte, 6*8))
	strs := []string{l.Name, "", "", "", "", ""}
	for _, s := range strs {
		w.str(s)
	}
	w.u16(0) // LogonCount
	w.u16(0) // BadPasswordCount
	w.u32(l.UserRID)
	primary := uint32(513)
	if len(l.Groups) > 0 {
		primary = l.Groups[0].RID
	}
	w.u32(primary)
	w.u32(uint32(len(l.Groups)))
	w.ptr(len(l.Groups) > 0)
	var flags uint32
	if len(l.ExtraSIDs) > 0 {
		flags |= 0x20 // D: ExtraSids populated
	}
	if l.ResourceDomain != "" && !l.ResourceFlagClear {
		flags |= 0x200 // H: ResourceGroupIds populated
	}
	w.u32(flags)
	w.Write(make([]byte, 16)) // UserSessionKey
	w.str("")                 // LogonServer
	w.str(l.Domain)
	w.ptr(true)               // LogonDomainId
	w.Write(make([]byte, 8))  // Reserved1
	w.u32(0x10)               // UserAccountControl: a normal account
	w.u32(0)                  // SubAuthStatus
	w.Write(make([]byte, 16)) // LastSuccessfulILogon, LastFailedILogon
	w.u32(0)                  // FailedILogonCount
	w.u32(0)                  // Reserved3
	w.u32(uint32(len(l.ExtraSIDs)))
	w.ptr(len(l.ExtraSIDs) > 0)
	w.ptr(l.ResourceDomain != "")
	w.u32(uint32(len(l.ResourceGroups)))
	w.ptr(len(l.ResourceGroups) > 0)

	for _, s := range strs {
		w.strData(s)
	}
	if len(l.Groups) > 0 {
		w.groups(l.Groups)
	}
	w.strData("")
	w.strData(l.Domain)
	w.sid(l.DomainSID)
	if len(l.ExtraSIDs) > 0 {
		w.u32(uint32(len(l.ExtraSIDs)))
		for _, e := range l.ExtraSIDs {
			w.ptr(true)
			w.u32(e.Attributes)
		}
		for _, e := range l.ExtraSIDs {
			w.sid(e.SID)
		}
	}
	if l.ResourceDomain != "" {
		w.sid(l.ResourceDomain)
	}
	if len(l.ResourceGroups) > 0 {
		w.groups(l.ResourceGroups)
	}
	for w.Len()%8 != 0 {
		w.WriteByte(0)
	}
	body := w.Bytes()
	hdr := []byte{1, 0x10, 8, 0, 0xcc, 0xcc, 0xcc, 0xcc}
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(len(body)))
	hdr = binary.LittleEndian.AppendUint32(hdr, 0)
	return append(hdr, body...)
}
