package vfstest

import (
	"context"
	"net/netip"
	"sync"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// Mutations is a Service that passes every call through to the Service it
// wraps, unchanged, and records each one that may change state (vfs.Mutates):
// the one-call rule a handler test reads. An entry takes its place in the
// record as its call starts, naming the call's reference, and is given its
// answer as the call returns. It answers nothing in the wrapped Service's
// place. Each method is implemented explicitly, so a method added to Service
// fails to compile here until it is recorded.
type Mutations struct {
	s     vfs.Service
	mu    sync.Mutex
	calls []*Mutation
}

// Mutation is one recorded call: its operation, the reference it names, and
// its answer. A field the call does not carry is zero.
type Mutation struct {
	Op vfs.Op

	// The reference the call names.
	Client  vfs.ClientID   // the client of a client-record or state call
	Session vfs.SessionRef // Disconnect's session
	End     vfs.SessionEnd // Disconnect's Last and the Gen it was given
	State   vfs.StateRef   // FreeState's reference
	Open    *vfs.OpenRef   // the open a call acts through, a Target's included; nil: none
	Owner   vfs.LockOwner  // the lock owner of Lock, Unlock, CancelLock and ReleaseLockOwner
	Range   vfs.ByteRange  // CancelLock's waiter is its Open, Owner and Range
	Watch   vfs.WatchID    // Unwatch's waiter

	// The answer, set when the call returns.
	Err error
	Gen uint64 // the Gen Disconnect returned
}

var _ vfs.Service = (*Mutations)(nil)

// RecordMutations wraps s.
func RecordMutations(s vfs.Service) *Mutations { return &Mutations{s: s} }

// Calls is each call that may change state served so far, in the order they
// started, each with the answer it has returned; a call still running has
// none yet.
func (rec *Mutations) Calls() []Mutation {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]Mutation, len(rec.calls))
	for i, m := range rec.calls {
		out[i] = *m
	}
	return out
}

// Ops is the operation of each call Calls holds, in the same order.
func (rec *Mutations) Ops() []vfs.Op {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	ops := make([]vfs.Op, len(rec.calls))
	for i, m := range rec.calls {
		ops[i] = m.Op
	}
	return ops
}

// Reset forgets the calls recorded so far, so the record starts afresh for
// the next request. A call still running when it is reset is not recorded.
func (rec *Mutations) Reset() {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.calls = nil
}

// start records m as its call starts, if its operation may change state, and
// answers what records the call's error as it returns.
func (rec *Mutations) start(m Mutation) func(err *error) { return rec.begin(m, nil) }

// begin is start, recording also the Gen gen holds as the call returns.
func (rec *Mutations) begin(m Mutation, gen *uint64) func(err *error) {
	if !vfs.Mutates(m.Op) {
		return func(*error) {}
	}
	rec.mu.Lock()
	rec.calls = append(rec.calls, &m)
	rec.mu.Unlock()
	return func(err *error) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if err != nil {
			m.Err = *err
		}
		if gen != nil {
			m.Gen = *gen
		}
	}
}

func (rec *Mutations) Propose(ctx context.Context, c vfs.Call, info vfs.ClientInfo) (_ vfs.Proposal, err error) {
	defer rec.start(Mutation{Op: vfs.OpPropose})(&err)
	return rec.s.Propose(ctx, c, info)
}

func (rec *Mutations) Connect(ctx context.Context, c vfs.Call, info vfs.ClientInfo, cb vfs.Callbacks) (_ vfs.ClientID, _ uint64, err error) {
	defer rec.start(Mutation{Op: vfs.OpConnect})(&err)
	return rec.s.Connect(ctx, c, info, cb)
}

func (rec *Mutations) AddSession(ctx context.Context, c vfs.Call, cl vfs.ClientID, prev []byte, next []byte) (_ vfs.ClientRecord, err error) {
	defer rec.start(Mutation{Op: vfs.OpAddSession, Client: cl})(&err)
	return rec.s.AddSession(ctx, c, cl, prev, next)
}

