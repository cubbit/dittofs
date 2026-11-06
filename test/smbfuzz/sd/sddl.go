package sd

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Resolver maps trustee names to SIDs and back. Well-known trustees use
// their SDDL aliases; domain accounts use their account names; a SID with no
// name is written in a normalised form that does not depend on which
// machine minted it (see Name).
type Resolver struct {
	Domain   SID            // the domain SID, S-1-5-21-a-b-c
	Accounts map[string]SID // domain accounts by name: alice, devs, ...
}

var wellKnown = map[string]string{
	"WD":        "S-1-1-0",
	"AU":        "S-1-5-11",
	"CO":        "S-1-3-0",
	"CG":        "S-1-3-1",
	"OW":        "S-1-3-4",
	"BA":        "S-1-5-32-544",
	"BU":        "S-1-5-32-545",
	"SY":        "S-1-5-18",
	"LW":        "S-1-16-4096",
	"ME":        "S-1-16-8192",
	"HI":        "S-1-16-12288",
	"SI":        "S-1-16-16384",
	"unix-1000": "S-1-22-1-1000",
}

// domainRIDs are trustees named by their RID in the domain.
var domainRIDs = map[string]uint32{
	"DA":    512,
	"DU":    513,
	"ghost": 99999,
}

// SID resolves a trustee name. A literal "S-1-..." string is accepted too.
func (r *Resolver) SID(name string) (SID, error) {
	if strings.HasPrefix(name, "S-1-") {
		return ParseSID(name)
	}
	if s, ok := wellKnown[name]; ok {
		return MustSID(s), nil
	}
	if rid, ok := domainRIDs[name]; ok {
		return r.domainSID(rid), nil
	}
	if s, ok := r.Accounts[name]; ok {
		return s, nil
	}
	for prefix, mk := range map[string]func(uint32) SID{
		"dom-":     r.domainSID,
		"unix-":    func(v uint32) SID { return SID{Authority: 22, Sub: []uint32{1, v}} },
		"unixgrp-": func(v uint32) SID { return SID{Authority: 22, Sub: []uint32{2, v}} },
	} {
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			if v, err := strconv.ParseUint(rest, 10, 32); err == nil {
				return mk(uint32(v)), nil
			}
		}
	}
	return SID{}, fmt.Errorf("sd: unknown trustee %q", name)
}

func (r *Resolver) domainSID(rid uint32) SID {
	sub := append(append([]uint32(nil), r.Domain.Sub...), rid)
	return SID{Authority: r.Domain.Authority, Sub: sub}
}

// Name returns the symbolic name of a SID. SIDs with no name are normalised:
// a domain SID with an unnamed RID is "dom-<rid>"; a SID of another
// S-1-5-21 authority (a server's own machine SID) is "local-<rid>", so two
// servers' local accounts compare by role, not by machine; Samba's Unix SIDs
// are "unix-<uid>" and "unixgrp-<gid>". Anything else is its string form.
func (r *Resolver) Name(s SID) string {
	str := s.String()
	for n, v := range wellKnown {
		if v == str {
			return n
		}
	}
	names := make([]string, 0, len(r.Accounts))
	for n := range r.Accounts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if r.Accounts[n].Equal(s) {
			return n
		}
	}
	if len(r.Domain.Sub) > 0 && s.HasPrefix(r.Domain) && len(s.Sub) == len(r.Domain.Sub)+1 {
		rid := s.Sub[len(s.Sub)-1]
		for n, v := range domainRIDs {
			if v == rid {
				return n
			}
		}
		return fmt.Sprintf("dom-%d", rid)
	}
	if s.Authority == 5 && len(s.Sub) == 5 && s.Sub[0] == 21 {
		return fmt.Sprintf("local-%d", s.Sub[4])
	}
	if s.Authority == 22 && len(s.Sub) == 2 {
		if s.Sub[0] == 1 {
			return fmt.Sprintf("unix-%d", s.Sub[1])
		}
		if s.Sub[0] == 2 {
			return fmt.Sprintf("unixgrp-%d", s.Sub[1])
		}
	}
	return str
}

var aceTypeNames = map[byte]string{AccessAllowed: "A", AccessDenied: "D", SystemAudit: "AU", MandatoryLabel: "ML"}

var aceFlagNames = []struct {
	bit  byte
	name string
}{
	{ObjectInherit, "OI"}, {ContainerInherit, "CI"}, {NoPropagateInherit, "NP"},
	{InheritOnly, "IO"}, {Inherited, "ID"}, {SuccessfulAccess, "SA"}, {FailedAccess, "FA"},
}

