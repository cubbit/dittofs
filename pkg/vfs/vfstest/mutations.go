package vfstest

import (
	"context"
	"sync"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// Mutations is a Service that passes every call through to the Service it
// wraps, unchanged, and records each one that may change state (vfs.Mutates)
// as it starts: the one-call rule a handler test reads. It answers nothing in
// the wrapped Service's place. Each method is implemented explicitly, so a
// method added to Service fails to compile here until it is recorded.
type Mutations struct {
	s   vfs.Service
	mu  sync.Mutex
	ops []vfs.Op
}

var _ vfs.Service = (*Mutations)(nil)

// RecordMutations wraps s.
func RecordMutations(s vfs.Service) *Mutations { return &Mutations{s: s} }

// Ops is each call that may change state served so far, in the order they
// started.
func (rec *Mutations) Ops() []vfs.Op {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]vfs.Op(nil), rec.ops...)
}

// Reset forgets the calls recorded so far, so the record starts afresh for
// the next request.
func (rec *Mutations) Reset() {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.ops = nil
}

func (rec *Mutations) note(op vfs.Op) {
	if !vfs.Mutates(op) {
		return
	}
	rec.mu.Lock()
	rec.ops = append(rec.ops, op)
	rec.mu.Unlock()
}

func (rec *Mutations) Propose(ctx context.Context, c vfs.Call, info vfs.ClientInfo) (vfs.Proposal, error) {
	rec.note(vfs.OpPropose)
	return rec.s.Propose(ctx, c, info)
}

func (rec *Mutations) Connect(ctx context.Context, c vfs.Call, info vfs.ClientInfo, cb vfs.Callbacks) (vfs.ClientID, uint64, error) {
	rec.note(vfs.OpConnect)
	return rec.s.Connect(ctx, c, info, cb)
}

func (rec *Mutations) AddSession(ctx context.Context, c vfs.Call, cl vfs.ClientID, prev []byte, next []byte) (vfs.ClientRecord, error) {
	rec.note(vfs.OpAddSession)
	return rec.s.AddSession(ctx, c, cl, prev, next)
}

func (rec *Mutations) Record(ctx context.Context, c vfs.Call, cl vfs.ClientID) (vfs.ClientRecord, error) {
	rec.note(vfs.OpRecord)
	return rec.s.Record(ctx, c, cl)
}

func (rec *Mutations) Renew(ctx context.Context, c vfs.Call, cl vfs.ClientID) (vfs.ClientStatus, error) {
	rec.note(vfs.OpRenew)
	return rec.s.Renew(ctx, c, cl)
}

func (rec *Mutations) Disconnect(ctx context.Context, c vfs.Call, cl vfs.ClientID, s vfs.SessionRef, end vfs.SessionEnd) (uint64, error) {
	rec.note(vfs.OpDisconnect)
	return rec.s.Disconnect(ctx, c, cl, s, end)
}

func (rec *Mutations) Destroy(ctx context.Context, c vfs.Call, cl vfs.ClientID) error {
	rec.note(vfs.OpDestroy)
	return rec.s.Destroy(ctx, c, cl)
}

func (rec *Mutations) ReclaimComplete(ctx context.Context, c vfs.Call, cl vfs.ClientID, shown []vfs.GraceRef) error {
	rec.note(vfs.OpReclaimComplete)
	return rec.s.ReclaimComplete(ctx, c, cl, shown)
}

func (rec *Mutations) FreeState(ctx context.Context, c vfs.Call, cl vfs.ClientID, s vfs.StateRef) error {
	rec.note(vfs.OpFreeState)
	return rec.s.FreeState(ctx, c, cl, s)
}

func (rec *Mutations) TestState(ctx context.Context, c vfs.Call, cl vfs.ClientID, s []vfs.StateRef) []error {
	rec.note(vfs.OpTestState)
	return rec.s.TestState(ctx, c, cl, s)
}

func (rec *Mutations) ReleaseLockOwner(ctx context.Context, c vfs.Call, cl vfs.ClientID, o vfs.LockOwner) error {
	rec.note(vfs.OpReleaseLockOwner)
	return rec.s.ReleaseLockOwner(ctx, c, cl, o)
}

func (rec *Mutations) HostRestarted(ctx context.Context, c vfs.Call, host []byte, state uint32) error {
	rec.note(vfs.OpHostRestarted)
	return rec.s.HostRestarted(ctx, c, host, state)
}

func (rec *Mutations) Lookup(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte) (vfs.Handle, vfs.File, error) {
	rec.note(vfs.OpLookup)
	return rec.s.Lookup(ctx, c, dir, name)
}

func (rec *Mutations) ReadDir(ctx context.Context, c vfs.Call, dir vfs.Handle, after vfs.Cursor, max int, want vfs.EntryFields) (vfs.Page, error) {
	rec.note(vfs.OpReadDir)
	return rec.s.ReadDir(ctx, c, dir, after, max, want)
}

