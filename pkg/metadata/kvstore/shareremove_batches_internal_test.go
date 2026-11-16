package kvstore

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/sim"
)

// oneItem is a key budget that lets every removal batch take one row.
const oneItem = batchItemKeys + 2

// extraShard is a second shard the tests give a share by its rows, as a
// share has only its root's today, so a removal walks a shard list longer
// than one.
const extraShard = 99

func addShard(t *testing.T, s *Store, share entity.ShareID) {
	t.Helper()
	rows(t, s, keys.Uint64(shardsPrefix(share), extraShard), encodeEmpty(),
		shardKey(extraShard), encodeShard(entity.Shard{ID: extraShard}),
		append(shardKey(extraShard), keys.SHHw), []byte{1})
}

// diskStore is a store on disk, closed and opened again over its data by
// reopen as a restart does, under a fault wrapper that can fail a write
// beneath the store.
type diskStore struct {
	t      *testing.T
	dir    string
	db     *badgerkv.Store
	faults *kvtest.FaultKV
	f      *cfix
}

func newDiskStore(t *testing.T) *diskStore {
	d := &diskStore{t: t, dir: t.TempDir()}
	t.Cleanup(func() {
		if err := d.db.Close(); err != nil {
			t.Error(err)
		}
	})
	d.reopen()
	return d
}

func (d *diskStore) reopen() {
	d.t.Helper()
	ctx := context.Background()
	if d.db != nil {
		if err := d.db.Close(); err != nil {
			d.t.Fatal(err)
		}
	}
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	db, err := badgerkv.Open(badgerkv.Options{Dir: d.dir, Clock: clk.now})
	if err != nil {
		d.t.Fatal(err)
	}
	d.db, d.faults = db, kvtest.NewFaultKV(db)
	if d.f == nil {
		if err := Init(ctx, d.faults, uuid.New()); err != nil {
			d.t.Fatal(err)
		}
	}
	s, err := Open(ctx, d.faults)
	if err != nil {
		d.t.Fatal(err)
	}
	if d.f == nil {
		d.f = &cfix{t: d.t, s: s, clk: clk, share: newShare(d.t, s)}
		d.f.ns = d.f.namespaces().Namespaces[0].Namespace
	}
	s.SetKeyBudget(oneItem)
	d.f.s, d.f.clk = s, clk
}

func (f *cfix) namespaces() shareNamespaces {
	f.t.Helper()
	var nss shareNamespaces
	if err := f.s.kv.View(context.Background(), func(r kv.Reader) (err error) {
		nss, err = shareNamespacesOf(r, f.share)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return nss
}

var errCrash = errors.New("the node crashed before the batch committed")

// batch runs one batch twice: first with its first write failed beneath the
// store, so the batch commits nothing, as a crash before its commit leaves
// it, then, after a restart, for real. It restarts again after. A batch with
// nothing left to write runs once.
func (d *diskStore) batch(call func() error) {
	d.t.Helper()
	sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: kvtest.OpSet, Key: "*", Count: 1, Action: sim.FaultAction{Err: errCrash}})
	if err != nil {
		d.t.Fatal(err)
	}
	d.faults.SetFault(sched)
	switch err := call(); {
	case errors.Is(err, errCrash):
		d.reopen()
		if err := call(); err != nil {
			d.t.Fatal(err)
		}
	case err != nil:
		d.t.Fatal(err)
	}
	d.reopen()
}