func (rec *Mutations) Record(ctx context.Context, c vfs.Call, cl vfs.ClientID) (_ vfs.ClientRecord, err error) {
	defer rec.start(Mutation{Op: vfs.OpRecord})(&err)
	return rec.s.Record(ctx, c, cl)
}

func (rec *Mutations) ClientByName(ctx context.Context, c vfs.Call, p vfs.Protocol, name []byte) (_ vfs.ClientRecord, _, _ bool, err error) {
	defer rec.start(Mutation{Op: vfs.OpClientByName})(&err)
	return rec.s.ClientByName(ctx, c, p, name)
}

func (rec *Mutations) Renew(ctx context.Context, c vfs.Call, cl vfs.ClientID) (_ vfs.ClientStatus, err error) {
	defer rec.start(Mutation{Op: vfs.OpRenew, Client: cl})(&err)
	return rec.s.Renew(ctx, c, cl)
}

func (rec *Mutations) Disconnect(ctx context.Context, c vfs.Call, cl vfs.ClientID, s vfs.SessionRef, end vfs.SessionEnd) (gen uint64, err error) {
	defer rec.begin(Mutation{Op: vfs.OpDisconnect, Client: cl, Session: s, End: end}, &gen)(&err)
	return rec.s.Disconnect(ctx, c, cl, s, end)
}

func (rec *Mutations) Destroy(ctx context.Context, c vfs.Call, cl vfs.ClientID) (err error) {
	defer rec.start(Mutation{Op: vfs.OpDestroy, Client: cl})(&err)
	return rec.s.Destroy(ctx, c, cl)
}

func (rec *Mutations) ReclaimComplete(ctx context.Context, c vfs.Call, cl vfs.ClientID, shown []vfs.GraceRef) (err error) {
	defer rec.start(Mutation{Op: vfs.OpReclaimComplete, Client: cl})(&err)
	return rec.s.ReclaimComplete(ctx, c, cl, shown)
}

func (rec *Mutations) FreeState(ctx context.Context, c vfs.Call, cl vfs.ClientID, s vfs.StateRef) (err error) {
	defer rec.start(Mutation{Op: vfs.OpFreeState, Client: cl, State: s})(&err)
	return rec.s.FreeState(ctx, c, cl, s)
}

func (rec *Mutations) TestState(ctx context.Context, c vfs.Call, cl vfs.ClientID, s []vfs.StateRef) []error {
	defer rec.start(Mutation{Op: vfs.OpTestState})(nil)
	return rec.s.TestState(ctx, c, cl, s)
}

func (rec *Mutations) ReleaseLockOwner(ctx context.Context, c vfs.Call, cl vfs.ClientID, o vfs.LockOwner) (err error) {
	defer rec.start(Mutation{Op: vfs.OpReleaseLockOwner, Client: cl, Owner: o})(&err)
	return rec.s.ReleaseLockOwner(ctx, c, cl, o)
}

func (rec *Mutations) HostRestarted(ctx context.Context, c vfs.Call, host []byte, state uint32) (err error) {
	defer rec.start(Mutation{Op: vfs.OpHostRestarted})(&err)
	return rec.s.HostRestarted(ctx, c, host, state)
}

func (rec *Mutations) Lookup(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte) (_ vfs.Handle, _ vfs.File, err error) {
	defer rec.start(Mutation{Op: vfs.OpLookup})(&err)
	return rec.s.Lookup(ctx, c, dir, name)
}

func (rec *Mutations) ReadDir(ctx context.Context, c vfs.Call, dir vfs.Handle, after vfs.Cursor, max int, want vfs.EntryFields) (_ vfs.Page, err error) {
	defer rec.start(Mutation{Op: vfs.OpReadDir})(&err)
	return rec.s.ReadDir(ctx, c, dir, after, max, want)
}

func (rec *Mutations) Create(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte, a vfs.CreateArgs) (_ vfs.CreateResult, err error) {
	defer rec.start(Mutation{Op: vfs.OpCreate})(&err)
	return rec.s.Create(ctx, c, dir, name, a)
}