func (rec *Mutations) Create(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte, a vfs.CreateArgs) (vfs.CreateResult, error) {
	rec.note(vfs.OpCreate)
	return rec.s.Create(ctx, c, dir, name, a)
}

func (rec *Mutations) Link(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte, target vfs.Handle) (vfs.CreateResult, error) {
	rec.note(vfs.OpLink)
	return rec.s.Link(ctx, c, dir, name, target)
}

func (rec *Mutations) Unlink(ctx context.Context, c vfs.Call, dir vfs.Target, name []byte) (vfs.UnlinkResult, error) {
	rec.note(vfs.OpUnlink)
	return rec.s.Unlink(ctx, c, dir, name)
}

func (rec *Mutations) Rmdir(ctx context.Context, c vfs.Call, dir vfs.Target, name []byte) (vfs.UnlinkResult, error) {
	rec.note(vfs.OpRmdir)
	return rec.s.Rmdir(ctx, c, dir, name)
}

func (rec *Mutations) Rename(ctx context.Context, c vfs.Call, from vfs.Target, fromName []byte, to vfs.Handle, toName []byte, noReplace bool) (vfs.RenameResult, error) {
	rec.note(vfs.OpRename)
	return rec.s.Rename(ctx, c, from, fromName, to, toName, noReplace)
}

func (rec *Mutations) GetAttr(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.File, error) {
	rec.note(vfs.OpGetAttr)
	return rec.s.GetAttr(ctx, c, h)
}

func (rec *Mutations) SetAttr(ctx context.Context, c vfs.Call, t vfs.Target, a vfs.AttrChange) (vfs.WCC, error) {
	rec.note(vfs.OpSetAttr)
	return rec.s.SetAttr(ctx, c, t, a)
}

func (rec *Mutations) GetACL(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.ACL, error) {
	rec.note(vfs.OpGetACL)
	return rec.s.GetACL(ctx, c, h)
}

func (rec *Mutations) SetACL(ctx context.Context, c vfs.Call, t vfs.Target, acl vfs.ACL) (vfs.WCC, error) {
	rec.note(vfs.OpSetACL)
	return rec.s.SetACL(ctx, c, t, acl)
}

func (rec *Mutations) Xattrs(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.Xattr, error) {
	rec.note(vfs.OpXattrs)
	return rec.s.Xattrs(ctx, c, h)
}

func (rec *Mutations) SetXattr(ctx context.Context, c vfs.Call, t vfs.Target, x vfs.Xattr, m vfs.XattrMode) (vfs.WCC, error) {
	rec.note(vfs.OpSetXattr)
	return rec.s.SetXattr(ctx, c, t, x, m)
}

func (rec *Mutations) RemoveXattr(ctx context.Context, c vfs.Call, t vfs.Target, name []byte) (vfs.WCC, error) {
	rec.note(vfs.OpRemoveXattr)
	return rec.s.RemoveXattr(ctx, c, t, name)
}

func (rec *Mutations) Streams(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.File, error) {
	rec.note(vfs.OpStreams)
	return rec.s.Streams(ctx, c, h)
}

func (rec *Mutations) Access(ctx context.Context, c vfs.Call, h vfs.Handle, parent vfs.Handle, want vfs.Access) (vfs.Access, error) {
	rec.note(vfs.OpAccess)
	return rec.s.Access(ctx, c, h, parent, want)
}

func (rec *Mutations) Open(ctx context.Context, c vfs.Call, req vfs.OpenRequest) (vfs.OpenResult, error) {
	rec.note(vfs.OpOpen)
	return rec.s.Open(ctx, c, req)
}

func (rec *Mutations) Downgrade(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs, access vfs.Access, deny vfs.Access) (uint32, error) {
	rec.note(vfs.OpDowngrade)
	return rec.s.Downgrade(ctx, c, ref, q, access, deny)
}

func (rec *Mutations) ConfirmOpen(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs) (uint32, error) {
	rec.note(vfs.OpConfirmOpen)
	return rec.s.ConfirmOpen(ctx, c, ref, q)
}

func (rec *Mutations) Close(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs) (*vfs.WCC, error) {
	rec.note(vfs.OpClose)
	return rec.s.Close(ctx, c, ref, q)
}

func (rec *Mutations) Read(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, dst []byte) (int, bool, error) {
	rec.note(vfs.OpRead)
	return rec.s.Read(ctx, c, ref, off, dst)
}

func (rec *Mutations) Write(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, src []byte, s vfs.Stability) (vfs.WriteResult, error) {
	rec.note(vfs.OpWrite)
	return rec.s.Write(ctx, c, ref, off, src, s)
}