var maskNames = []struct {
	mask uint32
	name string
}{
	{0x001F01FF, "FA"}, {0x00120089, "FR"}, {0x00120116, "FW"}, {0x001200A0, "FX"},
	{0x10000000, "GA"}, {0x80000000, "GR"}, {0x40000000, "GW"}, {0x20000000, "GX"},
}

func formatFlags(f byte) string {
	var b strings.Builder
	rest := f
	for _, n := range aceFlagNames {
		if f&n.bit != 0 {
			b.WriteString(n.name)
			rest &^= n.bit
		}
	}
	if rest != 0 {
		return fmt.Sprintf("0x%02x", f)
	}
	return b.String()
}

func formatMask(m uint32) string {
	for _, n := range maskNames {
		if n.mask == m {
			return n.name
		}
	}
	return fmt.Sprintf("0x%x", m)
}

func formatACLFlags(ctl uint16, protected, autoInherited, req uint16) string {
	var s string
	if ctl&protected != 0 {
		s += "P"
	}
	if ctl&req != 0 {
		s += "AR"
	}
	if ctl&autoInherited != 0 {
		s += "AI"
	}
	return s
}

// Format writes the descriptor as SDDL with symbolic trustees. Only the
// control flags that SDDL expresses (P, AR, AI per list, and a null DACL)
// appear; the rest are not part of the text.
func (r *Resolver) Format(d *SD) string {
	var b strings.Builder
	if d.Owner != nil {
		b.WriteString("O:" + r.Name(*d.Owner))
	}
	if d.Group != nil {
		b.WriteString("G:" + r.Name(*d.Group))
	}
	if d.DACL != nil || d.NullDACL() {
		b.WriteString("D:" + formatACLFlags(d.Control, DACLProtected, DACLAutoInherited, DACLAutoInheritReq))
		if d.DACL == nil {
			b.WriteString("NO_ACCESS_CONTROL")
		} else {
			r.formatACEs(&b, d.DACL)
		}
	}
	if d.SACL != nil {
		b.WriteString("S:" + formatACLFlags(d.Control, SACLProtected, SACLAutoInherited, SACLAutoInheritReq))
		r.formatACEs(&b, d.SACL)
	}
	return b.String()
}

func (r *Resolver) formatACEs(b *strings.Builder, l *ACL) {
	for _, a := range l.ACEs {
		if !a.modelled() {
			fmt.Fprintf(b, "(0x%02x;%s;raw:%s;;;)", a.Type, formatFlags(a.Flags), hex.EncodeToString(a.Raw))
			continue
		}
		fmt.Fprintf(b, "(%s;%s;%s;;;%s)", aceTypeNames[a.Type], formatFlags(a.Flags), formatMask(a.Mask), r.Name(a.SID))
	}
}

// Parse reads the SDDL text Format writes. Literal SIDs, hex masks and hex
// flags are accepted wherever a name is. Sections are found by their
// "O:", "G:", "D:" and "S:" markers, which no trustee name contains.
func (r *Resolver) Parse(text string) (*SD, error) {
	text = strings.TrimSpace(text)
	d := &SD{}
	if text == "" {
		return d, nil
	}
	if len(text) < 2 || text[1] != ':' || !strings.ContainsRune("OGDS", rune(text[0])) {
		return nil, fmt.Errorf("sd: %q does not start with a section", text)
	}
	for text != "" {
		kind := text[0]
		text = text[2:]
		end := len(text)
		if kind == 'O' || kind == 'G' {
			for _, m := range []string{"G:", "D:", "S:"} {
				if i := strings.Index(text, m); i >= 0 && i < end {
					end = i
				}
			}
		} else {
			// An ACL section ends at the next marker outside parentheses.
			depth := 0
			for i := 0; i < len(text); i++ {
				switch text[i] {
				case '(':
					depth++
				case ')':
					depth--
				}
				if depth == 0 && i+1 < len(text) && text[i+1] == ':' && strings.ContainsRune("OGDS", rune(text[i])) &&
					(i == 0 || text[i-1] == ')' || strings.HasSuffix(text[:i], "NO_ACCESS_CONTROL") || isACLFlags(text[:i])) {
					end = i
					break
				}
			}
		}
		body := text[:end]
		text = text[end:]
		switch kind {
		case 'O', 'G':
			sid, err := r.SID(strings.TrimSpace(body))
			if err != nil {
				return nil, err
			}
			if kind == 'O' {
				d.Owner = &sid
			} else {
				d.Group = &sid
			}
		case 'D', 'S':
			ctl, acl, null, err := r.parseACL(body, kind == 'D')
			if err != nil {
				return nil, err
			}
			d.Control |= ctl
			if kind == 'D' {
				if null {
					d.Control |= DACLPresent
				} else {
					d.DACL = acl
				}
			} else {
				d.SACL = acl
			}
		default:
			return nil, fmt.Errorf("sd: unknown section %q", kind)
		}
	}
	return d, nil
}

