package kvstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
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

// A share with linked and open files carrying content, grants, share-scoped
// settings, a client's marks and a second shard is removed in batches of one
// row, each first crashed before its commit and the store restarted around
// it, and every row of it goes, in RFC 16 §2.3.2's order, while every row of
// another share stays.
func TestShareRemovalResumesAcrossRestarts(t *testing.T) {
	if testing.Short() {
		t.Skip("syncs every commit")
	}
	ctx := context.Background()
	d := newDiskStore(t)
	f := d.f
	cp := f.s.ControlPlane()
	admin, id, root := adminRoot(t, f)
	info, _, err := f.s.ShareIndex().Share(ctx, f.share)
	if err != nil {
		t.Fatal(err)
	}
	other := namedShare(t, f.s, "other")
	if _, err := cp.SetGrant(ctx, entity.ShareGrant{Share: other.ID, Principal: admin.Principal, Access: entity.AccessRead}, 0); err != nil {
		t.Fatal(err)
	}
	for _, s := range []entity.Setting{
		{Scope: shareScope(f.share), Key: "a", Value: []byte{1}},
		{Scope: shareScope(f.share), Key: "b", Value: []byte{2}},
		{Scope: shareScope(other.ID), Key: "a", Value: []byte{3}},
	} {
		if _, err := cp.PutSetting(ctx, s, 0); err != nil {
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
	if err := cp.DeleteShare(ctx, f.share, entity.SnapshotsDelete, info.Generation); err != nil {
		t.Fatal(err)
	}
	slMarked := f.counter(shareListKey)
	d.reopen()

	// The last pass deletes these four with st, raising ShareList, so they
	// stand or go together; the extra shard goes before them, and only once
	// the share's other records have.
	last := [][]byte{shareKey(f.share, keys.SInfo), namespacesKey(f.share), shardKey(uint64(info.Shard))}
	check := func() {
		t.Helper()
		f := d.f
		st := f.present(shareKey(f.share, keys.SSt))
		for _, k := range last {
			if f.present(k) != st {
				t.Fatalf("%s present %v while the state record is present %v", keys.Describe(k), !st, st)
			}
		}
		if want := slMarked + map[bool]uint64{true: 0, false: 1}[st]; f.counter(shareListKey) != want {
			t.Fatalf("ShareList with the state record present %v: got %d, want %d", st, f.counter(shareListKey), want)
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

	var batches int
	for done := false; !done; {
		var step uint8
		d.batch(func() (err error) { step, done, err = d.f.s.Nodes().ContinueRemoval(ctx, f.share); return err })
		check()
		switch step {
		case 3, 4:
			reached := map[uint8]entity.TagState{3: entity.TagOffloaded, 4: entity.TagForgotten}[step]
			for _, sh := range []entity.ShardID{info.Shard, extraShard} {
				if err := d.f.s.Nodes().RecordShardRemoval(ctx, f.share, sh, reached); err != nil {
					t.Fatalf("recording shard %d %v at step %d: %v", sh, reached, step, err)
				}
			}
		}
		if step == 4 {
			for _, sh := range []entity.ShardID{info.Shard, extraShard} {
				for shardDone := false; !shardDone; {
					d.batch(func() (err error) { shardDone, err = d.f.s.Nodes().ContinueShardRemoval(ctx, f.share, sh); return err })
					check()
					batches++
				}
			}
		}
		if batches++; batches > 200 {
			t.Fatal("the removal has not finished after 200 batches")
		}
	}
	f = d.f

	for name, prefix := range map[string][]byte{
		"share records": sharePrefix(f.share),
		"files":         keys.ID([]byte{keys.KindF}, f.share[:]),
		"root shard":    shardKey(uint64(info.Shard)),
		"extra shard":   shardKey(extraShard),
		"settings":      scopePrefix(shareScope(f.share)),
		"grant index":   grantRevKey(admin.Principal.ID, f.share),
		"client's mark": clientShardKey(client.ID, info.Shard),
		"name and path": pathKey(info.Path),
	} {
		if got := keysUnder(t, f.s, prefix); len(got) != 0 {
			t.Errorf("%s after the removal: %d keys, want none, first %s", name, len(got), keys.Describe(got[0]))
		}
	}
	for name, key := range map[string][]byte{
		"other share's grant":   grantKey(other.ID, admin.Principal.ID),
		"other share's index":   grantRevKey(admin.Principal.ID, other.ID),
		"other share's setting": settingKey(shareScope(other.ID), "a"),
		"client record":         clientKey(client.ID),
		"client's other mark":   clientShardKey(client.ID, other.Shard),
		"other share's state":   shareKey(other.ID, keys.SSt),
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

	// The share's tag and shard are never given again, and its removal,
	// asked again, is done.
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

	record(sh.Shard, entity.TagOffloaded, errRemovalOrder) // the removal is not at step 3 yet
	cont(3)
	cont(3) // no shard offloaded: the batch holds at the first
	record(sh.Shard, entity.TagForgotten, errRemovalOrder)
	drop(sh.Shard, errRemovalOrder)
	record(sh.Shard, entity.TagOffloaded, nil)
	cont(3) // passes the root's shard
	cont(3) // holds at the extra one
	record(sh.Shard, entity.TagForgotten, errRemovalOrder)
	record(extraShard, entity.TagOffloaded, nil)
	cont(4) // passes the extra shard, the last
	drop(sh.Shard, errRemovalOrder)
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

// An rm row's bytes: its step, and at step 4 the cursor the drop resumes
// after; a row of another step carries none.
func TestShardStepCodec(t *testing.T) {
	for _, c := range []struct {
		step   removalStep
		cursor []byte
		golden string
	}{
		{stepOffloaded, nil, "01" + "02"},
		{stepFour, []byte{0x01, 0x07}, "01" + "04" + "02" + "0107"},
		{stepDone, nil, "01" + "05"},
	} {
		b := encodeShardStep(c.step, c.cursor)
		if got := hex.EncodeToString(b); got != c.golden {
			t.Errorf("rm row at step %d: got %s, want %s", c.step, got, c.golden)
		}
		step, cursor, err := decodeShardStep(b)
		if err != nil || step != c.step || !bytes.Equal(cursor, c.cursor) {
			t.Errorf("rm row %s read back: step %d cursor %x, %v; want step %d cursor %x", c.golden, step, cursor, err, c.step, c.cursor)
		}
		if _, _, err := decodeShardStep(append(b, 0)); !errors.Is(err, codec.ErrTrailing) {
			t.Errorf("rm row %s with a trailing byte: %v, want %v", c.golden, err, codec.ErrTrailing)
		}
	}
}
