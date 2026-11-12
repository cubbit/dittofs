package vfstest

import (
	"context"
	"sync"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// AnyOp, armed in place of a method's Op, matches a call of every method.
const AnyOp vfs.Op = 0

// FaultService is the Service Faults returns.
type FaultService struct {
	inner vfs.Service

	mu    sync.Mutex
	armed []*fault // in the order armed
}

// fault is one armed fault: an error to answer or a hold to wait on.
type fault struct {
	op   vfs.Op
	left int // the matching calls until it fires, the one it fires on included
	err  error
	hold *Hold
}

var _ vfs.Service = (*FaultService)(nil)

// Faults is a Service that passes every call to inner, arguments and results
// unchanged, until a test arms a fault: Fail or FailNth answers a call an
// error in inner's place, Hold keeps a call until the test releases it. Each
// fault fires once, on the call it names, and every other call passes
// through, so inner serves exactly the calls no error answered. Its methods
// are safe for concurrent use.
//
// Every Service method is written out here and none is embedded, so a method
// added to Service fails to compile until it is wrapped.
func Faults(inner vfs.Service) *FaultService {
	return &FaultService{inner: inner}
}

// Fail makes the next call of op, or of any method for AnyOp, answer err
// without reaching inner.
func (f *FaultService) Fail(op vfs.Op, err error) { f.FailNth(op, 1, err) }

// FailNth makes the n-th call of op from now, counted from 1, or of any
// method for AnyOp, answer err without reaching inner. The calls before it
// pass through. Where two faults would fire on the same call, the one armed
// first fires and the other fires on the next matching call, so faults armed
// on one op one after another fire one call after another. A TestState it
// fires on answers err for every reference. It panics on an n below 1 or a
// nil err, a mistake in the test.
func (f *FaultService) FailNth(op vfs.Op, n int, err error) {
	if n < 1 || err == nil {
		panic("vfstest: FailNth needs an n of at least 1 and an error")
	}
	f.arm(&fault{op: op, left: n, err: err})
}

// Hold makes the next call of op, or of any method for AnyOp, wait until
// Release, or until its context ends, and then pass through to inner, which
// answers it as it answers a cancelled context in the latter case.
func (f *FaultService) Hold(op vfs.Op) *Hold {
	h := &Hold{reached: make(chan struct{}), release: make(chan struct{})}
	f.arm(&fault{op: op, left: 1, hold: h})
	return h
}

// Hold is a call held until the test releases it.
type Hold struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

// Reached is closed once the held call has arrived and is waiting.
func (h *Hold) Reached() <-chan struct{} { return h.reached }

// Release lets the held call through, or the call yet to arrive straight
// through. A second Release does nothing.
func (h *Hold) Release() { h.once.Do(func() { close(h.release) }) }

func (f *FaultService) arm(a *fault) {
	f.mu.Lock()
	f.armed = append(f.armed, a)
	f.mu.Unlock()
}

// at applies the fault a call of op fires, if any: it answers the error the
// call fails with, or waits out a hold and answers nil.
func (f *FaultService) at(ctx context.Context, op vfs.Op) error {
	a := f.take(op)
	switch {
	case a == nil:
		return nil
	case a.hold != nil:
		close(a.hold.reached)
		select {
		case <-a.hold.release:
		case <-ctx.Done():
		}
		return nil
	}
	return a.err
}

// take counts a call of op against every fault it matches and removes and
// answers the first one whose turn it is.
func (f *FaultService) take(op vfs.Op) *fault {
	f.mu.Lock()
	defer f.mu.Unlock()
	fired := -1
	for i, a := range f.armed {
		if a.op != AnyOp && a.op != op {
			continue
		}
		if a.left > 1 {
			a.left--
		} else if fired < 0 {
			fired = i
		}
	}
	if fired < 0 {
		return nil
	}
	a := f.armed[fired]
	f.armed = append(f.armed[:fired], f.armed[fired+1:]...)
	return a
}

// failEach is TestState's answer when it fails: err for every reference.
func failEach(s []vfs.StateRef, err error) []error {
	errs := make([]error, len(s))
	for i := range errs {
		errs[i] = err
	}
	return errs
}

func (f *FaultService) Propose(ctx context.Context, c vfs.Call, info vfs.ClientInfo) (vfs.Proposal, error) {
	if err := f.at(ctx, vfs.OpPropose); err != nil {
		return vfs.Proposal{}, err
	}
	return f.inner.Propose(ctx, c, info)
}

func (f *FaultService) Connect(ctx context.Context, c vfs.Call, info vfs.ClientInfo, cb vfs.Callbacks) (vfs.ClientID, uint64, error) {
	if err := f.at(ctx, vfs.OpConnect); err != nil {
		return 0, 0, err
	}
	return f.inner.Connect(ctx, c, info, cb)
}

func (f *FaultService) AddSession(ctx context.Context, c vfs.Call, cl vfs.ClientID, prev []byte, next []byte) (vfs.ClientRecord, error) {
	if err := f.at(ctx, vfs.OpAddSession); err != nil {
		return vfs.ClientRecord{}, err
	}
	return f.inner.AddSession(ctx, c, cl, prev, next)
}

func (f *FaultService) Record(ctx context.Context, c vfs.Call, cl vfs.ClientID) (vfs.ClientRecord, error) {
	if err := f.at(ctx, vfs.OpRecord); err != nil {
		return vfs.ClientRecord{}, err
	}
	return f.inner.Record(ctx, c, cl)
}

func (f *FaultService) Renew(ctx context.Context, c vfs.Call, cl vfs.ClientID) (vfs.ClientStatus, error) {
	if err := f.at(ctx, vfs.OpRenew); err != nil {
		return vfs.ClientStatus{}, err
	}
	return f.inner.Renew(ctx, c, cl)
}

func (f *FaultService) Disconnect(ctx context.Context, c vfs.Call, cl vfs.ClientID, s vfs.SessionRef, end vfs.SessionEnd) (uint64, error) {
	if err := f.at(ctx, vfs.OpDisconnect); err != nil {
		return 0, err
	}
	return f.inner.Disconnect(ctx, c, cl, s, end)
}

func (f *FaultService) Destroy(ctx context.Context, c vfs.Call, cl vfs.ClientID) error {
	if err := f.at(ctx, vfs.OpDestroy); err != nil {
		return err
	}
	return f.inner.Destroy(ctx, c, cl)
}

func (f *FaultService) ReclaimComplete(ctx context.Context, c vfs.Call, cl vfs.ClientID, shown []vfs.GraceRef) error {
	if err := f.at(ctx, vfs.OpReclaimComplete); err != nil {
		return err
	}
	return f.inner.ReclaimComplete(ctx, c, cl, shown)
}

func (f *FaultService) FreeState(ctx context.Context, c vfs.Call, cl vfs.ClientID, s vfs.StateRef) error {
	if err := f.at(ctx, vfs.OpFreeState); err != nil {
		return err
	}
	return f.inner.FreeState(ctx, c, cl, s)
}

func (f *FaultService) TestState(ctx context.Context, c vfs.Call, cl vfs.ClientID, s []vfs.StateRef) []error {
	if err := f.at(ctx, vfs.OpTestState); err != nil {
		return failEach(s, err)
	}
	return f.inner.TestState(ctx, c, cl, s)
}

func (f *FaultService) ReleaseLockOwner(ctx context.Context, c vfs.Call, cl vfs.ClientID, o vfs.LockOwner) error {
	if err := f.at(ctx, vfs.OpReleaseLockOwner); err != nil {
		return err
	}
	return f.inner.ReleaseLockOwner(ctx, c, cl, o)
}

func (f *FaultService) HostRestarted(ctx context.Context, c vfs.Call, host []byte, state uint32) error {
	if err := f.at(ctx, vfs.OpHostRestarted); err != nil {
		return err
	}
	return f.inner.HostRestarted(ctx, c, host, state)
}

func (f *FaultService) Lookup(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte) (vfs.Handle, vfs.File, error) {
	if err := f.at(ctx, vfs.OpLookup); err != nil {
		return nil, vfs.File{}, err
	}
	return f.inner.Lookup(ctx, c, dir, name)
}

func (f *FaultService) ReadDir(ctx context.Context, c vfs.Call, dir vfs.Handle, after vfs.Cursor, max int, want vfs.EntryFields) (vfs.Page, error) {
	if err := f.at(ctx, vfs.OpReadDir); err != nil {
		return vfs.Page{}, err
	}
	return f.inner.ReadDir(ctx, c, dir, after, max, want)
}

func (f *FaultService) Create(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte, a vfs.CreateArgs) (vfs.CreateResult, error) {
	if err := f.at(ctx, vfs.OpCreate); err != nil {
		return vfs.CreateResult{}, err
	}
	return f.inner.Create(ctx, c, dir, name, a)
}

func (f *FaultService) Link(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte, target vfs.Handle) (vfs.CreateResult, error) {
	if err := f.at(ctx, vfs.OpLink); err != nil {
		return vfs.CreateResult{}, err
	}
	return f.inner.Link(ctx, c, dir, name, target)
}

func (f *FaultService) Unlink(ctx context.Context, c vfs.Call, dir vfs.Target, name []byte) (vfs.UnlinkResult, error) {
	if err := f.at(ctx, vfs.OpUnlink); err != nil {
		return vfs.UnlinkResult{}, err
	}
	return f.inner.Unlink(ctx, c, dir, name)
}

func (f *FaultService) Rmdir(ctx context.Context, c vfs.Call, dir vfs.Target, name []byte) (vfs.UnlinkResult, error) {
	if err := f.at(ctx, vfs.OpRmdir); err != nil {
		return vfs.UnlinkResult{}, err
	}
	return f.inner.Rmdir(ctx, c, dir, name)
}

func (f *FaultService) Rename(ctx context.Context, c vfs.Call, from vfs.Target, fromName []byte, to vfs.Handle, toName []byte, noReplace bool) (vfs.RenameResult, error) {
	if err := f.at(ctx, vfs.OpRename); err != nil {
		return vfs.RenameResult{}, err
	}
	return f.inner.Rename(ctx, c, from, fromName, to, toName, noReplace)
}

func (f *FaultService) GetAttr(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.File, error) {
	if err := f.at(ctx, vfs.OpGetAttr); err != nil {
		return vfs.File{}, err
	}
	return f.inner.GetAttr(ctx, c, h)
}

func (f *FaultService) SetAttr(ctx context.Context, c vfs.Call, t vfs.Target, a vfs.AttrChange) (vfs.WCC, error) {
	if err := f.at(ctx, vfs.OpSetAttr); err != nil {
		return vfs.WCC{}, err
	}
	return f.inner.SetAttr(ctx, c, t, a)
}

func (f *FaultService) GetACL(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.ACL, error) {
	if err := f.at(ctx, vfs.OpGetACL); err != nil {
		return vfs.ACL{}, err
	}
	return f.inner.GetACL(ctx, c, h)
}

func (f *FaultService) SetACL(ctx context.Context, c vfs.Call, t vfs.Target, acl vfs.ACL) (vfs.WCC, error) {
	if err := f.at(ctx, vfs.OpSetACL); err != nil {
		return vfs.WCC{}, err
	}
	return f.inner.SetACL(ctx, c, t, acl)
}

func (f *FaultService) Xattrs(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.Xattr, error) {
	if err := f.at(ctx, vfs.OpXattrs); err != nil {
		return nil, err
	}
	return f.inner.Xattrs(ctx, c, h)
}

func (f *FaultService) SetXattr(ctx context.Context, c vfs.Call, t vfs.Target, x vfs.Xattr, m vfs.XattrMode) (vfs.WCC, error) {
	if err := f.at(ctx, vfs.OpSetXattr); err != nil {
		return vfs.WCC{}, err
	}
	return f.inner.SetXattr(ctx, c, t, x, m)
}

func (f *FaultService) RemoveXattr(ctx context.Context, c vfs.Call, t vfs.Target, name []byte) (vfs.WCC, error) {
	if err := f.at(ctx, vfs.OpRemoveXattr); err != nil {
		return vfs.WCC{}, err
	}
	return f.inner.RemoveXattr(ctx, c, t, name)
}

func (f *FaultService) Streams(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.File, error) {
	if err := f.at(ctx, vfs.OpStreams); err != nil {
		return nil, err
	}
	return f.inner.Streams(ctx, c, h)
}

func (f *FaultService) Access(ctx context.Context, c vfs.Call, h vfs.Handle, parent vfs.Handle, want vfs.Access) (vfs.Access, error) {
	if err := f.at(ctx, vfs.OpAccess); err != nil {
		return 0, err
	}
	return f.inner.Access(ctx, c, h, parent, want)
}

func (f *FaultService) Open(ctx context.Context, c vfs.Call, req vfs.OpenRequest) (vfs.OpenResult, error) {
	if err := f.at(ctx, vfs.OpOpen); err != nil {
		return vfs.OpenResult{}, err
	}
	return f.inner.Open(ctx, c, req)
}

func (f *FaultService) Downgrade(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs, access vfs.Access, deny vfs.Access) (uint32, error) {
	if err := f.at(ctx, vfs.OpDowngrade); err != nil {
		return 0, err
	}
	return f.inner.Downgrade(ctx, c, ref, q, access, deny)
}

func (f *FaultService) ConfirmOpen(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs) (uint32, error) {
	if err := f.at(ctx, vfs.OpConfirmOpen); err != nil {
		return 0, err
	}
	return f.inner.ConfirmOpen(ctx, c, ref, q)
}

func (f *FaultService) Close(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs) (*vfs.WCC, error) {
	if err := f.at(ctx, vfs.OpClose); err != nil {
		return nil, err
	}
	return f.inner.Close(ctx, c, ref, q)
}

func (f *FaultService) Read(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, dst []byte) (int, bool, error) {
	if err := f.at(ctx, vfs.OpRead); err != nil {
		return 0, false, err
	}
	return f.inner.Read(ctx, c, ref, off, dst)
}

func (f *FaultService) Write(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, src []byte, s vfs.Stability) (vfs.WriteResult, error) {
	if err := f.at(ctx, vfs.OpWrite); err != nil {
		return vfs.WriteResult{}, err
	}
	return f.inner.Write(ctx, c, ref, off, src, s)
}

func (f *FaultService) Commit(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, length int64) (vfs.Verifier, error) {
	if err := f.at(ctx, vfs.OpCommit); err != nil {
		return vfs.Verifier{}, err
	}
	return f.inner.Commit(ctx, c, ref, off, length)
}

func (f *FaultService) Deallocate(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, length int64) (vfs.WCC, error) {
	if err := f.at(ctx, vfs.OpDeallocate); err != nil {
		return vfs.WCC{}, err
	}
	return f.inner.Deallocate(ctx, c, ref, off, length)
}

func (f *FaultService) Seek(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, what vfs.SeekWhat) (int64, error) {
	if err := f.at(ctx, vfs.OpSeek); err != nil {
		return 0, err
	}
	return f.inner.Seek(ctx, c, ref, off, what)
}

func (f *FaultService) Copy(ctx context.Context, c vfs.Call, a vfs.CopyArgs) (vfs.CopyResult, error) {
	if err := f.at(ctx, vfs.OpCopy); err != nil {
		return vfs.CopyResult{}, err
	}
	return f.inner.Copy(ctx, c, a)
}

func (f *FaultService) Clone(ctx context.Context, c vfs.Call, a vfs.CopyArgs) (vfs.WCC, error) {
	if err := f.at(ctx, vfs.OpClone); err != nil {
		return vfs.WCC{}, err
	}
	return f.inner.Clone(ctx, c, a)
}

func (f *FaultService) Lock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, r vfs.LockReq) (vfs.StateRef, error) {
	if err := f.at(ctx, vfs.OpLock); err != nil {
		return vfs.StateRef{}, err
	}
	return f.inner.Lock(ctx, c, ref, r)
}

