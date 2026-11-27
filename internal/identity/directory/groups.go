package directory

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/go-ldap/ldap/v3"
)

// BitAnd is LDAP_MATCHING_RULE_BIT_AND: (attr:<BitAnd>:=<n>) holds for an
// entry whose integer attr has every bit of n set.
const BitAnd = "1.2.840.113556.1.4.803"

// securityGroup matches a group whose groupType holds the security bit
// (MS-ADTS 2.2.12), the groups a security token, and so tokenGroups, holds.
const securityGroup = "(groupType:" + BitAnd + ":=2147483648)"

// WithRID is the SID domain with rid appended as one more sub-authority, as a
// domain's account is named; domain is a binary SID.
func WithRID(domain []byte, rid uint32) []byte {
	sid := binary.LittleEndian.AppendUint32(slices.Clone(domain), rid)
	sid[1]++ // the sub-authority count
	return sid
}

// pageSize is the page each RFC 2696 paged search asks for, Active
// Directory's default MaxPageSize.
const pageSize = 1000

// ponytail: fixed bounds on one question's queries. A paged search reads at
// most 64 pages (64 000 entries); a member list at most 64 ranges, which at
// Active Directory's MaxValRange of 1 500 is 96 000 values, so an all-staff
// group of 10^5 members fails; an OR filter carries at most 64 terms, the
// SIDs of one lookup or the groups of one rfc2307 level, so a PAC of 1 024
// SIDs costs 16 queries; an rfc2307 walk goes at most 64 levels deep and
// gathers at most 1 024 groups, RFC 18's cap on a principal's groups, as
// tokenGroups may hold at most 1 024 SIDs before any is looked up. Each
// query is under the 5-second timeout; the question as a whole is bounded
// only by the caller's context, which carries RFC 18's 20 seconds (§5.1).
// Size the OR filter from a measured filter cost, and the page from the
// directory's MaxPageSize, when a directory's answers show either.
const (
	maxPages  = 64
	maxRanges = 64
	maxLevels = 64
	maxGroups = 1024
	orTerms   = 64
)

// SIDBatch is the most SIDs one LookupSIDs query carries in its OR filter.
// It is exported so that a cache in front of the client can split a set into
// the same batches before the client does, and share or store each batch's
// answer as the one query the client sends for it.
const SIDBatch = orTerms

// LookupSIDs answers "who holds this SID" for each of sids, in batches of at
// most orTerms in one OR filter: the entries found, in the order of
// sids, a SID no entry holds left out. A SID two entries hold fails the
// whole lookup ErrUnavailable, as Lookup does.
func (c *Client) LookupSIDs(ctx context.Context, sids [][]byte) ([]Answer, error) {
	c.begin()
	defer c.end()
	base, _, err := c.domain(ctx)
	if err != nil {
		return nil, err
	}
	return c.lookupSIDs(ctx, base, sids)
}

// lookupSIDs is LookupSIDs under base.
func (c *Client) lookupSIDs(ctx context.Context, base string, sids [][]byte) ([]Answer, error) {
	var uniq [][]byte
	asked := map[string]bool{}
	for _, s := range sids {
		if len(s) > 0 && !asked[string(s)] {
			asked[string(s)] = true
			uniq = append(uniq, s)
		}
	}
	found := map[string]Answer{}
	for batch := range slices.Chunk(uniq, orTerms) {
		var f strings.Builder
		f.WriteString("(|")
		for _, s := range batch {
			f.WriteString("(objectSid=" + escapeBinary(s) + ")")
		}
		f.WriteString(")")
		_, res, err := c.do(ctx, Request{Base: base, Scope: ScopeSubtree, Filter: f.String(), Attrs: c.rec.attrs()})
		if err != nil {
			return nil, err
		}
		for _, e := range res.Entries {
			a := c.answer(e)
			if prev, ok := found[string(a.SID)]; ok {
				c.log.Warn("identity.directory: a SID held by more than one entry, refused",
					"first", prev.DN, "second", a.DN)
				return nil, unavailable("SID lookup", fmt.Errorf("%w: two entries hold one SID", ErrAmbiguous))
			}
			found[string(a.SID)] = a
		}
	}
	var out []Answer
	for _, s := range uniq {
		if a, ok := found[string(s)]; ok {
			out = append(out, a)
		}
	}
	return out, nil
}

