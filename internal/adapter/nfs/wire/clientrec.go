package wire

//go:generate go run ./gen -o clientrec_gen.go clientrec.go

import (
	"bytes"
	"errors"
	"fmt"
)

// The Adapter bytes are the NFS adapter's own state in a client's record: one
// version byte, then an XDR union (RFC 4506 section 4.15) on the record's kind.
// The layout extends the list of RFC 21 section 11.4 with the host's address section
// 6.2 sends the notice to and the address it leaves from; section 11.7 names the three refusals.
//
// decision: a new field is a new version, which every node must read before
// any node writes it, since a node that read a newer version as its own would
// answer a client from state it misunderstands. A record that does not decode
// is refused, never rewritten, so an older node cannot overwrite a newer
// node's state. Overturned if the decode-failure series shows a client locked
// out in practice.
const ClientRecVersion = 1

// The kinds of a ClientRec, which are the record's own protocol bytes
// (NFSv4 2, NLM 3): wire cannot import the package that names them, and the
// xdr tags below must repeat the numbers, so a change here is a change there.
const (
	ClientRecKindNFS4 = 2
	ClientRecKindNLM  = 3
)

// ClientRec is the Adapter bytes' value. Kind selects the arm, and a reader
// checks Kind before it reads an arm: the other arm is the zero value, which
// reads as a valid empty record, not as a refusal.
type ClientRec struct {
	Kind uint32        `xdr:"union"`
	NFS4 ClientRecNFS4 `xdr:"case=2"`
	NLM  ClientRecNLM  `xdr:"case=3"`
}

// ClientRecNFS4 is an NFSv4 client's state.
type ClientRecNFS4 struct {
	Minor        uint32            // 0 or 1: the minor version that confirmed the record
	SessionSeq   uint32            // the last csa_sequence applied
	SessionReply []byte            `xdr:"max=256"` // its CREATE_SESSION4res only
	MustEnforce  []uint32          `xdr:"max=3"`   // spo_must_enforce, as sent
	MustAllow    []uint32          `xdr:"max=3"`   // spo_must_allow, as sent
	Callback     *CBClient4        // NFSv4.0: cb_client4 and callback_ident
	Confirm      *ClientRecConfirm // NFSv4.0: the adapter's confirm, for a repeated confirm
}

// CBClient4 is an NFSv4.0 client's callback: cb_client4 and callback_ident.
type CBClient4 struct {
	Program uint32
	NetID   string `xdr:"max=16"`
	Addr    string `xdr:"max=64"`
	Ident   uint32
}

// ClientRecConfirm is the confirm verifier the adapter drew for an NFSv4.0
// client: a struct, since the generator's optional data is a struct's.
type ClientRecConfirm struct {
	Verifier [8]byte
}

// ClientRecNLM is an NLM host's state: where SM_NOTIFY goes, and from where.
type ClientRecNLM struct {
	MonName string `xdr:"max=1024"`
	Priv    [16]byte
	Host    []byte `xdr:"max=16"` // 4 or 16 bytes
	Server  []byte `xdr:"max=16"` // 4 or 16 bytes
}

// The three reasons a record does not decode (RFC 21 section 11.7).
var (
	ErrRecTruncated = errors.New("nfs/wire: client record truncated")
	ErrRecCorrupt   = errors.New("nfs/wire: client record corrupt")
	ErrRecVersion   = errors.New("nfs/wire: client record of an unknown version")
)

// AppendClientRec appends r's bytes to dst and returns them, or dst as it came
// and ErrRecCorrupt for a value DecodeClientRec would refuse, so no node writes
// a record no node reads.
func AppendClientRec(dst []byte, r *ClientRec) ([]byte, error) {
	if r == nil {
		return dst, fmt.Errorf("%w: no record", ErrRecCorrupt)
	}

	if err := checkFields(r); err != nil {
		return dst, err
	}

	e := Encoder{B: append(dst, ClientRecVersion)}
	r.encodeXDR(&e, nil, 0)

	if e.Err != nil {
		return dst, fmt.Errorf("%w: %v", ErrRecCorrupt, e.Err)
	}

	return e.B, nil
}

// DecodeClientRec reads the Adapter bytes in b. The value owns its bytes: the
// runtime's opaques are slices of b, which a rewrite of the record would
// overwrite.
func DecodeClientRec(b []byte) (ClientRec, error) {
	var r ClientRec
	if len(b) == 0 {
		return r, fmt.Errorf("%w: no version byte", ErrRecTruncated)
	}

	if b[0] != ClientRecVersion {
		return r, fmt.Errorf("%w: %d", ErrRecVersion, b[0])
	}

	d := Decoder{B: b[1:]}
	r.decodeXDR(&d, 0)

	switch {
	case errors.Is(d.Err, errPastRecord):
		return ClientRec{}, fmt.Errorf("%w: %v", ErrRecTruncated, d.Err)
	case d.Err != nil:
		return ClientRec{}, fmt.Errorf("%w: %v", ErrRecCorrupt, d.Err)
	case d.N != len(d.B):
		return ClientRec{}, fmt.Errorf("%w: %d bytes left over", ErrRecCorrupt, len(d.B)-d.N)
	}

	if err := checkFields(&r); err != nil {
		return ClientRec{}, err
	}

	r.NFS4.SessionReply = own(r.NFS4.SessionReply)
	r.NLM.Host = own(r.NLM.Host)
	r.NLM.Server = own(r.NLM.Server)

	return r, nil
}

// checkFields refuses what the generated codec's bounds allow and the layout
// does not: an NFSv4 minor version other than 0 or 1, the two the record can
// have been confirmed at, and an NLM address of another length than 4 or 16.
func checkFields(r *ClientRec) error {
	if r.Kind == ClientRecKindNFS4 && r.NFS4.Minor > 1 {
		return fmt.Errorf("%w: a minor version of %d", ErrRecCorrupt, r.NFS4.Minor)
	}

	return checkAddrs(r)
}

// checkAddrs refuses an NLM record whose Host or Server is neither an IPv4 nor
// an IPv6 address's length, which the generated codec's bound of 16 allows.
func checkAddrs(r *ClientRec) error {
	n := &r.NLM
	if r.Kind != ClientRecKindNLM || isAddr(n.Host) && isAddr(n.Server) {
		return nil
	}

	return fmt.Errorf(
		"%w: an address of %d and %d bytes", ErrRecCorrupt, len(n.Host), len(n.Server),
	)
}

func isAddr(a []byte) bool {
	return len(a) == 4 || len(a) == 16
}

// own copies b, and gives an empty one as nil.
func own(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}

	return bytes.Clone(b)
}
