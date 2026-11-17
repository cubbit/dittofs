package api

import (
	"bytes"
	"context"
	"iter"
	"slices"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// shareDoc is a share as its PATCH carries it: its spec alone.
type shareDoc struct {
	Spec apiv1.ShareSpec `json:"spec"`
}

// namespaceOf is the namespace a share's new content is counted in, the one
// at its write generation; empty for a share that names none.
func namespaceOf(sh entity.Share) string {
	for _, g := range sh.Namespaces {
		if g.Generation == sh.Write {
			return idString(g.Namespace)
		}
	}
	return ""
}

// caseOf is a share's fold rule as its spec's case sensitivity.
func caseOf(f entity.FoldRule) string {
	if f == entity.FoldUpcase {
		return "insensitive"
	}
	return "sensitive"
}

// foldOf is the fold rule a create's case sensitivity names, sensitive when
// it names none; false for a value that is neither.
func foldOf(c string) (entity.FoldRule, bool) {
	switch c {
	case "", "sensitive":
		return entity.FoldIdentity, true
	case "insensitive":
		return entity.FoldUpcase, true
	}
	return 0, false
}

func specOf(sh entity.Share) apiv1.ShareSpec {
	return apiv1.ShareSpec{Name: sh.Name, Path: sh.Path, Namespace: namespaceOf(sh), CaseSensitivity: caseOf(sh.Fold)}
}

// shareOf is sh as a write answers it: with no applied, which no node can
// have reached before the answer.
func shareOf(sh entity.Share) apiv1.Share {
	return apiv1.Share{ID: idString(sh.ID), Generation: sh.Generation, Spec: specOf(sh), Status: apiv1.ShareStatus{State: sh.State.String()}}
}

// shareSpecs is what the write adapter knows of a share's spec: its tag is
// the info record's generation, which only a share put raises, so a change
// of state, of namespaces or of the export policy moves no ETag a client
// holds for the spec.
var shareSpecs = writing[shareDoc]{kind: entity.ResourceShare, read: func(tx entity.ControlPlaneTx, id [16]byte) (shareDoc, uint64, bool, error) {
	sh, ok, err := tx.Share(entity.ShareID(id))
	return shareDoc{specOf(sh)}, sh.Generation, ok, err
}}

// shareTags is shareSpecs for a call that carries no body: quiesce, resume
// and delete.
var shareTags = writing[none]{kind: entity.ResourceShare, read: func(tx entity.ControlPlaneTx, id [16]byte) (none, uint64, bool, error) {
	sh, ok, err := tx.Share(entity.ShareID(id))
	return none{}, sh.Generation, ok, err
}}

// newShares is what the write adapter knows of a share it creates: the
// spec and the export policy the create wrote, which its audit record holds.
var newShares = writing[apiv1.NewShare]{kind: entity.ResourceShare, read: func(tx entity.ControlPlaneTx, id [16]byte) (apiv1.NewShare, uint64, bool, error) {
	sh, ok, err := tx.Share(entity.ShareID(id))
	if err != nil || !ok {
		return apiv1.NewShare{}, 0, false, err
	}
	p, err := tx.ExportPolicy(sh.ID)
	return apiv1.NewShare{Spec: specOf(sh), ExportPolicy: policyOf(p)}, sh.Generation, err == nil, err
}}

// createShare is POST /shares: in one transaction, the share in the one
// existing namespace its spec names, enabled, bound to the case sensitivity
// it names, and its export policy as the request states it. PutShare reads
// the namespace tracked and refuses one that does not exist, and a namespace
// exists only on a remote store, so a share is never created where no
// remote store holds its content.
//
// decision: a new share admits nobody: the create writes no grant. An open
// default is one operator's forgotten step away from a share every client on
// the network can write. Overturned if a provisioning workflow shows the
// extra grant call to be the common error, and then by a named template as
// the default, not an open one.
func createShare(_ context.Context, rq request[apiv1.NewShare], _ struct{}, tx entity.ControlPlaneTx) (apiv1.Share, [16]byte, error) {
	ns, err := entity.ParseShareID(rq.Body.Spec.Namespace)
	if err != nil {
		return apiv1.Share{}, rq.ID, &fieldError{field: "spec.namespace", detail: "not a namespace ID"}
	}
	fold, ok := foldOf(rq.Body.Spec.CaseSensitivity)
	if !ok {
		return apiv1.Share{}, rq.ID, &fieldError{field: "spec.case_sensitivity", detail: "sensitive or insensitive"}
	}
	id := entity.NewShareID()
	p, err := policyFrom(id, rq.Body.ExportPolicy, "export_policy.")
	if err != nil {
		return apiv1.Share{}, id, err
	}
	sh := entity.Share{ID: id, Name: rq.Body.Spec.Name, Path: rq.Body.Spec.Path, Root: entity.NewFileID(), Fold: fold,
		Namespaces: []entity.NamespaceGen{{Namespace: entity.NamespaceID(ns)}}}
	if _, err := tx.PutShare(sh, 0); err != nil {
		return apiv1.Share{}, id, err
	}
	if _, err := tx.PutExportPolicy(p, 0); err != nil {
		return apiv1.Share{}, id, err
	}
	sh, _, err = tx.Share(id)
	return shareOf(sh), id, err
}

// patchShare is PATCH /shares/{id}: a rename or a re-path, which changes
// only the share's index rows and raises ShareList. The put runs first, so a
// share being removed or retired is refused stale by the store's check alone,
// ahead of the checks below; a refusal after it rolls the put back. A patch
// naming another namespace is refused, since only a re-home moves a share's
// content, and one naming another case sensitivity, bound at the share's
// creation.
func patchShare(_ context.Context, rq request[shareDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.Share, [16]byte, error) {
	sh, _, err := tx.Share(entity.ShareID(rq.ID))
	if err != nil {
		return apiv1.Share{}, rq.ID, err
	}
	sh.Name, sh.Path = rq.Body.Spec.Name, rq.Body.Spec.Path
	if sh.Generation, err = tx.PutShare(sh, rq.Expect); err != nil {
		return apiv1.Share{}, rq.ID, err
	}
	if id, err := entity.ParseShareID(rq.Body.Spec.Namespace); err != nil || idString(id) != namespaceOf(sh) {
		return apiv1.Share{}, rq.ID, &fieldError{field: "spec.namespace", detail: "a share's namespace changes only by :rehome"}
	}
	if rq.Body.Spec.CaseSensitivity != caseOf(sh.Fold) {
		return apiv1.Share{}, rq.ID, &fieldError{field: "spec.case_sensitivity", detail: "bound when the share is created"}
	}
	return shareOf(sh), rq.ID, nil
}

// setState is :quiesce or :resume: one SetShareState from from to to, whose
// expected state is its precondition, so it takes no If-Match. A share
// already in to is left as it is and answered 200; one being removed or
// retired is refused stale by the store.
func setState(from, to entity.ShareState) func(context.Context, request[none], struct{}, entity.ControlPlaneTx) (apiv1.Share, [16]byte, error) {
	return func(_ context.Context, rq request[none], _ struct{}, tx entity.ControlPlaneTx) (apiv1.Share, [16]byte, error) {
		sh, ok, err := tx.Share(entity.ShareID(rq.ID))
		switch {
		case err != nil:
			return apiv1.Share{}, rq.ID, err
		case !ok:
			return apiv1.Share{}, rq.ID, errNotFound
		}
		expect := from
		if sh.State == to {
			expect = to
		}
		if err := tx.SetShareState(sh.ID, expect, to); err != nil {
			return apiv1.Share{}, rq.ID, err
		}
		sh.State = to
		return shareOf(sh), rq.ID, nil
	}
}

// deleteShare is DELETE /shares/{id}?snapshots=: DeleteShare's mark under
// the generation If-Match names, its disposition refuse when the request
// names none.
func deleteShare(_ context.Context, rq request[none], _ struct{}, tx entity.ControlPlaneTx) (none, [16]byte, error) {
	var d entity.SnapshotDisposition
	if v, ok := rq.Query["snapshots"]; ok && (len(v) != 1 || d.UnmarshalText([]byte(v[0])) != nil) {
		return none{}, rq.ID, &fieldError{field: "snapshots", detail: "one of refuse, delete or detach"}
	}
	return none{}, rq.ID, tx.DeleteShare(entity.ShareID(rq.ID), d, rq.Expect)
}

// statusOf is sh's status as a read answers it: on how many nodes it is
// applied, those that have reconciled the share list through its position,
// and how many refused it.
func statusOf(pp perPage, sh entity.Share) apiv1.ShareStatus {
	ref := entity.ResourceRef{Kind: entity.ResourceShare, ID: sh.ID[:]}
	a := pp.applied(ref, sh.Position, func(st entity.NodeStatus) uint64 { return st.ShareList })
	v := apiv1.ShareStatus{State: sh.State.String(), Applied: &a}
	for _, st := range pp.statuses {
		if slices.ContainsFunc(st.Refused, func(r entity.RefusedRecord) bool {
			return r.Record.Kind == ref.Kind && bytes.Equal(r.Record.ID, ref.ID)
		}) {
			v.Refused++
		}
	}
	return v
}

// shareRow is a share as a listing yields it, with the key its cursor
// resumes after: its name in the name order, its ID in a namespace's.
type shareRow struct {
	sh  entity.Share
	key []byte
}

// shares is GET /shares, every share in folded-name order with on how many
// nodes it is applied. ?name= answers the one share of that name, or none;
// ?namespace= the shares counted in that namespace, in ID order, a share
// being removed left out as the name order leaves it out; both together, the
// named share when it is counted in that namespace.
var shares = listing[shareRow, apiv1.Share]{
	filters: []string{"name", "namespace"},
	records: func(ctx context.Context, d Deps, after []byte, f map[string]string) iter.Seq2[shareRow, error] {
		return func(yield func(shareRow, error) bool) {
			ns, inNS := f["namespace"]
			if inNS {
				if _, err := entity.ParseShareID(ns); err != nil {
					yield(shareRow{}, &fieldError{field: "namespace", detail: "not a namespace ID"})
					return
				}
			}
			if name, ok := f["name"]; ok {
				sh, found, err := d.Store.ShareNamed(ctx, name)
				if err != nil || found && (!inNS || namespaceOf(sh) == ns) {
					yield(shareRow{sh, nil}, err)
				}
				return
			}
			var all iter.Seq2[entity.Share, error]
			if inNS {
				id, _ := entity.ParseShareID(ns)
				var from entity.ShareID
				copy(from[:], after)
				all = d.Store.NamespaceShares(ctx, entity.NamespaceID(id), from)
			} else {
				all = d.Store.SharesAfter(ctx, string(after))
			}
			for sh, err := range all {
				switch {
				case err != nil:
					yield(shareRow{}, err)
					return
				case sh.State == entity.ShareRemoving || sh.State == entity.ShareRetired:
					continue
				}
				key := []byte(sh.Name)
				if inNS {
					key = bytes.Clone(sh.ID[:])
				}
				if !yield(shareRow{sh, key}, nil) {
					return
				}
			}
		}
	},
	key: func(r shareRow) []byte { return r.key },
	item: func(r shareRow, pp perPage) apiv1.Share {
		v := shareOf(r.sh)
		v.Status = statusOf(pp, r.sh)
		return v
	},
}

// shareRead is a share as its GET answers it, its state, and whether it is
// serving: at least one node is registered, and every one serves it, or
// one refuses it.
type shareRead struct {
	apiv1.Share
	state   entity.ShareState
	serving bool
}

// readShare is the share the request names, 404 when it does not exist, with
// on how many nodes it is applied and how many refused it, so a wait that
// ends on a refusal tells the caller which ending it was.
//
// ponytail: a node's status carries no binding state, so a share is applied,
// and serving, on a node once that node has reconciled the share list through
// the share's position and holds the share neither refused nor
// restart-pending. Read each node's binding state once the status reports
// one, so a binding that fails after it is added reads as not serving. Every
// registered node counts, so a node that has reported no status, whose
// status is stale, or that will never serve the share holds ?wait=serving to
// its bound; count only the nodes that serve the share once membership says
// which.
func readShare(ctx context.Context, d Deps, rq request[none]) (shareRead, error) {
	if d.Nodes == nil {
		return shareRead{}, errNoStore
	}
	sh, _, err := d.Store.Share(ctx, entity.ShareID(rq.ID))
	if err != nil {
		return shareRead{}, err
	}
	var pp perPage
	if pp.statuses, err = d.Nodes.RegisteredStatuses(ctx); err != nil {
		return shareRead{}, err
	}
	v := shareOf(sh)
	v.Status = statusOf(pp, sh)
	a := v.Status.Applied
	return shareRead{v, sh.State, a.Of > 0 && a.Nodes == a.Of || v.Status.Refused > 0}, nil
}

// share is GET /shares/{id}. ?wait=serving returns once the share is
// serving, for at most the longest wait; a duration returns once its
// generation or its state changes.
//
// decision: only an enabled share is waited on to serve. A quiesced one
// serves nobody until it is resumed, so the wait ends at once with the share
// in its state, never claiming it serves; one being removed never serves
// again, so the wait is refused 409 stale. Overturned if a client needs to
// wait through a quiesce for the resume.
var share = watching[shareRead]{
	read: readShare,
	tag:  func(s shareRead) uint64 { return s.Generation },
	until: func(w string) (time.Duration, func(first, now shareRead) (bool, error), error) {
		if w == "serving" {
			return maxWait, func(_, now shareRead) (bool, error) {
				switch now.state {
				case entity.ShareEnabled:
					return now.serving, nil
				case entity.ShareQuiesced:
					return true, nil
				}
				return false, entity.ErrStale
			}, nil
		}
		return changes(func(first, now shareRead) bool {
			return first.Generation != now.Generation || first.state != now.state
		})(w)
	},
}
