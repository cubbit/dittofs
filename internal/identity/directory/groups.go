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

// inChain is LDAP_MATCHING_RULE_IN_CHAIN, Active Directory's transitive
// match: (member:inChain:=<dn>) holds for every group dn is a member of,
// directly or through other groups.
const inChain = "1.2.840.113556.1.4.1941"

// pageSize is the page each RFC 2696 paged search asks for, Active
// Directory's default MaxPageSize.
const pageSize = 1000

// ponytail: fixed bounds on one question's queries. A paged search reads at
// most 64 pages (64 000 entries), a member list at most 64 ranges, and a SID
// lookup carries at most 64 SIDs in one OR filter, so a PAC of 1 024 SIDs
// costs 16 queries. Each query is under the 5-second timeout and the
// question under RFC 18's 20 seconds. Size the SID batch from a measured
// filter cost, and the page from the directory's MaxPageSize, when a
// directory's answers show either.
const (
	maxPages     = 64
	maxRanges    = 64
	sidsPerQuery = 64
)

// do sends req on the client's connection, opening it when there is none.
// A held connection the directory closed while idle fails the first query on
// it, so that one is sent again, once, on a new one; a fresh connection's
// failure is the directory's answer, not retried.
func (c *Client) do(ctx context.Context, req Request) (Result, error) {
	conn, fresh, err := c.connect(ctx)
	if err != nil {
		return Result{}, err
	}
	res, err := c.search(ctx, conn, req)
	if err != nil && !fresh && errors.Is(err, ErrClosed) {
		if conn, _, err = c.connect(ctx); err != nil {
			return Result{}, err
		}
		res, err = c.search(ctx, conn, req)
	}
	return res, err
}

// all sends req as an RFC 2696 paged search and follows its pages to the
// end; a page that fails, or more than maxPages, fails the whole search.
func (c *Client) all(ctx context.Context, req Request) ([]Entry, error) {
	req.PageSize = pageSize
	var out []Entry
	for range maxPages {
		res, err := c.do(ctx, req)
		if err != nil {
			return nil, err
		}
		out = append(out, res.Entries...)
		if len(res.Cookie) == 0 {
			return out, nil
		}
		req.Cookie = res.Cookie
	}
	return nil, unavailable("paged search", fmt.Errorf("more than %d pages", maxPages))
}

// LookupSIDs answers "who holds this SID" for each of sids, in batches of at
// most sidsPerQuery in one OR filter: the entries found, in the order of
// sids, a SID no entry holds left out. A SID two entries hold fails the
// whole lookup ErrUnavailable, as Lookup does.
func (c *Client) LookupSIDs(ctx context.Context, sids [][]byte) ([]Answer, error) {
	c.begin()
	defer c.end()
	return c.lookupSIDs(ctx, sids)
}