func (f *cfix) present(key []byte) bool {
	f.t.Helper()
	var ok bool
	if err := f.s.kv.View(context.Background(), func(r kv.Reader) (err error) {
		_, ok, err = get(r, key)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return ok
}

func (f *cfix) counter(key []byte) uint64 {
	f.t.Helper()
	v, err := counter(context.Background(), f.s.kv, key)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

// removing reports whether the share's removal is under way: its state
// record says being removed, not retired or absent.
func (f *cfix) removing(share entity.ShareID) bool {
	f.t.Helper()
	var st shareState
	if err := f.s.kv.View(context.Background(), func(r kv.Reader) error {
		b, ok, err := get(r, shareKey(share, keys.SSt))
		if err != nil || !ok {
			return err
		}
		st, err = decodeState(b)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return st.State == entity.ShareRemoving
}

// removeInBatches is the whole removal of a share, made after the share the
// fixture starts with, through batches of one row. Each batch runs through
// batch, which may crash and restart around it. The share has linked and
// open files carrying content, grants, share-scoped settings, a client's
// marks and a second shard; every row of it goes, in RFC 16 §2.3.2's order,
// while every row of the other two shares stays.
func removeInBatches(t *testing.T, f *cfix, batch func(call func() error)) {
	ctx := context.Background()
	first, _, err := f.s.ShareIndex().Share(ctx, f.share)
	if err != nil {
		t.Fatal(err)
	}
	info := namedShare(t, f.s, "gone")
	f.share = info.ID
	f.ns = f.namespaces().Namespaces[0].Namespace
	admin, id, root := adminRoot(t, f)
	info, _, err = f.s.ShareIndex().Share(ctx, f.share)
	if err != nil {
		t.Fatal(err)
	}
	if info.Shard == first.Shard || info.Shard > extraShard {
		t.Fatalf("the removed share's shard %d, want one above the first share's %d and below %d", info.Shard, first.Shard, extraShard)
	}
	other := namedShare(t, f.s, "other")
	if _, err := f.s.ControlPlane().SetGrant(ctx, entity.ShareGrant{Share: other.ID, Principal: admin.Principal, Access: entity.AccessRead}, 0); err != nil {
		t.Fatal(err)
	}
	for _, s := range []entity.Setting{
		{Scope: shareScope(f.share), Key: "a", Value: []byte{1}},
		{Scope: shareScope(f.share), Key: "b", Value: []byte{2}},
		{Scope: shareScope(other.ID), Key: "a", Value: []byte{3}},
	} {
		if _, err := f.s.ControlPlane().PutSetting(ctx, s, 0); err != nil {
			t.Fatal(err)
		}
	}
	client := entity.ClientRecord{ID: 1, Protocol: entity.ProtocolNFSv4, Name: []byte("c")}
	if err := f.s.Clients().Create(ctx, client); err != nil {
		t.Fatal(err)
	}
	for _, sh := range []entity.ShardID{info.Shard, other.Shard} {
		if err := f.s.Clients().MarkShard(ctx, client.ID, entity.ShardMark{Shard: sh}); err != nil {
			t.Fatal(err)
		}
	}

	// Two linked files with content, one of them open, and a directory.
	_, linked, err := f.s.Namespace().Create(ctx, id, root, []byte("a"), NewFile{Type: entity.Regular, Mode: 0o644})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.Namespace().Create(ctx, id, root, []byte("d"), NewFile{Type: entity.Directory, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	big := f.file(3 * mib)
	small := linked.ID
	if err := f.s.kv.Update(ctx, func(tx kv.Txn) error {
		x, err := fileIn(tx, f.share, small)
		if err != nil {
			return err
		}
		was := x
		x.Size = mib
		return putFile(tx, was.Version, x)
	}); err != nil {
		t.Fatal(err)
	}
	blk, _ := f.commit(big, carry(chunk(1), chunk(2), chunk(3)), ref(0, mib, chunk(1), 1, 1), ref(mib, mib, chunk(2), 1, 1), ref(2*mib, mib, chunk(3), 1, 1))
	shared, _ := f.commit(small, carry(chunk(4)), ref(0, mib, chunk(4), 1, 1))
	if err := f.s.Opens().Put(ctx, f.share, entity.OpenRecord{ID: 7, File: small, Client: client.ID, Principal: admin.Principal, Durability: entity.DurabilityPersistent}); err != nil {
		t.Fatal(err)
	}
	addShard(t, f.s, f.share)
	if err := f.s.Check(ctx); err != nil {
		t.Fatalf("the store before the removal: %v", err)
	}

	cfgBefore := f.counter(cfgGenKey)
	batch(func() error {
		return f.s.ControlPlane().DeleteShare(ctx, f.share, entity.SnapshotsDelete, info.Generation)
	})
	slMarked := f.counter(shareListKey)

	// The last pass deletes these with st's removal, raising ShareList, so they
	// stand or go together; the extra shard goes before them, and only once
	// the share's other records have.
	last := [][]byte{shareKey(f.share, keys.SInfo), namespacesKey(f.share), shardKey(uint64(info.Shard)), namespaceShareKey(f.ns, f.share)}
	check := func() {
		t.Helper()
		removing := f.removing(f.share)
		for _, k := range last {
			if f.present(k) != removing {
				t.Fatalf("%s present %v while the removal is under way %v", keys.Describe(k), !removing, removing)
			}
		}
		if want := slMarked + map[bool]uint64{true: 0, false: 1}[removing]; f.counter(shareListKey) != want {
			t.Fatalf("ShareList with the removal under way %v: got %d, want %d", removing, f.counter(shareListKey), want)
		}
		if f.present(shardKey(extraShard)) {
			return
		}
		for _, k := range keysUnder(t, f.s, sharePrefix(f.share)) {
			if !bytes.HasPrefix(k, shardsPrefix(f.share)) && !bytes.Equal(k, shareKey(f.share, keys.SSt)) &&
				!slices.ContainsFunc(last, func(l []byte) bool { return bytes.Equal(l, k) }) {
				t.Fatalf("%s is left once the extra shard is gone", keys.Describe(k))
			}
		}
	}

	var limit []byte
	var batches int
	for done := false; !done; {
		var step uint8
		batch(func() (err error) { step, done, err = f.s.Nodes().ContinueRemoval(ctx, f.share); return err })
		check()
		switch step {
		case 3, 4:
			reached := map[uint8]entity.TagState{3: entity.TagOffloaded, 4: entity.TagForgotten}[step]
			for _, sh := range []entity.ShardID{info.Shard, extraShard} {
				if err := f.s.Nodes().RecordShardRemoval(ctx, f.share, sh, reached); err != nil {
					t.Fatalf("recording shard %d %v at step %d: %v", sh, reached, step, err)
				}
			}
		}
		if step == 4 {
			if limit == nil {
				limit = append([]byte{}, f.limit()...)
			}
			for _, sh := range []entity.ShardID{info.Shard, extraShard} {
				for shardDone := false; !shardDone; {
					batch(func() (err error) { shardDone, err = f.s.Nodes().ContinueShardRemoval(ctx, f.share, sh); return err })
					check()
					if batches++; batches > 200 {
						t.Fatalf("shard %d's files are not dropped after 200 batches", sh)
					}
				}
			}
			if got := f.limit(); !bytes.Equal(got, limit) {
				t.Fatalf("the share's version limit across step 4: got %x, want %x, as the drop takes no journal version", got, limit)
			}
		}
		if batches++; batches > 200 {
			t.Fatal("the removal has not finished after 200 batches")
		}
	}

	if got := keysUnder(t, f.s, sharePrefix(f.share)); len(got) != 1 || !bytes.Equal(got[0], shareKey(f.share, keys.SSt)) || f.removing(f.share) {
		t.Errorf("share records after the removal: %d keys, want the retired state record alone", len(got))
	}
	for name, prefix := range map[string][]byte{
		"files":           keys.ID([]byte{keys.KindF}, f.share[:]),
		"root shard":      shardKey(uint64(info.Shard)),
		"extra shard":     shardKey(extraShard),
		"settings":        scopePrefix(shareScope(f.share)),
		"grant index":     grantRevKey(admin.Principal.ID, f.share),
		"client's mark":   clientShardKey(client.ID, info.Shard),
		"name and path":   pathKey(info.Path),
		"namespace index": namespaceShareKey(f.ns, f.share),
	} {
		if got := keysUnder(t, f.s, prefix); len(got) != 0 {
			t.Errorf("%s after the removal: %d keys, want none, first %s", name, len(got), keys.Describe(got[0]))
		}
	}
	for name, key := range map[string][]byte{
		"other share's grant":       grantKey(other.ID, admin.Principal.ID),
		"other share's index":       grantRevKey(admin.Principal.ID, other.ID),
		"other share's setting":     settingKey(shareScope(other.ID), "a"),
		"client record":             clientKey(client.ID),
		"client's other mark":       clientShardKey(client.ID, other.Shard),
		"other share's state":       shareKey(other.ID, keys.SSt),
		"first share's shard":       shardKey(uint64(first.Shard)),
		"first share's shard row":   keys.Uint64(shardsPrefix(first.ID), uint64(first.Shard)),
		"first share's info":        shareKey(first.ID, keys.SInfo),
		"other share's shard":       shardKey(uint64(other.Shard)),
		"other share's shard row":   keys.Uint64(shardsPrefix(other.ID), uint64(other.Shard)),
		"other share's namespaces":  namespacesKey(other.ID),
		"first share's namespaces":  namespacesKey(first.ID),
		"first share's root's file": fileKey(first.ID, first.Root),
	} {
		if !f.present(key) {
			t.Errorf("%s is gone after another share's removal", name)
		}
	}
	if got := f.counter(cfgGenKey); got <= cfgBefore {
		t.Errorf("settings generation after the share's settings went: got %d, want above %d", got, cfgBefore)
	}

	// Every ref's count is down and every block retired.
	for _, h := range []entity.ChunkHash{chunk(1), chunk(2), chunk(3), chunk(4)} {
		if rec, ok := f.chunkRec(h); !ok || rec.Refcount != 0 {
			t.Errorf("chunk %x after its refs went: %+v, %v; want a record at count 0", h[31], rec, ok)
		}
		if got := keysUnder(t, f.s, reversePrefix(f.ns, h)); len(got) != 0 {
			t.Errorf("chunk %x's reverse refs after its refs went: %d, want none", h[31], len(got))
		}
	}
	for _, name := range []entity.BlockName{blk, shared} {
		if b, ok := f.block(name); !ok || b.State != entity.BlockRetired || b.Live != 0 {
			t.Errorf("block %x after its chunks' counts fell to zero: %+v, %v; want retired, live 0", name[31], b, ok)
		}
	}
	if err := f.s.Check(ctx); err != nil {
		t.Errorf("the store after the removal: %v", err)
	}

	// The share's ID, tag and shard are never given again, its settings are
	// refused, and its removal, asked again, is done.
	again := entity.Share{ID: f.share, Name: "again", Path: "/again", Root: entity.NewFileID(),
		Namespaces: []entity.NamespaceGen{{Generation: 1, Namespace: f.ns}}}
	if _, err := f.s.ControlPlane().PutShare(ctx, again, 0); !errors.Is(err, entity.ErrStale) {
		t.Errorf("PutShare naming the removed share's ID: %v, want %v", err, entity.ErrStale)
	}
	if _, err := f.s.ControlPlane().PutSetting(ctx, entity.Setting{Scope: shareScope(f.share), Key: "c", Value: []byte{4}}, 0); !errors.Is(err, entity.ErrStale) {
		t.Errorf("PutSetting on the removed share: %v, want %v", err, entity.ErrStale)
	}
	if got := keysUnder(t, f.s, scopePrefix(shareScope(f.share))); len(got) != 0 {
		t.Errorf("settings of the removed share after a refused put: %d, want none", len(got))
	}
	next := namedShare(t, f.s, "next")
	if next.Tag == info.Tag || next.Shard == info.Shard {
		t.Errorf("the next share's tag %d and shard %d, want neither the removed share's %d and %d", next.Tag, next.Shard, info.Tag, info.Shard)
	}
	nodes := f.s.Nodes()
	if step, done, err := nodes.ContinueRemoval(ctx, f.share); err != nil || !done || step != 0 {
		t.Errorf("ContinueRemoval after the removal: step %d, done %v, %v; want done", step, done, err)
	}
	if done, err := nodes.ContinueShardRemoval(ctx, f.share, info.Shard); err != nil || !done {
		t.Errorf("ContinueShardRemoval after the removal: done %v, %v; want done", done, err)
	}
	if err := nodes.RecordShardRemoval(ctx, f.share, info.Shard, entity.TagForgotten); err != nil {
		t.Errorf("RecordShardRemoval after the removal: %v, want nil", err)
	}
}

// limit is the raw version limit of the fixture's share, nil for none.
func (f *cfix) limit() []byte {
	f.t.Helper()
	var b []byte
	if err := f.s.kv.View(context.Background(), func(r kv.Reader) (err error) {
		b, _, err = get(r, versionLimitKey(f.share))
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return b
}

func TestShareRemovalInBatches(t *testing.T) {
	f := newContentFix(t, nil)
	f.s.SetKeyBudget(oneItem)
	removeInBatches(t, f, func(call func() error) {
		t.Helper()
		if err := call(); err != nil {
			t.Fatal(err)
		}
	})
}

// The same removal on disk, each batch first crashed before its commit and
// the store restarted around it.
func TestShareRemovalResumesAcrossRestarts(t *testing.T) {
	if testing.Short() {
		t.Skip("syncs every commit")
	}
	d := newDiskStore(t)
	removeInBatches(t, d.f, d.batch)
}

// removalAt drives a share's removal on a store of one shard to its files'
// drop: every shard offloaded and forgotten.
func removalAt4(t *testing.T, s *Store, sh entity.Share) {
	t.Helper()
	ctx := context.Background()
	if err := s.ControlPlane().DeleteShare(ctx, sh.ID, entity.SnapshotsDelete, sh.Generation); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		want    uint8
		reached entity.TagState
	}{{3, entity.TagOffloaded}, {4, entity.TagForgotten}} {
		if step, _, err := s.Nodes().ContinueRemoval(ctx, sh.ID); err != nil || step != c.want {
			t.Fatalf("ContinueRemoval: step %d, %v; want %d", step, err, c.want)
		}
		if err := s.Nodes().RecordShardRemoval(ctx, sh.ID, sh.Shard, c.reached); err != nil {
			t.Fatal(err)
		}
	}
}

// Step 4 drops a shard's files, and step 5 a share's grants, a batch's worth
// at a time: with room for one row a batch, the first leaves the rest.
func TestShareRemovalBatchesAreBounded(t *testing.T) {
	ctx := context.Background()
	f := newContentFix(t, nil)
	f.s.SetKeyBudget(oneItem)
	a, b := f.file(0), f.file(0)
	if bytes.Compare(a[:], b[:]) > 0 {
		a, b = b, a
	}
	cp := f.s.ControlPlane()
	for _, p := range []entity.Principal{entity.AuthenticatedUsers, entity.Everyone} {
		if _, err := cp.SetGrant(ctx, entity.ShareGrant{Share: f.share, Principal: p, Access: entity.AccessRead}, 0); err != nil {
			t.Fatal(err)
		}
	}
	sh, _, err := f.s.ShareIndex().Share(ctx, f.share)
	if err != nil {
		t.Fatal(err)
	}
	removalAt4(t, f.s, sh)

	if done, err := f.s.Nodes().ContinueShardRemoval(ctx, f.share, sh.Shard); err != nil || done {
		t.Fatalf("the first drop batch: done %v, %v; want not done", done, err)
	}
	if got := keysUnder(t, f.s, fileKey(f.share, b)); len(got) == 0 {
		t.Errorf("the later file's keys after the first drop batch: none, want them left to a later batch")
	}
	for done := false; !done; {
		if done, err = f.s.Nodes().ContinueShardRemoval(ctx, f.share, sh.Shard); err != nil {
			t.Fatal(err)
		}
	}
	if step, _, err := f.s.Nodes().ContinueRemoval(ctx, f.share); err != nil || step != 5 {
		t.Fatalf("ContinueRemoval once the files are dropped: step %d, %v; want 5", step, err)
	}
	if step, _, err := f.s.Nodes().ContinueRemoval(ctx, f.share); err != nil || step != 5 {
		t.Fatalf("the first grants batch: step %d, %v; want 5", step, err)
	}
	if got := keysUnder(t, f.s, shareKey(f.share, keys.SG)); len(got) != 1 {
		t.Errorf("grants after the first grants batch: %d, want 1", len(got))
	}
}

// A share's removal drops only its own shards' files: naming another share's
// shard is refused, and changes neither share.
func TestShardRemovalRefusesAnotherSharesShard(t *testing.T) {
	ctx := context.Background()
	f := newContentFix(t, nil)
	f.file(0)
	sh, _, err := f.s.ShareIndex().Share(ctx, f.share)
	if err != nil {
		t.Fatal(err)
	}
	other := namedShare(t, f.s, "other")
	rows(t, f.s, shardRemovalKey(uint64(other.Shard)), encodeRemovalStep(stepForgotten))
	removalAt4(t, f.s, sh)
	files := keys.ID([]byte{keys.KindF}, f.share[:])
	before := len(keysUnder(t, f.s, files))

	if _, err := f.s.Nodes().ContinueShardRemoval(ctx, f.share, other.Shard); !merrs.IsNotFoundError(err) {
		t.Errorf("dropping another share's shard: %v, want not found", err)
	}
	if got := len(keysUnder(t, f.s, files)); got != before {
		t.Errorf("the share's file keys after the refused drop: %d, want %d", got, before)
	}
	var step removalStep
	if err := f.s.kv.View(ctx, func(r kv.Reader) (err error) { step, err = shardStepIn(r, uint64(other.Shard)); return err }); err != nil || step != stepForgotten {
		t.Errorf("the other shard's rm row after the refused drop: %d, %v; want %d", step, err, stepForgotten)
	}
}

// Step 3's batch reads each shard's rm row with tracking: an rm row written
// between the batch's reads and its commit sends it round again, and it holds
// at that shard rather than pass it.
func TestShareRemovalRereadsAnRmRowWrittenUnderIt(t *testing.T) {
	ctx := context.Background()
	clk := sim.NewTestClock(time.Unix(0, 0))
	var faults *kvtest.FaultKV
	s := openStoreOver(t, func(db kv.KV) kv.KV { faults = kvtest.NewFaultKV(db); return faults })
	sh := namedShare(t, s, "s")
	if err := s.ControlPlane().DeleteShare(ctx, sh.ID, entity.SnapshotsDelete, sh.Generation); err != nil {
		t.Fatal(err)
	}
	if step, _, err := s.Nodes().ContinueRemoval(ctx, sh.ID); err != nil || step != 3 {
		t.Fatalf("ContinueRemoval: step %d, %v; want 3", step, err)
	}
	if err := s.Nodes().RecordShardRemoval(ctx, sh.ID, sh.Shard, entity.TagOffloaded); err != nil {
		t.Fatal(err)
	}
	sched, err := sim.NewSchedule(1, clk, sim.Rule{Op: kvtest.OpSet, Key: keys.Describe(shareKey(sh.ID, keys.SSt)), Count: 1,
		Action: sim.FaultAction{Delay: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	faults.SetFault(sched)
	type result struct {
		step uint8
		err  error
	}
	got := make(chan result, 1)
	go func() {
		step, _, err := s.Nodes().ContinueRemoval(ctx, sh.ID)
		got <- result{step, err}
	}()
	// Bounded in real time: the clock is not advanced until the batch waits on it.
	for deadline := time.Now().Add(10 * time.Second); clk.Pending() < 1; runtime.Gosched() {
		if time.Now().After(deadline) {
			t.Fatal("the batch never reached its state record's write")
		}
	}
	rows(t, s, shardRemovalKey(uint64(sh.Shard)), encodeRemovalStep(stepThree))
	clk.Advance(time.Second)
	if r := <-got; r.err != nil || r.step != 3 {
		t.Errorf("the batch over an rm row moved back under it: step %d, %v; want 3", r.step, r.err)
	}
}

// A share one of whose files is the source of a clone not done onto another
// share's file is not marked for removal until the clone is done.
func TestDeleteShareWaitsForAnotherSharesClone(t *testing.T) {
	ctx := context.Background()
	f := newContentFix(t, nil)
	src := f.file(0)
	f.exist(src, write(1, 0, 4*mib))
	cp := f.s.ControlPlane()
	other := entity.Share{ID: entity.NewShareID(), Name: "other", Path: "/other", Root: entity.NewFileID(),
		Namespaces: []entity.NamespaceGen{{Generation: 1, Namespace: f.ns}}}
	if _, err := cp.PutShare(ctx, other, 0); err != nil {
		t.Fatal(err)
	}
	dst := entity.NewFileID()
	if err := f.s.kv.Update(ctx, func(tx kv.Txn) error {
		x := entity.File{ID: dst, Share: other.ID, Type: entity.Regular, Number: 9, Nlink: 1, Version: entity.VersionOf(1)}
		if err := putFile(tx, entity.JournalVersion{}, x); err != nil {
			return err
		}
		return chargeFile(tx, other.ID, nil, &x)
	}); err != nil {
		t.Fatal(err)
	}
	f.clone(CloneOp{Share: other.ID, File: dst, Version: entity.VersionOf(5), Time: time.Unix(1_900_000_005, 0),
		Spec: entity.CloneSpec{SrcShare: f.share, Src: src, Len: mib}})
	sh, _, err := f.s.ShareIndex().Share(ctx, f.share)
	if err != nil {
		t.Fatal(err)
	}

	if err := cp.DeleteShare(ctx, f.share, entity.SnapshotsDelete, sh.Generation); !merrs.IsConflictError(err) {
		t.Fatalf("removing a clone's source share while the clone runs: %v, want a conflict", err)
	}
	if _, err := f.s.Content().Resume(ctx, other.ID, dst); err != nil {
		t.Fatal(err)
	}
	if err := cp.DeleteShare(ctx, f.share, entity.SnapshotsDelete, sh.Generation); err != nil {
		t.Errorf("removing the source share once the clone is done: %v, want nil", err)
	}
}

// The removal stays at step 3 until every shard is offloaded, and at step 4
// until every shard is done, each batch passing the shards found there and
// stopping at the first that is not; a shard forgets, and drops its files,
// only once the removal has found every shard offloaded.
func TestShareRemovalWaitsForEveryShard(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	s.SetKeyBudget(oneItem)
	sh := namedShare(t, s, "s")
	addShard(t, s, sh.ID)
	if err := s.ControlPlane().DeleteShare(ctx, sh.ID, entity.SnapshotsDelete, sh.Generation); err != nil {
		t.Fatal(err)
	}
	nodes := s.Nodes()
	cont := func(want uint8) {
		t.Helper()
		if step, done, err := nodes.ContinueRemoval(ctx, sh.ID); err != nil || done || step != want {
			t.Fatalf("ContinueRemoval: step %d, done %v, %v; want step %d", step, done, err, want)
		}
	}
	record := func(shard entity.ShardID, reached entity.TagState, want error) {
		t.Helper()
		if err := nodes.RecordShardRemoval(ctx, sh.ID, shard, reached); !errors.Is(err, want) {
			t.Fatalf("recording shard %d %v: %v, want %v", shard, reached, err, want)
		}
	}
	drop := func(shard entity.ShardID, want error) {
		t.Helper()
		if _, err := nodes.ContinueShardRemoval(ctx, sh.ID, shard); !errors.Is(err, want) {
			t.Fatalf("dropping shard %d's files: %v, want %v", shard, err, want)
		}
	}

	record(sh.Shard, entity.TagOffloaded, ErrRemovalOrder) // the removal is not at step 3 yet
	cont(3)
	cont(3) // no shard offloaded: the batch holds at the first
	record(sh.Shard, entity.TagForgotten, ErrRemovalOrder)
	drop(sh.Shard, ErrRemovalOrder)
	record(sh.Shard, entity.TagOffloaded, nil)
	cont(3) // passes the root's shard
	cont(3) // holds at the extra one
	record(sh.Shard, entity.TagForgotten, ErrRemovalOrder)
	record(extraShard, entity.TagOffloaded, nil)
	cont(4) // passes the extra shard, the last
	drop(sh.Shard, ErrRemovalOrder)
	record(sh.Shard, entity.TagForgotten, nil)
	record(sh.Shard, entity.TagOffloaded, nil) // a step behind the one recorded changes nothing
	for done := false; !done; {
		var err error
		if done, err = nodes.ContinueShardRemoval(ctx, sh.ID, sh.Shard); err != nil {
			t.Fatal(err)
		}
	}
	cont(4) // passes the root's shard
	cont(4) // holds at the extra one, not yet forgotten
	record(extraShard, entity.TagForgotten, nil)
	if done, err := nodes.ContinueShardRemoval(ctx, sh.ID, extraShard); err != nil || !done {
		t.Fatalf("dropping the extra shard's files, none left: done %v, %v; want done", done, err)
	}
	cont(5)
}
