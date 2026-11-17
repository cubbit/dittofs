package api

import (
	"bytes"
	"context"
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

func specOf(sh entity.Share) apiv1.ShareSpec {
	return apiv1.ShareSpec{Name: sh.Name, Path: sh.Path, Namespace: namespaceOf(sh)}
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
	p, _, err := tx.ExportPolicy(sh.ID)
	return apiv1.NewShare{Spec: specOf(sh), ExportPolicy: policyOf(p)}, sh.Generation, err == nil, err
}}

// createShare is POST /shares: in one transaction, the share in the one
// existing namespace its spec names, enabled, and its export policy as the
// request states it. PutShare reads the namespace tracked and refuses one
// that does not exist, and a namespace exists only on a remote store, so a
// share is never created where no remote store holds its content.
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
	id := entity.NewShareID()
	p, err := policyFrom(id, rq.Body.ExportPolicy, "export_policy.")
	if err != nil {
		return apiv1.Share{}, id, err
	}
	sh := entity.Share{ID: id, Name: rq.Body.Spec.Name, Path: rq.Body.Spec.Path, Root: entity.NewFileID(),
		Namespaces: []entity.NamespaceGen{{Namespace: entity.NamespaceID(ns)}}}
	gen, err := tx.PutShare(sh, 0)
	if err != nil {
		return apiv1.Share{}, id, err
	}
	if _, err := tx.PutExportPolicy(p, 0); err != nil {
		return apiv1.Share{}, id, err
	}
	spec := apiv1.ShareSpec{Name: sh.Name, Path: sh.Path, Namespace: idString(ns)}
	return apiv1.Share{ID: idString(id), Generation: gen, Spec: spec, Status: apiv1.ShareStatus{State: entity.ShareEnabled.String()}}, id, nil
}

// patchShare is PATCH /shares/{id}: a rename or a re-path, which changes
// only the share's index rows and raises ShareList. A patch naming another
// namespace is refused, since only a re-home moves a share's content; a
// share being removed or retired is refused stale by the store.
func patchShare(_ context.Context, rq request[shareDoc], _ struct{}, tx entity.ControlPlaneTx) (shareDoc, [16]byte, error) {
	sh, ok, err := tx.Share(entity.ShareID(rq.ID))
	switch {
	case err != nil:
		return shareDoc{}, rq.ID, err
	case !ok:
		return shareDoc{}, rq.ID, errNotFound
	}
	if ns, err := entity.ParseShareID(rq.Body.Spec.Namespace); err != nil || idString(ns) != namespaceOf(sh) {
		return shareDoc{}, rq.ID, &fieldError{field: "spec.namespace", detail: "a share's namespace changes only by :rehome"}
	}
	sh.Name, sh.Path = rq.Body.Spec.Name, rq.Body.Spec.Path
	_, err = tx.PutShare(sh, rq.Expect)
	return shareDoc{specOf(sh)}, rq.ID, err
}

// setState is :quiesce or :resume: one SetShareState from from to to, whose
// expected state is its precondition, so it takes no If-Match. A share
// already in to is left as it is and answered 200; one being removed or
// retired is refused stale by the store.
func setState(from, to entity.ShareState) func(context.Context, request[none], struct{}, entity.ControlPlaneTx) (none, [16]byte, error) {
	return func(_ context.Context, rq request[none], _ struct{}, tx entity.ControlPlaneTx) (none, [16]byte, error) {
		sh, ok, err := tx.Share(entity.ShareID(rq.ID))
		switch {
		case err != nil:
			return none{}, rq.ID, err
		case !ok:
			return none{}, rq.ID, errNotFound
		}
		expect := from
		if sh.State == to {
			expect = to
		}
		return none{}, rq.ID, tx.SetShareState(sh.ID, expect, to)
	}
}

// deleteShare is DELETE /shares/{id}?snapshots=: DeleteShare's mark, under
// the generation If-Match names, after which the row's operation drives the
// removal. Without ?snapshots= the disposition is refuse, so a share that
// has snapshots is refused until the request says to delete or detach them;
// the store's refusals, a re-home or a move running and a second delete
// among them, come back as its errors.
func deleteShare(_ context.Context, rq request[none], _ struct{}, tx entity.ControlPlaneTx) (none, [16]byte, error) {
	var d entity.SnapshotDisposition
	if v, ok := rq.Query["snapshots"]; ok && (len(v) != 1 || d.UnmarshalText([]byte(v[0])) != nil) {
		return none{}, rq.ID, &fieldError{field: "snapshots", detail: "one of refuse, delete or detach"}
	}
	return none{}, rq.ID, tx.DeleteShare(entity.ShareID(rq.ID), d, rq.Expect)
}

// shareRead is a share as its GET answers it, and whether it is serving:
// every registered node serves it, or one refuses it.
type shareRead struct {
	apiv1.Share
	serving bool
}

// readShare is the share the request names, 404 when it does not exist, with
// on how many nodes it is applied.
//
// ponytail: a node's status carries no binding state, so a share is applied,
// and serving, on a node once that node has reconciled the share list through
// the share's position and holds the share neither refused nor
// restart-pending. Read each node's binding state once the status reports
// one, so a binding that fails after it is added reads as not serving.
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
	ref := entity.ResourceRef{Kind: entity.ResourceShare, ID: sh.ID[:]}
	a := pp.applied(ref, sh.Position, func(st entity.NodeStatus) uint64 { return st.ShareList })
	refused := slices.ContainsFunc(pp.statuses, func(st entity.NodeStatus) bool {
		return slices.ContainsFunc(st.Refused, func(r entity.RefusedRecord) bool {
			return r.Record.Kind == ref.Kind && bytes.Equal(r.Record.ID, ref.ID)
		})
	})
	return shareRead{apiv1.Share{ID: idString(sh.ID), Generation: sh.Generation, Spec: specOf(sh),
		Status: apiv1.ShareStatus{State: sh.State.String(), Applied: &a}}, a.Nodes == a.Of || refused}, nil
}

// share is GET /shares/{id}. ?wait=serving returns once the share is
// serving, for at most the longest wait; a duration returns once its
// generation or its state changes.
var share = watching[shareRead]{
	read: readShare,
	tag:  func(s shareRead) uint64 { return s.Generation },
	until: func(w string) (time.Duration, func(first, now shareRead) bool, error) {
		if w == "serving" {
			return maxWait, func(_, now shareRead) bool { return now.serving }, nil
		}
		return changes(func(first, now shareRead) bool {
			return first.Generation != now.Generation || first.Status.State != now.Status.State
		})(w)
	},
}
