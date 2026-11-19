package api

import (
	"context"
	"iter"
	"net/netip"
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// netgroupDoc is a netgroup as its PUT carries it: its spec alone.
type netgroupDoc struct {
	Spec apiv1.NetgroupSpec `json:"spec"`
}

func netgroupOf(g entity.Netgroup) apiv1.Netgroup {
	members := g.Members
	if members == nil {
		members = []string{}
	}
	return apiv1.Netgroup{ID: g.Name, Generation: g.Version, Spec: apiv1.NetgroupSpec{Members: members}}
}

// netgroupRef is the target of a netgroup's row: its name, not empty and at
// most maxNetgroupRef bytes, so a netgroup stored before putNetgroup's name
// rule stays readable and deletable.
//
// decision: the target is bounded far above the name rule's 255 bytes, not
// by the rule, because a refused request's audit record holds its target and
// a target of tens of kilobytes is past what one transaction carries, which
// would leave the refusal unaudited. Lower it to the rule only once no stored
// netgroup can hold a name the rule refuses.
func netgroupRef(_ [16]byte, name string) ([]byte, error) {
	switch {
	case name == "":
		return nil, &fieldError{field: "name", detail: "a netgroup has a name"}
	case len(name) > maxNetgroupRef:
		return nil, &fieldError{field: "name", detail: "longer than " + strconv.Itoa(maxNetgroupRef) + " bytes"}
	}
	return []byte(name), nil
}

// maxNetgroupRef bounds the name a netgroup's path may name, in bytes.
const maxNetgroupRef = 4096

// hostName reports whether s is a DNS host name: dot-separated labels of
// 1 to 63 letters, digits and hyphens, no label starting or ending with a
// hyphen, 253 bytes at most, and the last label not all digits, which is a
// mistyped address rather than a name.
func hostName(s string) bool {
	if s == "" || len(s) > 253 || strings.Trim(s[strings.LastIndexByte(s, '.')+1:], "0123456789") == "" {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		if strings.Trim(l, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") != "" {
			return false
		}
	}
	return true
}

// netgroups is what the write adapter knows of a netgroup: the path's {sub}
// is its name, and its tag is its version.
var netgroups = writing[netgroupDoc]{kind: entity.ResourceNetgroup,
	read: func(tx entity.ControlPlaneTx, _ [16]byte, name string) (netgroupDoc, uint64, bool, error) {
		g, ok, err := tx.Netgroup(name)
		return netgroupDoc{netgroupOf(g).Spec}, g.Version, ok, err
	}}

// putNetgroup is PUT /netgroups/{sub}: the netgroup sub names, created
// under If-None-Match: * or replaced whole under the version If-Match
// names. Its name is one entity.CheckLocalName admits, checked here since the
// store keeps a netgroup's name opaque. Each member is an address, a CIDR or
// a host name.
func putNetgroup(_ context.Context, rq request[netgroupDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.Netgroup, [16]byte, error) {
	if err := entity.CheckLocalName(rq.Sub); err != nil {
		return apiv1.Netgroup{}, rq.ID, &fieldError{field: "name", detail: err.Error()}
	}
	for i, m := range rq.Body.Spec.Members {
		if _, err := netip.ParseAddr(m); err == nil {
			continue
		}
		if _, err := netip.ParsePrefix(m); err == nil {
			continue
		}
		if !hostName(m) {
			return apiv1.Netgroup{}, rq.ID, &fieldError{field: "spec.members[" + strconv.Itoa(i) + "]", detail: "not an address, a CIDR or a host name: " + m}
		}
	}
	g := entity.Netgroup{Name: rq.Sub, Members: rq.Body.Spec.Members}
	var err error
	if g.Version, err = tx.PutNetgroup(g, rq.Expect); err != nil {
		return apiv1.Netgroup{}, rq.ID, err
	}
	return netgroupOf(g), rq.ID, nil
}

// deleteNetgroup is DELETE /netgroups/{sub}: the netgroup removed whole,
// under the version If-Match names. A client rule naming it then fails
// closed.
func deleteNetgroup(_ context.Context, rq request[none], _ struct{}, tx entity.ControlPlaneTx) (none, [16]byte, error) {
	return none{}, rq.ID, tx.DeleteNetgroup(rq.Sub, rq.Expect)
}

// netgroupsList is GET /netgroups, every netgroup in name order.
var netgroupsList = listing[entity.Netgroup, apiv1.Netgroup]{
	records: func(ctx context.Context, d Deps, after []byte, _ map[string]string) iter.Seq2[entity.Netgroup, error] {
		return d.Store.Netgroups(ctx, string(after))
	},
	key:  func(g entity.Netgroup) []byte { return []byte(g.Name) },
	item: func(g entity.Netgroup, _ perPage) apiv1.Netgroup { return netgroupOf(g) },
}

// netgroup is GET /netgroups/{sub}, its version the ETag, 404 for a name
// none holds.
var netgroup = watching[apiv1.Netgroup]{
	read: func(ctx context.Context, d Deps, rq request[none]) (apiv1.Netgroup, error) {
		g, ok, err := d.Store.Netgroup(ctx, rq.Sub)
		if err == nil && !ok {
			err = errNotFound
		}
		return netgroupOf(g), err
	},
	tag:   func(g apiv1.Netgroup) uint64 { return g.Generation },
	until: changes(func(first, now apiv1.Netgroup) bool { return first.Generation != now.Generation }),
}