func (f *FaultService) CancelLock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, o vfs.LockOwner, r vfs.ByteRange) (bool, error) {
	if err := f.at(ctx, vfs.OpCancelLock); err != nil {
		return false, err
	}
	return f.inner.CancelLock(ctx, c, ref, o, r)
}

func (f *FaultService) TestLock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, o vfs.LockOwner, r vfs.ByteRange, exclusive bool) (*vfs.Lock, error) {
	if err := f.at(ctx, vfs.OpTestLock); err != nil {
		return nil, err
	}
	return f.inner.TestLock(ctx, c, ref, o, r, exclusive)
}

func (f *FaultService) Unlock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, o vfs.LockOwner, rs []vfs.ByteRange, q vfs.Seqs, replay *vfs.LockSequence) (uint32, error) {
	if err := f.at(ctx, vfs.OpUnlock); err != nil {
		return 0, err
	}
	return f.inner.Unlock(ctx, c, ref, o, rs, q, replay)
}

func (f *FaultService) AckRecall(ctx context.Context, c vfs.Call, g vfs.GrantID, to vfs.GrantKind) error {
	if err := f.at(ctx, vfs.OpAckRecall); err != nil {
		return err
	}
	return f.inner.AckRecall(ctx, c, g, to)
}

func (f *FaultService) Watch(ctx context.Context, c vfs.Call, w vfs.WatchSpec) (vfs.WatchID, error) {
	if err := f.at(ctx, vfs.OpWatch); err != nil {
		return vfs.WatchID{}, err
	}
	return f.inner.Watch(ctx, c, w)
}

