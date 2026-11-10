// Package sd is the fuzzer's own codec for Windows security descriptors: the
// self-relative binary form a client sends and receives (MS-DTYP 2.4.6), and a
// text form, a subset of SDDL, that names trustees symbolically so a plan does
// not depend on the domain SID of one provisioning.
//
// It is deliberately independent of DittoFS's own descriptor code: the fuzzer
// judges DittoFS, so it must not share a parser with it, or a bug in that
// parser would be invisible on both sides of the comparison.
package sd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Control flags of a security descriptor (MS-DTYP 2.4.6).
const (
	OwnerDefaulted     uint16 = 0x0001
	GroupDefaulted     uint16 = 0x0002
	DACLPresent        uint16 = 0x0004
	DACLDefaulted      uint16 = 0x0008
	SACLPresent        uint16 = 0x0010
	SACLDefaulted      uint16 = 0x0020
	DACLAutoInheritReq uint16 = 0x0100
	SACLAutoInheritReq uint16 = 0x0200
	DACLAutoInherited  uint16 = 0x0400
	SACLAutoInherited  uint16 = 0x0800
	DACLProtected      uint16 = 0x1000
	SACLProtected      uint16 = 0x2000
	RMControlValid     uint16 = 0x4000
	SelfRelative       uint16 = 0x8000
)

// ACE types used by the fuzzer. Other types are kept as raw bytes.
const (
	AccessAllowed  byte = 0x00
	AccessDenied   byte = 0x01
	SystemAudit    byte = 0x02
	MandatoryLabel byte = 0x11
)

// ACE flags.
const (
	ObjectInherit      byte = 0x01
	ContainerInherit   byte = 0x02
	NoPropagateInherit byte = 0x04
	InheritOnly        byte = 0x08
	Inherited          byte = 0x10
	SuccessfulAccess   byte = 0x40
	FailedAccess       byte = 0x80
)

// SID is a security identifier.
type SID struct {
	Authority uint64 // 48 bits
	Sub       []uint32
}

// ParseSID parses the "S-1-5-21-..." string form.
func ParseSID(s string) (SID, error) {
	parts := strings.Split(s, "-")
	if len(parts) < 3 || parts[0] != "S" || parts[1] != "1" {
		return SID{}, fmt.Errorf("sd: bad SID %q", s)
	}
	auth, err := strconv.ParseUint(parts[2], 10, 48)
	if err != nil {
		return SID{}, fmt.Errorf("sd: bad SID authority in %q", s)
	}
	sid := SID{Authority: auth}
	for _, p := range parts[3:] {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return SID{}, fmt.Errorf("sd: bad sub-authority in %q", s)
		}
		sid.Sub = append(sid.Sub, uint32(v))
	}
	if len(sid.Sub) > 15 {
		return SID{}, fmt.Errorf("sd: too many sub-authorities in %q", s)
	}
	return sid, nil
}

// MustSID parses s and panics on error; for constants.
func MustSID(s string) SID {
	sid, err := ParseSID(s)
	if err != nil {
		panic(err)
	}
	return sid
}

func (s SID) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "S-1-%d", s.Authority)
	for _, v := range s.Sub {
		fmt.Fprintf(&b, "-%d", v)
	}
	return b.String()
}

// Equal reports whether two SIDs are the same.
func (s SID) Equal(o SID) bool {
	if s.Authority != o.Authority || len(s.Sub) != len(o.Sub) {
		return false
	}
	for i := range s.Sub {
		if s.Sub[i] != o.Sub[i] {
			return false
		}
	}
	return true
}

// HasPrefix reports whether s starts with every sub-authority of p.
func (s SID) HasPrefix(p SID) bool {
	if s.Authority != p.Authority || len(s.Sub) < len(p.Sub) {
		return false
	}
	for i := range p.Sub {
		if s.Sub[i] != p.Sub[i] {
			return false
		}
	}
	return true
}

