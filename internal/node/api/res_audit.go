package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"iter"
	"strings"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// auditRecordOf is a on the wire.
func auditRecordOf(a entity.AuditRecord) apiv1.AuditRecord {
	return apiv1.AuditRecord{ID: idString(a.ID), Time: a.Time, Actor: principalOf(a.Actor), ActorName: a.ActorName,
		Credential: a.Credential, Node: idString(a.Node), Source: a.Source, Request: json.RawMessage(a.Request),
		Target: refOf(a.Target), Before: a.Before, After: a.After, Change: json.RawMessage(a.Change), Outcome: string(a.Outcome)}
}

// auditRow is an audit record and its position, which a cursor resuming
// after it names.
type auditRow struct {
	entity.AuditRecord
	pos []byte
}

// audit is GET /audit, in store-time order: from and to bound the records'
// store time, RFC 3339, to excluded; actor names an account as kind:id, or
// schedule; target names a resource as kind:id.
var audit = listing[auditRow, apiv1.AuditRecord]{
	filters: []string{"from", "to", "actor", "target"},
	records: func(ctx context.Context, d Deps, after []byte, q map[string]string) iter.Seq2[auditRow, error] {
		return func(yield func(auditRow, error) bool) {
			f, err := auditFilter(q)
			if err != nil {
				yield(auditRow{}, err)
				return
			}
			for a, err := range d.Store.AuditRecords(ctx, f, after) {
				if !yield(auditRow{a, d.Store.AuditPosition(a)}, err) || err != nil {
					return
				}
			}
		}
	},
	key:  func(a auditRow) []byte { return a.pos },
	item: func(a auditRow, _ perPage) apiv1.AuditRecord { return auditRecordOf(a.AuditRecord) },
}

// auditFilter is the filter q's parameters name, refusing one that does not
// parse, naming it.
func auditFilter(q map[string]string) (f entity.AuditFilter, err error) {
	for _, k := range []string{"from", "to"} {
		v, ok := q[k]
		if !ok {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return f, &fieldError{field: k, detail: "not an RFC 3339 time"}
		}
		if k == "from" {
			f.From = t
		} else {
			f.To = t
		}
	}
	if v, ok := q["actor"]; ok {
		p := entity.Principal{}
		if v != apiv1.ScheduleKind {
			if p, err = entity.ParsePrincipal(v); err != nil {
				return f, &fieldError{field: "actor", detail: "not kind:id or schedule"}
			}
		}
		f.Actor = &p
	}
	if v, ok := q["target"]; ok {
		kind, id, _ := strings.Cut(v, ":")
		var k entity.ResourceKind
		ref, ok := parseID(id)
		if k.UnmarshalText([]byte(kind)) != nil || !ok {
			return f, &fieldError{field: "target", detail: "not kind:id"}
		}
		f.Target = &entity.ResourceRef{Kind: k, ID: ref}
	}
	return f, nil
}

// parseID is the ID refOf prints as s: a UUID in its canonical form for 16
// bytes, and base64url for any other length, so base64url decoding to 16
// bytes is none, and neither is a UUID in another spelling.
//
// decision: text that parses as a UUID reads as a 16-byte ID when it is the
// canonical spelling and as no ID otherwise, though another length's
// base64url could spell the same text, and the filter then cannot name that
// ID: it takes bytes whose encoding is all hex digits, with or without
// hyphens just where a UUID has them. Overturned once refOf prints each
// kind's ID in its own form.
func parseID(s string) ([]byte, bool) {
	if id, err := entity.ParseShareID(s); err == nil {
		return id[:], id.String() == s
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	return b, err == nil && len(b) != 16 && len(b) > 0
}