// isACLFlags reports whether s is only ACL control flags (P, AI, AR), the
// whole body of an ACL section with no entries.
func isACLFlags(s string) bool {
	s = strings.NewReplacer("AI", "", "AR", "", "P", "").Replace(s)
	return s == ""
}

func (r *Resolver) parseACL(body string, dacl bool) (uint16, *ACL, bool, error) {
	body = strings.TrimSpace(body)
	var ctl uint16
	p, ai, ar := SACLProtected, SACLAutoInherited, SACLAutoInheritReq
	if dacl {
		p, ai, ar = DACLProtected, DACLAutoInherited, DACLAutoInheritReq
	}
	for {
		switch {
		case strings.HasPrefix(body, "P"):
			ctl |= p
			body = body[1:]
			continue
		case strings.HasPrefix(body, "AI"):
			ctl |= ai
			body = body[2:]
			continue
		case strings.HasPrefix(body, "AR"):
			ctl |= ar
			body = body[2:]
			continue
		}
		break
	}
	if body == "NO_ACCESS_CONTROL" {
		return ctl, nil, true, nil
	}
	acl := &ACL{}
	for body != "" {
		if body[0] != '(' {
			return 0, nil, false, fmt.Errorf("sd: expected '(' at %q", body)
		}
		end := strings.IndexByte(body, ')')
		if end < 0 {
			return 0, nil, false, fmt.Errorf("sd: unterminated ACE %q", body)
		}
		a, err := r.parseACE(body[1:end])
		if err != nil {
			return 0, nil, false, err
		}
		acl.ACEs = append(acl.ACEs, a)
		body = strings.TrimSpace(body[end+1:])
	}
	return ctl, acl, false, nil
}

func (r *Resolver) parseACE(s string) (ACE, error) {
	f := strings.Split(s, ";")
	if len(f) != 6 {
		return ACE{}, fmt.Errorf("sd: ACE %q needs 6 fields", s)
	}
	var a ACE
	switch f[0] {
	case "A":
		a.Type = AccessAllowed
	case "D":
		a.Type = AccessDenied
	case "AU":
		a.Type = SystemAudit
	case "ML":
		a.Type = MandatoryLabel
	default:
		v, err := strconv.ParseUint(f[0], 0, 8)
		if err != nil {
			return ACE{}, fmt.Errorf("sd: ACE type %q", f[0])
		}
		a.Type = byte(v)
	}
	flags, err := parseFlags(f[1])
	if err != nil {
		return ACE{}, err
	}
	a.Flags = flags
	if !a.modelled() {
		raw, ok := strings.CutPrefix(f[2], "raw:")
		if !ok {
			return ACE{}, fmt.Errorf("sd: unmodelled ACE %q needs raw:", s)
		}
		a.Raw, err = hex.DecodeString(raw)
		return a, err
	}
	a.Mask, err = parseMask(f[2])
	if err != nil {
		return ACE{}, err
	}
	a.SID, err = r.SID(f[5])
	return a, err
}

func parseFlags(s string) (byte, error) {
	if strings.HasPrefix(s, "0x") {
		v, err := strconv.ParseUint(s, 0, 8)
		return byte(v), err
	}
	var f byte
	for len(s) > 0 {
		if len(s) < 2 {
			return 0, fmt.Errorf("sd: ACE flags %q", s)
		}
		found := false
		for _, n := range aceFlagNames {
			if s[:2] == n.name {
				f |= n.bit
				found = true
				break
			}
		}
		if !found {
			return 0, fmt.Errorf("sd: ACE flag %q", s[:2])
		}
		s = s[2:]
	}
	return f, nil
}

func parseMask(s string) (uint32, error) {
	for _, n := range maskNames {
		if n.name == s {
			return n.mask, nil
		}
	}
	v, err := strconv.ParseUint(s, 0, 32)
	if err != nil {
		return 0, fmt.Errorf("sd: mask %q", s)
	}
	return uint32(v), nil
}
