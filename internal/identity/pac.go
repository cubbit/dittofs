package identity

import (
	"bytes"
	"crypto/hmac"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
	"unicode/utf16"

	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/iana/chksumtype"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/pac"
	"github.com/jcmturner/gokrb5/v8/types"
)

// PAC buffer types (MS-PAC 2.4) identity reads.
const (
	pacLogonInfo       = 1
	pacServerSignature = 6
	pacKDCSignature    = 7
	pacClientInfo      = 10
)

// errPACClient is a verified PAC whose client information names another
// client or another authentication time than its ticket's.
var errPACClient = errors.New("the PAC's client information differs from the ticket's")

// pacBuffer is one buffer of a PAC: its type, its offset in the PAC, and its
// bytes, a slice of the PAC's.
type pacBuffer struct {
	typ  uint32
	off  int
	data []byte
}

// pacBuffers walks a PAC's header and buffer array (MS-PAC 2.3, 2.4) and
// returns the first buffer of each type, in the order they appear. A PAC
// whose version is not 0, whose array does not fit, or with a buffer outside
// its bytes is refused before anything is allocated for it.
func pacBuffers(b []byte) ([]pacBuffer, error) {
	if len(b) < 8 {
		return nil, errors.New("PAC shorter than its header")
	}
	n := binary.LittleEndian.Uint32(b)
	if v := binary.LittleEndian.Uint32(b[4:]); v != 0 {
		return nil, fmt.Errorf("PAC version %d", v)
	}
	if uint64(n) > uint64(len(b)-8)/16 {
		return nil, fmt.Errorf("PAC of %d bytes claims %d buffers", len(b), n)
	}
	var out []pacBuffer
	for i := range int(n) {
		e := b[8+16*i:]
		typ, size, off := binary.LittleEndian.Uint32(e), binary.LittleEndian.Uint32(e[4:]), binary.LittleEndian.Uint64(e[8:])
		if off > uint64(len(b)) || uint64(size) > uint64(len(b))-off {
			return nil, fmt.Errorf("PAC buffer %d of type %d lies outside the PAC", i, typ)
		}
		// MS-PAC 2.4: of a type that appears more than once, only the first
		// is used.
		if !containsType(out, typ) {
			out = append(out, pacBuffer{typ: typ, off: int(off), data: b[off : off+uint64(size)]})
		}
	}
	return out, nil
}

func containsType(bs []pacBuffer, typ uint32) bool {
	for _, x := range bs {
		if x.typ == typ {
			return true
		}
	}
	return false
}

func bufferOf(bs []pacBuffer, typ uint32) (pacBuffer, bool) {
	for _, x := range bs {
		if x.typ == typ {
			return x, true
		}
	}
	return pacBuffer{}, false
}

// signatureLen is the value's length of each checksum type a KDC signature
// may have, so its value can be zeroed (MS-PAC 2.8).
var signatureLen = map[int32]int{
	chksumtype.HMAC_SHA1_96_AES128:    12,
	chksumtype.HMAC_SHA1_96_AES256:    12,
	chksumtype.KERB_CHECKSUM_HMAC_MD5: 16,
}

// verifyPAC verifies raw, a PAC's bytes as its ticket carries them, under
// key, the service key that decrypted the ticket, for the ticket's client
// cname and authentication time, and returns its logon information: nil when
// it holds none, which is read as no PAC. A PAC that does not verify is
// refused; one whose client information differs from the ticket's is
// refused errPACClient.
//
// decision: the PAC's buffers are walked here, never by gokrb5's
// PACType.Unmarshal and ProcessPACInfoBuffers, which allocate by the buffer
// count unchecked, refuse a PAC with no logon information and do not pin the
// checksum type. The logon information is decoded by gokrb5 only once the
// server signature verifies: its NDR decoder allocates by the counts it
// reads, unbounded, so only a holder of the service key, the KDC, reaches it.
// Overturned if a PAC is ever read before its signature verifies, or the KDC
// is not trusted with the decoder's allocations; then a bounded
// logon-information decoder replaces gokrb5's.
func verifyPAC(raw []byte, key types.EncryptionKey, cname types.PrincipalName, authTime time.Time) (*pac.KerbValidationInfo, error) {
	bufs, err := pacBuffers(raw)
	if err != nil {
		return nil, err
	}
	srv, ok := bufferOf(bufs, pacServerSignature)
	if !ok {
		return nil, errors.New("PAC without a server signature")
	}
	kdc, ok := bufferOf(bufs, pacKDCSignature)
	if !ok {
		return nil, errors.New("PAC without a KDC signature")
	}
	et, err := crypto.GetEtype(key.KeyType)
	if err != nil {
		return nil, err
	}
	// The server signature is the keyed checksum of the key's own type with
	// its 12-byte value (RFC 18 §3.4), never what the PAC claims.
	if len(srv.data) != 16 || int32(binary.LittleEndian.Uint32(srv.data)) != et.GetHashID() || !aesEtype(key.KeyType) {
		return nil, errors.New("PAC server signature of another type or length than the service key's")
	}
	if len(kdc.data) < 4 {
		return nil, errors.New("PAC KDC signature shorter than its type")
	}
	kdcLen, ok := signatureLen[int32(binary.LittleEndian.Uint32(kdc.data))]
	if !ok || len(kdc.data) < 4+kdcLen {
		return nil, errors.New("PAC KDC signature of an unknown type or length")
	}
	// decision: only the server signature is verified; the KDC signature is
	// not sent to a domain controller. A forged PAC needs the service key,
	// which the keytab's seal protects. Overturned if the keytab is ever
	// readable without its wrapping key, or a deployment requires
	// KDC-signature checks (RFC 18 §3.4).
	zeroed := bytes.Clone(raw)
	clear(zeroed[srv.off+4 : srv.off+16])
	clear(zeroed[kdc.off+4 : kdc.off+4+kdcLen])
	sum, err := et.GetChecksumHash(key.KeyValue, zeroed, keyusage.KERB_NON_KERB_CKSUM_SALT)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal(sum, srv.data[4:]) {
		return nil, errors.New("PAC server signature does not verify")
	}
	ci, ok := bufferOf(bufs, pacClientInfo)
	if !ok {
		return nil, errors.New("PAC without client information")
	}
	name, at, err := clientInfo(ci.data)
	if err != nil {
		return nil, err
	}
	if name != cname.PrincipalNameString() || at != filetime(authTime) {
		return nil, errPACClient
	}
	li, ok := bufferOf(bufs, pacLogonInfo)
	if !ok {
		return nil, nil
	}
	var v pac.KerbValidationInfo
	if err := v.Unmarshal(li.data); err != nil {
		return nil, err
	}
	return &v, nil
}

// clientInfo decodes PAC_CLIENT_INFO (MS-PAC 2.7): the client's name and its
// time, as a FILETIME.
func clientInfo(b []byte) (string, uint64, error) {
	if len(b) < 10 {
		return "", 0, errors.New("PAC client information shorter than its header")
	}
	n := int(binary.LittleEndian.Uint16(b[8:]))
	if n%2 != 0 || n > len(b)-10 {
		return "", 0, errors.New("PAC client name outside its buffer")
	}
	u := make([]uint16, n/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[10+2*i:])
	}
	return string(utf16.Decode(u)), binary.LittleEndian.Uint64(b), nil
}

// filetime is t in 100-nanosecond intervals since 1601 (MS-DTYP 2.3.3).
func filetime(t time.Time) uint64 { return uint64(t.Unix())*1e7 + 116444736000000000 }