func (c *Client) lookupSIDs(ctx context.Context, sids [][]byte) ([]Answer, error) {
	var uniq [][]byte
	for _, s := range sids {
		if len(s) > 0 && !slices.ContainsFunc(uniq, func(u []byte) bool { return string(u) == string(s) }) {
			uniq = append(uniq, s)
		}
	}
	found := map[string]Answer{}
	for batch := range slices.Chunk(uniq, sidsPerQuery) {
		var f strings.Builder
		f.WriteString("(|")
		for _, s := range batch {
			f.WriteString("(objectSid=" + escapeBinary(s) + ")")
		}
		f.WriteString(")")
		res, err := c.do(ctx, Request{Base: c.rec.Base, Scope: ScopeSubtree, Filter: f.String(), Attrs: c.rec.attrs()})
		if err != nil {
			return nil, err
		}
		for _, e := range res.Entries {
			a := c.answer(e)
			if prev, ok := found[string(a.SID)]; ok {
				c.log.Warn("identity.directory: a SID held by more than one entry, refused",
					"first", prev.DN, "second", a.DN)
				return nil, unavailable("SID lookup", errors.New("two entries hold one SID"))
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
// it ErrUnavailable.
//
// An ad user's closure is one read of its constructed tokenGroups, its SIDs
// then looked up in batches; a group SID no entry holds is answered by its
// SID alone, so the closure keeps it. A bind that may not read tokenGroups
// gets it empty, and the closure is then asked by LDAP_MATCHING_RULE_IN_CHAIN
// from the user and from the primary group primaryGroupID names, since
// member omits a user's primary group. An rfc2307 user's closure is walked
// level by level: the groups naming it in memberUid or member, then the
// groups naming those in member, each group visited once.
func (c *Client) Groups(ctx context.Context, user Answer) ([]Answer, error) {
	c.begin()
	defer c.end()
	if !c.rec.IsAD() {
		return c.walk(ctx, user)
	}
	res, err := c.do(ctx, Request{Base: user.DN, Scope: ScopeBase, Filter: "(objectClass=user)",
		Attrs: []string{"tokenGroups", "primaryGroupID"}})
	if err != nil {
		return nil, err
	}
	if len(res.Entries) != 1 {
		return nil, unavailable("groups", fmt.Errorf("the user %q answered %d entries", user.DN, len(res.Entries)))
	}
	if sids := values(res.Entries[0], "tokenGroups"); len(sids) > 0 {
		out, err := c.lookupSIDs(ctx, sids)
		if err != nil {
			return nil, err
		}
		for _, s := range sids {
			if !slices.ContainsFunc(out, func(a Answer) bool { return string(a.SID) == string(s) }) {
				out = append(out, Answer{SID: slices.Clone(s), Group: true, Enabled: true})
			}
		}
		return out, nil
	}
	return c.chain(ctx, user.DN, first(res.Entries[0], "primaryGroupID"))
}

// chain is an ad user's closure by LDAP_MATCHING_RULE_IN_CHAIN, from the
// user and from its primary group, primaryGroup its RID as text.
func (c *Client) chain(ctx context.Context, dn string, primaryGroup []byte) ([]Answer, error) {
	from := "(member:" + inChain + ":=" + ldap.EscapeFilter(dn) + ")"
	var out []Answer
	if primaryGroup != nil {
		rid, err := strconv.ParseUint(string(primaryGroup), 10, 32)
		if err != nil {
			return nil, unavailable("groups", fmt.Errorf("primaryGroupID %q: %w", primaryGroup, err))
		}
		c.mu.Lock()
		sid := slices.Clone(c.sid)
		c.mu.Unlock()
		if len(sid) < 8 {
			return nil, unavailable("groups", errors.New("no domain SID to name the primary group by"))
		}
		sid[1]++ // one sub-authority more: the RID
		sid = binary.LittleEndian.AppendUint32(sid, uint32(rid))
		pg, err := c.lookupSIDs(ctx, [][]byte{sid})
		if err != nil {
			return nil, err
		}
		if len(pg) != 1 {
			return nil, unavailable("groups", fmt.Errorf("no entry holds the primary group's SID, RID %d", rid))
		}
		out = append(out, pg[0])
		from = "(|" + from + "(member:" + inChain + ":=" + ldap.EscapeFilter(pg[0].DN) + "))"
	}
	entries, err := c.all(ctx, Request{Base: c.rec.Base, Scope: ScopeSubtree,
		Filter: "(&(objectClass=group)" + from + ")", Attrs: c.rec.attrs()})
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !slices.ContainsFunc(out, func(a Answer) bool { return strings.EqualFold(a.DN, e.DN) }) {
			out = append(out, c.answer(e))
		}
	}
	return out, nil
}

// walk is an rfc2307 user's closure, level by level: each level one paged
// search per sidsPerQuery groups of the level before.
func (c *Client) walk(ctx context.Context, user Answer) ([]Answer, error) {
	conds := []string{"(member=" + ldap.EscapeFilter(user.DN) + ")"}
	if user.Name != "" {
		conds = append(conds, "(memberUid="+ldap.EscapeFilter(user.Name)+")")
	}
	seen := map[string]bool{}
	var out []Answer
	for len(conds) > 0 {
		var next []string
		for batch := range slices.Chunk(conds, sidsPerQuery) {
			entries, err := c.all(ctx, Request{Base: c.rec.Base, Scope: ScopeSubtree,
				Filter: "(&(objectClass=posixGroup)(|" + strings.Join(batch, "") + "))", Attrs: c.rec.attrs()})
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
		}
		conds = next
	}
	return out, nil
}

// Members is the member values of the group at dn, the distinguished names
// of its direct members. An ad group's are range-retrieved, member;range=
// <lo>-* asked until a part ending in * comes back; a part that does not
// parse, does not start where it was asked, or holds other than its range's
// count of values fails the read ErrUnavailable, so a hostile range ends it
// rather than loop.
func (c *Client) Members(ctx context.Context, dn string) ([]string, error) {
	c.begin()
	defer c.end()
	read := func(attr string) (Entry, error) {
		group := "group"
		if !c.rec.IsAD() {
			group = "posixGroup"
		}
		res, err := c.do(ctx, Request{Base: dn, Scope: ScopeBase, Filter: "(objectClass=" + group + ")", Attrs: []string{attr}})
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