// Groups answers "what are this principal's groups, nested" for the user
// Lookup answered: every group it belongs to, directly or through others,
// its primary group and that group's own groups included. The closure is
// returned whole or not at all: a query, a page or a range that fails fails
// it ErrUnavailable, and so does one of more than 1 024 groups.
//
// An ad user's closure is one read of its constructed tokenGroups, its SIDs
// then looked up in batches under the naming context, as tokenGroups covers
// the whole domain whatever the record's base; a group SID no entry holds is
// answered by its SID alone, so the closure keeps it. A bind that may not
// read tokenGroups gets it empty, and the closure is then walked level by
// level from the user and from the primary group primaryGroupID names, since
// member omits a user's primary group, following security groups only: the
// groups tokenGroups would answer (RFC 18 §2.5). An rfc2307 user's closure
// is walked level by level too: the groups naming it in memberUid or member,
// then the groups naming those in member.
func (c *Client) Groups(ctx context.Context, user Answer) ([]Answer, error) {
	c.begin()
	defer c.end()
	if !c.rec.IsAD() {
		conds := []string{"(member=" + ldap.EscapeFilter(user.DN) + ")"}
		if user.Name != "" {
			conds = append(conds, "(memberUid="+ldap.EscapeFilter(user.Name)+")")
		}
		return c.walk(ctx, c.rec.Base, "(objectClass=posixGroup)", nil, conds)
	}
	nc, domainSID, err := c.domain(ctx)
	if err != nil {
		return nil, err
	}
	_, res, err := c.do(ctx, Request{Base: user.DN, Scope: ScopeBase, Filter: "(objectClass=user)",
		Attrs: []string{"tokenGroups", "primaryGroupID"}})
	if err != nil {
		return nil, err
	}
	if len(res.Entries) != 1 {
		return nil, unavailable("groups", fmt.Errorf("the user %q answered %d entries", user.DN, len(res.Entries)))
	}
	if sids := values(res.Entries[0], "tokenGroups"); len(sids) > 0 {
		if len(sids) > maxGroups {
			return nil, unavailable("groups", fmt.Errorf("tokenGroups holds %d SIDs, more than %d", len(sids), maxGroups))
		}
		out, err := c.lookupSIDs(ctx, nc, sids)
		if err != nil {
			return nil, err
		}
		have := map[string]bool{}
		for _, a := range out {
			have[string(a.SID)] = true
		}
		for _, s := range sids {
			if !have[string(s)] {
				have[string(s)] = true
				out = append(out, Answer{SID: slices.Clone(s), Group: true, Enabled: true})
			}
		}
		return out, nil
	}
	// An ad user always holds primaryGroupID, so a read without it is a bind
	// that may not see it, and a closure without the primary group is short.
	primaryGroup := first(res.Entries[0], "primaryGroupID")
	if primaryGroup == nil {
		return nil, unavailable("groups", errors.New("no primaryGroupID read"))
	}
	rid, err := strconv.ParseUint(string(primaryGroup), 10, 32)
	if err != nil {
		return nil, unavailable("groups", fmt.Errorf("primaryGroupID %q: %w", primaryGroup, err))
	}
	if len(domainSID) < 8 {
		return nil, unavailable("groups", errors.New("no domain SID to name the primary group by"))
	}
	pg, err := c.lookupSIDs(ctx, nc, [][]byte{WithRID(domainSID, uint32(rid))})
	if err != nil {
		return nil, err
	}
	if len(pg) != 1 {
		return nil, unavailable("groups", fmt.Errorf("no entry holds the primary group's SID, RID %d", rid))
	}
	return c.walk(ctx, nc, "(objectClass=group)"+securityGroup, pg,
		[]string{"(member=" + ldap.EscapeFilter(user.DN) + ")", "(member=" + ldap.EscapeFilter(pg[0].DN) + ")"})
}