func (f *FaultService) Unwatch(ctx context.Context, c vfs.Call, w vfs.WatchID) error {
	if err := f.at(ctx, vfs.OpUnwatch); err != nil {
		return err
	}
	return f.inner.Unwatch(ctx, c, w)
}

func (f *FaultService) Root(ctx context.Context, c vfs.Call, s vfs.ShareRef) (vfs.Handle, vfs.ShareInfo, error) {
	if err := f.at(ctx, vfs.OpRoot); err != nil {
		return nil, vfs.ShareInfo{}, err
	}
	return f.inner.Root(ctx, c, s)
}

func (f *FaultService) SecInfo(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte) ([]vfs.AuthFlavor, error) {
	if err := f.at(ctx, vfs.OpSecInfo); err != nil {
		return nil, err
	}
	return f.inner.SecInfo(ctx, c, dir, name)
}

func (f *FaultService) ListShares(ctx context.Context, c vfs.Call) ([]vfs.ShareInfo, error) {
	if err := f.at(ctx, vfs.OpListShares); err != nil {
		return nil, err
	}
	return f.inner.ListShares(ctx, c)
}

func (f *FaultService) StatFS(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.FilesystemInfo, error) {
	if err := f.at(ctx, vfs.OpStatFS); err != nil {
		return vfs.FilesystemInfo{}, err
	}
	return f.inner.StatFS(ctx, c, h)
}