func (rec *Mutations) Commit(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, length int64) (vfs.Verifier, error) {
	rec.note(vfs.OpCommit)
	return rec.s.Commit(ctx, c, ref, off, length)
}

func (rec *Mutations) Deallocate(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, length int64) (vfs.WCC, error) {
	rec.note(vfs.OpDeallocate)
	return rec.s.Deallocate(ctx, c, ref, off, length)
}

func (rec *Mutations) Seek(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, what vfs.SeekWhat) (int64, error) {
	rec.note(vfs.OpSeek)
	return rec.s.Seek(ctx, c, ref, off, what)
}

func (rec *Mutations) Copy(ctx context.Context, c vfs.Call, a vfs.CopyArgs) (vfs.CopyResult, error) {
	rec.note(vfs.OpCopy)
	return rec.s.Copy(ctx, c, a)
}

func (rec *Mutations) Clone(ctx context.Context, c vfs.Call, a vfs.CopyArgs) (vfs.WCC, error) {
	rec.note(vfs.OpClone)
	return rec.s.Clone(ctx, c, a)
}

func (rec *Mutations) Lock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, r vfs.LockReq) (vfs.StateRef, error) {
	rec.note(vfs.OpLock)
	return rec.s.Lock(ctx, c, ref, r)
}

func (rec *Mutations) CancelLock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, o vfs.LockOwner, r vfs.ByteRange) (bool, error) {
	rec.note(vfs.OpCancelLock)
	return rec.s.CancelLock(ctx, c, ref, o, r)
}

func (rec *Mutations) TestLock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, o vfs.LockOwner, r vfs.ByteRange, exclusive bool) (*vfs.Lock, error) {
	rec.note(vfs.OpTestLock)
	return rec.s.TestLock(ctx, c, ref, o, r, exclusive)
}

func (rec *Mutations) Unlock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, o vfs.LockOwner, rs []vfs.ByteRange, q vfs.Seqs, replay *vfs.LockSequence) (uint32, error) {
	rec.note(vfs.OpUnlock)
	return rec.s.Unlock(ctx, c, ref, o, rs, q, replay)
}

func (rec *Mutations) AckRecall(ctx context.Context, c vfs.Call, g vfs.GrantID, to vfs.GrantKind) error {
	rec.note(vfs.OpAckRecall)
	return rec.s.AckRecall(ctx, c, g, to)
}

func (rec *Mutations) Watch(ctx context.Context, c vfs.Call, w vfs.WatchSpec) (vfs.WatchID, error) {
	rec.note(vfs.OpWatch)
	return rec.s.Watch(ctx, c, w)
}

func (rec *Mutations) Unwatch(ctx context.Context, c vfs.Call, w vfs.WatchID) error {
	rec.note(vfs.OpUnwatch)
	return rec.s.Unwatch(ctx, c, w)
}

func (rec *Mutations) Root(ctx context.Context, c vfs.Call, s vfs.ShareRef) (vfs.Handle, vfs.ShareInfo, error) {
	rec.note(vfs.OpRoot)
	return rec.s.Root(ctx, c, s)
}

func (rec *Mutations) SecInfo(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte) ([]vfs.AuthFlavor, error) {
	rec.note(vfs.OpSecInfo)
	return rec.s.SecInfo(ctx, c, dir, name)
}

func (rec *Mutations) ListShares(ctx context.Context, c vfs.Call) ([]vfs.ShareInfo, error) {
	rec.note(vfs.OpListShares)
	return rec.s.ListShares(ctx, c)
}

func (rec *Mutations) StatFS(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.FilesystemInfo, error) {
	rec.note(vfs.OpStatFS)
	return rec.s.StatFS(ctx, c, h)
}

func (rec *Mutations) Quota(ctx context.Context, c vfs.Call, h vfs.Handle, p vfs.Principal) (vfs.Usage, vfs.Quota, error) {
	rec.note(vfs.OpQuota)
	return rec.s.Quota(ctx, c, h, p)
}

func (rec *Mutations) Snapshots(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.SnapshotInfo, error) {
	rec.note(vfs.OpSnapshots)
	return rec.s.Snapshots(ctx, c, h)
}

func (rec *Mutations) SignIn(ctx context.Context, step vfs.SignInStep) (vfs.SignInResult, error) {
	rec.note(vfs.OpSignIn)
	return rec.s.SignIn(ctx, step)
}

func (rec *Mutations) Spell(ctx context.Context, p vfs.Principal, as vfs.Scheme) (vfs.Spelling, error) {
	rec.note(vfs.OpSpell)
	return rec.s.Spell(ctx, p, as)
}

func (rec *Mutations) Find(ctx context.Context, c vfs.Call, s vfs.Spelling) (vfs.Principal, error) {
	rec.note(vfs.OpFind)
	return rec.s.Find(ctx, c, s)
}