func (s SID) size() int { return 8 + 4*len(s.Sub) }

func (s SID) marshal(b []byte) {
	b[0] = 1
	b[1] = byte(len(s.Sub))
	for i := 0; i < 6; i++ {
		b[2+i] = byte(s.Authority >> (8 * (5 - i)))
	}
	for i, v := range s.Sub {
		binary.LittleEndian.PutUint32(b[8+4*i:], v)
	}
}

func unmarshalSID(b []byte) (SID, int, error) {
	if len(b) < 8 {
		return SID{}, 0, errors.New("sd: short SID")
	}
	n := int(b[1])
	if n > 15 || len(b) < 8+4*n {
		return SID{}, 0, errors.New("sd: bad SID length")
	}
	var sid SID
	for i := 0; i < 6; i++ {
		sid.Authority = sid.Authority<<8 | uint64(b[2+i])
	}
	for i := 0; i < n; i++ {
		sid.Sub = append(sid.Sub, binary.LittleEndian.Uint32(b[8+4*i:]))
	}
	return sid, 8 + 4*n, nil
}

// ACE is one access control entry. An ACE whose type the codec does not
// model keeps its body in Raw and is written back byte for byte.
type ACE struct {
	Type  byte
	Flags byte
	Mask  uint32
	SID   SID
	Raw   []byte // body after the 4-byte header, for unmodelled types
}

func (a ACE) modelled() bool {
	switch a.Type {
	case AccessAllowed, AccessDenied, SystemAudit, MandatoryLabel:
		return true
	}
	return false
}

func (a ACE) size() int {
	if !a.modelled() {
		return 4 + len(a.Raw)
	}
	return 8 + a.SID.size()
}

// ACL is a list of ACEs.
type ACL struct {
	Revision byte // 2, or 4 for object ACEs; 0 means 2
	ACEs     []ACE
}

func (l *ACL) size() int {
	n := 8
	for _, a := range l.ACEs {
		n += a.size()
	}
	return n
}

func (l *ACL) marshal(b []byte) {
	rev := l.Revision
	if rev == 0 {
		rev = 2
	}
	b[0] = rev
	binary.LittleEndian.PutUint16(b[2:], uint16(l.size()))
	binary.LittleEndian.PutUint16(b[4:], uint16(len(l.ACEs)))
	off := 8
	for _, a := range l.ACEs {
		sz := a.size()
		b[off] = a.Type
		b[off+1] = a.Flags
		binary.LittleEndian.PutUint16(b[off+2:], uint16(sz))
		if a.modelled() {
			binary.LittleEndian.PutUint32(b[off+4:], a.Mask)
			a.SID.marshal(b[off+8:])
		} else {
			copy(b[off+4:], a.Raw)
		}
		off += sz
	}
}

func unmarshalACL(b []byte) (*ACL, error) {
	if len(b) < 8 {
		return nil, errors.New("sd: short ACL")
	}
	size := int(binary.LittleEndian.Uint16(b[2:]))
	count := int(binary.LittleEndian.Uint16(b[4:]))
	if size < 8 || size > len(b) {
		return nil, errors.New("sd: bad ACL size")
	}
	l := &ACL{Revision: b[0]}
	off := 8
	for i := 0; i < count; i++ {
		if off+4 > size {
			return nil, errors.New("sd: ACE past ACL end")
		}
		a := ACE{Type: b[off], Flags: b[off+1]}
		sz := int(binary.LittleEndian.Uint16(b[off+2:]))
		if sz < 4 || off+sz > size {
			return nil, errors.New("sd: bad ACE size")
		}
		body := b[off+4 : off+sz]
		if a.modelled() {
			if len(body) < 4 {
				return nil, errors.New("sd: short ACE")
			}
			a.Mask = binary.LittleEndian.Uint32(body)
			sid, _, err := unmarshalSID(body[4:])
			if err != nil {
				return nil, err
			}
			a.SID = sid
		} else {
			a.Raw = append([]byte(nil), body...)
		}
		l.ACEs = append(l.ACEs, a)
		off += sz
	}
	return l, nil
}

