package engine

import (
	"context"
	"fmt"
	"iter"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

var (
	_ Existence = (*StubStore)(nil)
	_ Content   = (*StubStore)(nil)
	_ Lifecycle = (*StubStore)(nil)
)

// StubStore is block metadata for the tests that run in a testing/synctest
// bubble, where the real store cannot: its in-memory badger keeps goroutines
// with tickers, so a bubble never goes idle and a hang is never reported. It
// answers what an empty store answers, no floor, no limit and a first
// incarnation, and holds nothing. It promises no more than the interfaces
// Deps declares: a call outside what a test uses answers an error, never a
// panic.
type StubStore struct {
	// RaiseErr is what the raise at start and at attach answer, when set.
	RaiseErr error

	// Commits counts the existence commits asked of the store.
	Commits atomic.Int32
}

// NewStubStore is a StubStore that fails nothing.
func NewStubStore() *StubStore {
	return &StubStore{}
}

// unused is the answer of a method a test does not use.
func (*StubStore) unused(name string) error {
	return fmt.Errorf("engine: stub store: %s is not part of the stub", name)
}

func (s *StubStore) VersionFloor(context.Context, []entity.ShareID) (entity.JournalVersion, error) {
	return entity.JournalVersion{}, nil
}

func (s *StubStore) VersionLimits(
	context.Context,
	[]entity.ShareID,
) (map[entity.ShareID]entity.JournalVersion, error) {
	return map[entity.ShareID]entity.JournalVersion{}, nil
}

func (s *StubStore) RaiseVersionLimits(
	context.Context,
	map[entity.ShareID]entity.JournalVersion,
) error {
	return nil
}

func (s *StubStore) RaiseAtStart(
	_ context.Context,
	shards []entity.ShardID,
) (map[entity.ShardID]uint64, error) {
	if s.RaiseErr != nil {
		return nil, s.RaiseErr
	}

	out := map[entity.ShardID]uint64{}

	for _, shard := range shards {
		out[shard] = 1
	}

	return out, nil
}

func (s *StubStore) RaiseAtAttach(context.Context, entity.ShardID) (uint64, error) {
	if s.RaiseErr != nil {
		return 0, s.RaiseErr
	}

	return 1, nil
}

func (s *StubStore) JournalID(context.Context, entity.NodeID, entity.JournalDir) ([16]byte, error) {
	return [16]byte{}, s.unused("JournalID")
}

func (s *StubStore) Tags(context.Context) (map[entity.ShareID]entity.TagRecord, error) {
	return nil, s.unused("Tags")
}

func (s *StubStore) Covering(
	context.Context,
	entity.ShareID,
	entity.FileID,
	int64,
	int64,
) iter.Seq2[entity.Span, error] {
	return func(yield func(entity.Span, error) bool) {
		yield(entity.Span{}, s.unused("Covering"))
	}
}

func (s *StubStore) Resume(context.Context, entity.ShareID, entity.FileID) (bool, error) {
	return false, s.unused("Resume")
}

func (s *StubStore) PruneRemovals(
	context.Context,
	entity.ShareID,
	entity.FileID,
	entity.JournalVersion,
) error {
	return s.unused("PruneRemovals")
}

func (s *StubStore) UnfinishedRemovals(
	context.Context,
	entity.ShareID,
	uint64,
) iter.Seq2[kvstore.UnfinishedRemoval, error] {
	return func(yield func(kvstore.UnfinishedRemoval, error) bool) {
		yield(kvstore.UnfinishedRemoval{}, s.unused("UnfinishedRemovals"))
	}
}

func (s *StubStore) Commit(
	context.Context,
	[]kvstore.PendingExistence,
	*kvstore.MarksWrite,
) (map[entity.FileID]error, error) {
	s.Commits.Add(1)

	return nil, s.unused("Commit")
}

func (s *StubStore) Overwrites(
	context.Context,
	entity.ShareID,
	entity.FileID,
	[]entity.Extent,
) (bool, error) {
	return false, s.unused("Overwrites")
}

func (s *StubStore) Applied(
	context.Context,
	entity.ShareID,
	entity.FileID,
) (entity.JournalVersion, error) {
	return entity.JournalVersion{}, s.unused("Applied")
}

func (s *StubStore) Size(context.Context, entity.ShareID, entity.FileID) (int64, error) {
	return 0, s.unused("Size")
}

func (s *StubStore) Allocation(
	context.Context,
	entity.ShareID,
	entity.FileID,
	int64,
) (entity.Extent, bool, error) {
	return entity.Extent{}, false, s.unused("Allocation")
}

func (s *StubStore) Remove(
	context.Context,
	entity.ShareID,
	entity.FileID,
	uint64,
	kvstore.RemoveOp,
	[]kvstore.PendingOp,
) error {
	return s.unused("Remove")
}

func (s *StubStore) Clone(context.Context, kvstore.CloneOp, []kvstore.PendingOp) error {
	return s.unused("Clone")
}

func (s *StubStore) CloneFree(context.Context, kvstore.CloneOp) error {
	return s.unused("CloneFree")
}