func (f *FaultService) Quota(ctx context.Context, c vfs.Call, h vfs.Handle, p vfs.Principal) (vfs.Usage, vfs.Quota, error) {
	if err := f.at(ctx, vfs.OpQuota); err != nil {
		return vfs.Usage{}, vfs.Quota{}, err
	}
	return f.inner.Quota(ctx, c, h, p)
}

func (f *FaultService) Snapshots(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.SnapshotInfo, error) {
	if err := f.at(ctx, vfs.OpSnapshots); err != nil {
		return nil, err
	}
	return f.inner.Snapshots(ctx, c, h)
}

func (f *FaultService) SignIn(ctx context.Context, step vfs.SignInStep) (vfs.SignInResult, error) {
	if err := f.at(ctx, vfs.OpSignIn); err != nil {
		return vfs.SignInResult{}, err
	}
	return f.inner.SignIn(ctx, step)
}

func (f *FaultService) Spell(ctx context.Context, p vfs.Principal, as vfs.Scheme) (vfs.Spelling, error) {
	if err := f.at(ctx, vfs.OpSpell); err != nil {
		return vfs.Spelling{}, err
	}
	return f.inner.Spell(ctx, p, as)
}

func (f *FaultService) Find(ctx context.Context, c vfs.Call, s vfs.Spelling) (vfs.Principal, error) {
	if err := f.at(ctx, vfs.OpFind); err != nil {
		return vfs.Principal{}, err
	}
	return f.inner.Find(ctx, c, s)
}