func (rec *Mutations) Link(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte, target vfs.Handle) (_ vfs.CreateResult, err error) {
	defer rec.start(Mutation{Op: vfs.OpLink})(&err)
	return rec.s.Link(ctx, c, dir, name, target)
}

func (rec *Mutations) Unlink(ctx context.Context, c vfs.Call, dir vfs.Target, name []byte) (_ vfs.UnlinkResult, err error) {
	defer rec.start(Mutation{Op: vfs.OpUnlink, Open: dir.Open})(&err)
	return rec.s.Unlink(ctx, c, dir, name)
}

func (rec *Mutations) Rmdir(ctx context.Context, c vfs.Call, dir vfs.Target, name []byte) (_ vfs.UnlinkResult, err error) {
	defer rec.start(Mutation{Op: vfs.OpRmdir, Open: dir.Open})(&err)
	return rec.s.Rmdir(ctx, c, dir, name)
}

func (rec *Mutations) Rename(ctx context.Context, c vfs.Call, from vfs.Target, fromName []byte, to vfs.Handle, toName []byte, noReplace bool) (_ vfs.RenameResult, err error) {
	defer rec.start(Mutation{Op: vfs.OpRename, Open: from.Open})(&err)
	return rec.s.Rename(ctx, c, from, fromName, to, toName, noReplace)
}

func (rec *Mutations) GetAttr(ctx context.Context, c vfs.Call, h vfs.Handle) (_ vfs.File, err error) {
	defer rec.start(Mutation{Op: vfs.OpGetAttr})(&err)
	return rec.s.GetAttr(ctx, c, h)
}

func (rec *Mutations) SetAttr(ctx context.Context, c vfs.Call, t vfs.Target, a vfs.AttrChange) (_ vfs.WCC, err error) {
	defer rec.start(Mutation{Op: vfs.OpSetAttr, Open: t.Open})(&err)
	return rec.s.SetAttr(ctx, c, t, a)
}

func (rec *Mutations) GetACL(ctx context.Context, c vfs.Call, h vfs.Handle) (_ vfs.ACL, err error) {
	defer rec.start(Mutation{Op: vfs.OpGetACL})(&err)
	return rec.s.GetACL(ctx, c, h)
}

func (rec *Mutations) SetACL(ctx context.Context, c vfs.Call, t vfs.Target, acl vfs.ACL) (_ vfs.WCC, err error) {
	defer rec.start(Mutation{Op: vfs.OpSetACL, Open: t.Open})(&err)
	return rec.s.SetACL(ctx, c, t, acl)
}

func (rec *Mutations) Xattrs(ctx context.Context, c vfs.Call, h vfs.Handle) (_ []vfs.Xattr, err error) {
	defer rec.start(Mutation{Op: vfs.OpXattrs})(&err)
	return rec.s.Xattrs(ctx, c, h)
}

func (rec *Mutations) SetXattr(ctx context.Context, c vfs.Call, t vfs.Target, x vfs.Xattr, m vfs.XattrMode) (_ vfs.WCC, err error) {
	defer rec.start(Mutation{Op: vfs.OpSetXattr, Open: t.Open})(&err)
	return rec.s.SetXattr(ctx, c, t, x, m)
}

func (rec *Mutations) RemoveXattr(ctx context.Context, c vfs.Call, t vfs.Target, name []byte) (_ vfs.WCC, err error) {
	defer rec.start(Mutation{Op: vfs.OpRemoveXattr, Open: t.Open})(&err)
	return rec.s.RemoveXattr(ctx, c, t, name)
}

func (rec *Mutations) Streams(ctx context.Context, c vfs.Call, h vfs.Handle) (_ []vfs.File, err error) {
	defer rec.start(Mutation{Op: vfs.OpStreams})(&err)
	return rec.s.Streams(ctx, c, h)
}

func (rec *Mutations) Access(ctx context.Context, c vfs.Call, h vfs.Handle, parent vfs.Handle, want vfs.Access) (_ vfs.Access, err error) {
	defer rec.start(Mutation{Op: vfs.OpAccess})(&err)
	return rec.s.Access(ctx, c, h, parent, want)
}