// walk is a closure level by level, under base: the groups of class that
// one of conds matches, then those naming them in member, each level one
// paged search per orTerms groups of the level before, each group visited
// once; out holds the groups already found. A closure deeper than maxLevels
// or wider than maxGroups fails ErrUnavailable, so a hostile or runaway
// nesting costs a bounded number of queries.
func (c *Client) walk(ctx context.Context, base, class string, out []Answer, conds []string) ([]Answer, error) {
	seen := map[string]bool{}
	for _, a := range out {
		seen[strings.ToLower(a.DN)] = true
	}
	for level := 0; len(conds) > 0; level++ {
		if level == maxLevels {
			return nil, unavailable("groups", fmt.Errorf("nested more than %d levels deep", maxLevels))
		}
		var next []string
		for batch := range slices.Chunk(conds, orTerms) {
			entries, err := c.all(ctx, Request{Base: base, Scope: ScopeSubtree,
				Filter: "(&" + class + "(|" + strings.Join(batch, "") + "))", Attrs: c.rec.attrs()})
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if k := strings.ToLower(e.DN); !seen[k] {
					seen[k] = true
					out = append(out, c.answer(e))
					next = append(next, "(member="+ldap.EscapeFilter(e.DN)+")")
				}
			}
			if len(out) > maxGroups {
				return nil, unavailable("groups", fmt.Errorf("more than %d groups", maxGroups))
			}
		}
		conds = next
	}
	return out, nil
}

// members is the member values of the group at dn, the distinguished names
// of its direct members. An ad group's are range-retrieved, member;range=
// <lo>-* asked until a part ending in * comes back; a part that does not
// parse, does not start where it was asked, or holds other than its range's
// count of values fails the read ErrUnavailable, so a hostile range ends it
// rather than loop.
//
// decision: unexported, and reached only from tests, because none of RFC
// 18's directory questions asks a group's members: a closure is read through
// tokenGroups or a level-by-level walk, neither of which reads member. It is the client's range retrieval, held to Samba beside paging;
// export it with the first caller that lists a group's members.
func (c *Client) members(ctx context.Context, dn string) ([]string, error) {
	c.begin()
	defer c.end()
	read := func(attr string) (Entry, error) {
		group := "group"
		if !c.rec.IsAD() {
			group = "posixGroup"
		}
		_, res, err := c.do(ctx, Request{Base: dn, Scope: ScopeBase, Filter: "(objectClass=" + group + ")", Attrs: []string{attr}})
		if err != nil {
			return Entry{}, err
		}
		if len(res.Entries) != 1 {
			return Entry{}, unavailable("members", fmt.Errorf("the group %q answered %d entries", dn, len(res.Entries)))
		}
		return res.Entries[0], nil
	}
	var out []string
	add := func(vs [][]byte) {
		for _, v := range vs {
			out = append(out, string(v))
		}
	}
	if !c.rec.IsAD() {
		e, err := read("member")
		if err != nil {
			return nil, err
		}
		add(values(e, "member"))
		return out, nil
	}
	lo := 0
	for range maxRanges {
		e, err := read(Range{Attr: "member", Lo: lo, Hi: -1}.String())
		if err != nil {
			return nil, err
		}
		r, vs, err := memberPart(e)
		if err != nil {
			return nil, unavailable("members", err)
		}
		switch {
		case r == nil && lo == 0: // every value at once, or none
			add(vs)
			return out, nil
		case r == nil:
			return nil, unavailable("members", fmt.Errorf("no part after value %d", lo-1))
		case r.Lo != lo:
			return nil, unavailable("members", fmt.Errorf("part %s, asked from %d", r, lo))
		case r.Hi < 0:
			add(vs)
			return out, nil
		case len(vs) != r.Hi-r.Lo+1:
			return nil, unavailable("members", fmt.Errorf("part %s holds %d values", r, len(vs)))
		}
		add(vs)
		lo = r.Hi + 1
	}
	return nil, unavailable("members", fmt.Errorf("more than %d parts", maxRanges))
}

// memberPart is the member values in e: their range, nil when e holds them
// under member itself, and the values.
func memberPart(e Entry) (*Range, [][]byte, error) {
	for k, vs := range e.Attrs {
		if strings.EqualFold(k, "member") {
			return nil, vs, nil
		}
		if !strings.HasPrefix(strings.ToLower(k), "member;") {
			continue
		}
		r, ok := ParseRange(k)
		if !ok || !strings.EqualFold(r.Attr, "member") {
			return nil, nil, fmt.Errorf("the attribute %q is no range of member", k)
		}
		return &r, vs, nil
	}
	return nil, nil, nil
}
