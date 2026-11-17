package wire

import (
	"encoding/binary"
	"fmt"
)

// AuthSysParms is RFC 5531 Appendix A's authsys_parms: its machine name and
// group list in fixed arrays, so decoding one allocates nothing.
type AuthSysParms struct {
	Stamp   uint32
	Machine [255]byte
	NameLen int
	UID     uint32
	GID     uint32
	GIDs    [16]uint32
	NGIDs   int
}

// MachineName is the machine name's bytes.
func (a AuthSysParms) MachineName() []byte { return a.Machine[:a.NameLen] }

// Groups is the credential's group list.
func (a AuthSysParms) Groups() []uint32 { return a.GIDs[:a.NGIDs] }

// DecodeAuthSys reads an AUTH_SYS credential's body: the stamp, the machine
// name, the uid and gid, and the group list (RFC 5531 section 8.2, Appendix A).
// It refuses a machine name over 255 bytes, more than 16 groups, a body cut
// before any of their bytes, and one with bytes left after the group list: the
// body is one authsys_parms, so bytes left after it mean its length and its
// contents disagree.
func DecodeAuthSys(body []byte) (AuthSysParms, error) {
	var a AuthSysParms
	r := reader{b: body}
	stamp, ok := r.u32()
	if !ok {
		return a, fmt.Errorf("nfs/wire: %w: AUTH_SYS's stamp", errPastRecord)
	}
	a.Stamp = stamp
	n, ok := r.u32()
	if !ok {
		return a, fmt.Errorf("nfs/wire: %w: the machine name's length", errPastRecord)
	}
	if n > 255 {
		return a, fmt.Errorf("nfs/wire: an AUTH_SYS machine name of %d bytes, past 255", n)
	}
	pad := (4 - n%4) % 4
	if r.off+int(n)+int(pad) > len(r.b) {
		return a, fmt.Errorf("nfs/wire: %w: a machine name of %d bytes", errPastRecord, n)
	}
	a.NameLen = int(n)
	copy(a.Machine[:], r.b[r.off:r.off+int(n)])
	r.off += int(n) + int(pad)
	uid, ok := r.u32()
	if !ok {
		return a, fmt.Errorf("nfs/wire: %w: AUTH_SYS's uid", errPastRecord)
	}
	gid, ok := r.u32()
	if !ok {
		return a, fmt.Errorf("nfs/wire: %w: AUTH_SYS's gid", errPastRecord)
	}
	a.UID, a.GID = uid, gid
	ng, ok := r.u32()
	if !ok {
		return a, fmt.Errorf("nfs/wire: %w: the group list's length", errPastRecord)
	}
	if ng > 16 {
		return a, fmt.Errorf("nfs/wire: an AUTH_SYS group list of %d, past 16", ng)
	}
	if r.off+4*int(ng) > len(r.b) {
		return a, fmt.Errorf("nfs/wire: %w: a group list of %d", errPastRecord, ng)
	}
	a.NGIDs = int(ng)
	for i := range a.NGIDs {
		a.GIDs[i] = binary.BigEndian.Uint32(r.b[r.off:])
		r.off += 4
	}
	if r.off != len(r.b) {
		return a, fmt.Errorf("nfs/wire: %d bytes after the group list", len(r.b)-r.off)
	}
	return a, nil
}