func (rec *Mutations) Open(ctx context.Context, c vfs.Call, req vfs.OpenRequest) (_ vfs.OpenResult, err error) {
	defer rec.start(Mutation{Op: vfs.OpOpen})(&err)
	return rec.s.Open(ctx, c, req)
}

func (rec *Mutations) Downgrade(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs, access vfs.Access, deny vfs.Access) (_ uint32, err error) {
	defer rec.start(Mutation{Op: vfs.OpDowngrade, Open: &ref})(&err)
	return rec.s.Downgrade(ctx, c, ref, q, access, deny)
}

func (rec *Mutations) ConfirmOpen(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs) (_ uint32, err error) {
	defer rec.start(Mutation{Op: vfs.OpConfirmOpen, Open: &ref})(&err)
	return rec.s.ConfirmOpen(ctx, c, ref, q)
}

func (rec *Mutations) Close(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs) (_ *vfs.WCC, err error) {
	defer rec.start(Mutation{Op: vfs.OpClose, Open: &ref})(&err)
	return rec.s.Close(ctx, c, ref, q)
}

func (rec *Mutations) Read(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, dst []byte) (_ int, _ bool, err error) {
	defer rec.start(Mutation{Op: vfs.OpRead})(&err)
	return rec.s.Read(ctx, c, ref, off, dst)
}

func (rec *Mutations) Write(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, src []byte, s vfs.Stability) (_ vfs.WriteResult, err error) {
	defer rec.start(Mutation{Op: vfs.OpWrite, Open: &ref})(&err)
	return rec.s.Write(ctx, c, ref, off, src, s)
}

func (rec *Mutations) Commit(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, length int64) (_ vfs.Verifier, err error) {
	defer rec.start(Mutation{Op: vfs.OpCommit, Open: &ref})(&err)
	return rec.s.Commit(ctx, c, ref, off, length)
}

func (rec *Mutations) Deallocate(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, length int64) (_ vfs.WCC, err error) {
	defer rec.start(Mutation{Op: vfs.OpDeallocate, Open: &ref})(&err)
	return rec.s.Deallocate(ctx, c, ref, off, length)
}

func (rec *Mutations) Seek(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, what vfs.SeekWhat) (_ int64, err error) {
	defer rec.start(Mutation{Op: vfs.OpSeek})(&err)
	return rec.s.Seek(ctx, c, ref, off, what)
}

func (rec *Mutations) Copy(ctx context.Context, c vfs.Call, a vfs.CopyArgs) (_ vfs.CopyResult, err error) {
	defer rec.start(Mutation{Op: vfs.OpCopy})(&err)
	return rec.s.Copy(ctx, c, a)
}

func (rec *Mutations) Clone(ctx context.Context, c vfs.Call, a vfs.CopyArgs) (_ vfs.WCC, err error) {
	defer rec.start(Mutation{Op: vfs.OpClone})(&err)
	return rec.s.Clone(ctx, c, a)
}

func (rec *Mutations) Lock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, r vfs.LockReq) (_ vfs.StateRef, err error) {
	defer rec.start(Mutation{Op: vfs.OpLock, Open: &ref, Owner: r.Owner})(&err)
	return rec.s.Lock(ctx, c, ref, r)
}

func (rec *Mutations) CancelLock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, o vfs.LockOwner, r vfs.ByteRange) (_ bool, err error) {
	defer rec.start(Mutation{Op: vfs.OpCancelLock, Open: &ref, Owner: o, Range: r})(&err)
	return rec.s.CancelLock(ctx, c, ref, o, r)
}

func (rec *Mutations) TestLock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, o vfs.LockOwner, r vfs.ByteRange, exclusive bool) (_ *vfs.Lock, err error) {
	defer rec.start(Mutation{Op: vfs.OpTestLock})(&err)
	return rec.s.TestLock(ctx, c, ref, o, r, exclusive)
}