// SD is a security descriptor. A nil DACL with DACLPresent set is a null
// DACL (everyone is granted everything); a nil DACL without it is absent.
type SD struct {
	Control uint16
	Owner   *SID
	Group   *SID
	DACL    *ACL
	SACL    *ACL
}

// Marshal returns the self-relative binary form. DACLPresent and SACLPresent
// are set from the ACLs present, keeping a null DACL when the caller set
// DACLPresent with no DACL.
func (d *SD) Marshal() []byte {
	ctl := d.Control | SelfRelative
	if d.DACL != nil {
		ctl |= DACLPresent
	}
	if d.SACL != nil {
		ctl |= SACLPresent
	}
	n := 20
	var offOwner, offGroup, offSACL, offDACL int
	if d.SACL != nil {
		offSACL = n
		n += d.SACL.size()
	}
	if d.DACL != nil {
		offDACL = n
		n += d.DACL.size()
	}
	if d.Owner != nil {
		offOwner = n
		n += d.Owner.size()
	}
	if d.Group != nil {
		offGroup = n
		n += d.Group.size()
	}
	b := make([]byte, n)
	b[0] = 1
	binary.LittleEndian.PutUint16(b[2:], ctl)
	binary.LittleEndian.PutUint32(b[4:], uint32(offOwner))
	binary.LittleEndian.PutUint32(b[8:], uint32(offGroup))
	binary.LittleEndian.PutUint32(b[12:], uint32(offSACL))
	binary.LittleEndian.PutUint32(b[16:], uint32(offDACL))
	if d.SACL != nil {
		d.SACL.marshal(b[offSACL:])
	}
	if d.DACL != nil {
		d.DACL.marshal(b[offDACL:])
	}
	if d.Owner != nil {
		d.Owner.marshal(b[offOwner:])
	}
	if d.Group != nil {
		d.Group.marshal(b[offGroup:])
	}
	return b
}

// Unmarshal parses a self-relative security descriptor.
func Unmarshal(b []byte) (*SD, error) {
	if len(b) < 20 {
		return nil, errors.New("sd: short descriptor")
	}
	if b[0] != 1 {
		return nil, fmt.Errorf("sd: revision %d", b[0])
	}
	d := &SD{Control: binary.LittleEndian.Uint16(b[2:])}
	at := func(i int) (int, error) {
		o := int(binary.LittleEndian.Uint32(b[i:]))
		if o != 0 && (o < 20 || o >= len(b)) {
			return 0, fmt.Errorf("sd: offset %d out of range", o)
		}
		return o, nil
	}
	if o, err := at(4); err != nil {
		return nil, err
	} else if o != 0 {
		sid, _, err := unmarshalSID(b[o:])
		if err != nil {
			return nil, err
		}
		d.Owner = &sid
	}
	if o, err := at(8); err != nil {
		return nil, err
	} else if o != 0 {
		sid, _, err := unmarshalSID(b[o:])
		if err != nil {
			return nil, err
		}
		d.Group = &sid
	}
	if o, err := at(12); err != nil {
		return nil, err
	} else if o != 0 && d.Control&SACLPresent != 0 {
		l, err := unmarshalACL(b[o:])
		if err != nil {
			return nil, err
		}
		d.SACL = l
	}
	if o, err := at(16); err != nil {
		return nil, err
	} else if o != 0 && d.Control&DACLPresent != 0 {
		l, err := unmarshalACL(b[o:])
		if err != nil {
			return nil, err
		}
		d.DACL = l
	}
	return d, nil
}

// NullDACL reports whether the descriptor carries a null DACL.
func (d *SD) NullDACL() bool { return d.Control&DACLPresent != 0 && d.DACL == nil }