func (rec *Mutations) Unlock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, o vfs.LockOwner, rs []vfs.ByteRange, q vfs.Seqs, replay *vfs.LockSequence) (_ uint32, err error) {
	defer rec.start(Mutation{Op: vfs.OpUnlock, Open: &ref, Owner: o})(&err)
	return rec.s.Unlock(ctx, c, ref, o, rs, q, replay)
}

func (rec *Mutations) AckRecall(ctx context.Context, c vfs.Call, g vfs.GrantID, to vfs.GrantKind) (err error) {
	defer rec.start(Mutation{Op: vfs.OpAckRecall})(&err)
	return rec.s.AckRecall(ctx, c, g, to)
}

func (rec *Mutations) Watch(ctx context.Context, c vfs.Call, w vfs.WatchSpec) (_ vfs.WatchID, err error) {
	defer rec.start(Mutation{Op: vfs.OpWatch})(&err)
	return rec.s.Watch(ctx, c, w)
}

func (rec *Mutations) Unwatch(ctx context.Context, c vfs.Call, w vfs.WatchID) (err error) {
	defer rec.start(Mutation{Op: vfs.OpUnwatch, Watch: w})(&err)
	return rec.s.Unwatch(ctx, c, w)
}

func (rec *Mutations) Root(ctx context.Context, c vfs.Call, s vfs.ShareRef) (_ vfs.Handle, _ vfs.ShareInfo, err error) {
	defer rec.start(Mutation{Op: vfs.OpRoot})(&err)
	return rec.s.Root(ctx, c, s)
}

func (rec *Mutations) SecInfo(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte) (_ []vfs.AuthFlavor, err error) {
	defer rec.start(Mutation{Op: vfs.OpSecInfo})(&err)
	return rec.s.SecInfo(ctx, c, dir, name)
}

func (rec *Mutations) ListShares(ctx context.Context, c vfs.Call) (_ []vfs.ShareInfo, err error) {
	defer rec.start(Mutation{Op: vfs.OpListShares})(&err)
	return rec.s.ListShares(ctx, c)
}

func (rec *Mutations) StatFS(ctx context.Context, c vfs.Call, h vfs.Handle) (_ vfs.FilesystemInfo, err error) {
	defer rec.start(Mutation{Op: vfs.OpStatFS})(&err)
	return rec.s.StatFS(ctx, c, h)
}

func (rec *Mutations) Quota(ctx context.Context, c vfs.Call, h vfs.Handle, p vfs.Principal) (_ vfs.Usage, _ vfs.Quota, err error) {
	defer rec.start(Mutation{Op: vfs.OpQuota})(&err)
	return rec.s.Quota(ctx, c, h, p)
}

func (rec *Mutations) Snapshots(ctx context.Context, c vfs.Call, h vfs.Handle) (_ []vfs.SnapshotInfo, err error) {
	defer rec.start(Mutation{Op: vfs.OpSnapshots})(&err)
	return rec.s.Snapshots(ctx, c, h)
}

func (rec *Mutations) MoveTarget(ctx context.Context, c vfs.Call, h vfs.Handle) (_ netip.Addr, err error) {
	defer rec.start(Mutation{Op: vfs.OpMoveTarget})(&err)
	return rec.s.MoveTarget(ctx, c, h)
}

func (rec *Mutations) SignIn(ctx context.Context, step vfs.SignInStep) (_ vfs.SignInResult, err error) {
	defer rec.start(Mutation{Op: vfs.OpSignIn})(&err)
	return rec.s.SignIn(ctx, step)
}

func (rec *Mutations) Spell(ctx context.Context, p vfs.Principal, as vfs.Scheme) (_ vfs.Spelling, err error) {
	defer rec.start(Mutation{Op: vfs.OpSpell})(&err)
	return rec.s.Spell(ctx, p, as)
}

func (rec *Mutations) Find(ctx context.Context, c vfs.Call, s vfs.Spelling) (_ vfs.Principal, err error) {
	defer rec.start(Mutation{Op: vfs.OpFind})(&err)
	return rec.s.Find(ctx, c, s)
}
