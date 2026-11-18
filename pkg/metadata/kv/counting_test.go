package kv_test

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
)

func TestCountingFlagsGuardOnlyUpdates(t *testing.T) {
	s, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	c := &kv.Counting{KV: s}
	ctx := context.Background()
	if err := c.Update(ctx, func(tx kv.Txn) error { return tx.Guard([]byte("k")) }); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, func(tx kv.Txn) error {
		if err := tx.GuardRange([]byte("p/")); err != nil {
			return err
		}
		return tx.Set([]byte("k"), []byte("v"))
	}); err != nil {
		t.Fatal(err)
	}
	if got := c.Take(); got.GuardOnly != 1 || got.Guards != 1 || got.RangeGuards != 1 || got.Sets != 1 {
		t.Fatalf("counts %+v, want one guard-only Update of two", got)
	}
}

// TakeUpdates counts the Updates, not the Views, and the most keys one of
// them set or deleted, and resets both.
func TestCountingCountsUpdatesAndTheMostKeysOneWrote(t *testing.T) {
	s, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()

	c := &kv.Counting{KV: s}
	ctx := context.Background()
	write := func(sets, deletes []string) func(kv.Txn) error {
		return func(tx kv.Txn) error {
			for _, k := range sets {
				if err := tx.Set([]byte(k), []byte("v")); err != nil {
					return err
				}
			}

			for _, k := range deletes {
				if err := tx.Delete([]byte(k)); err != nil {
					return err
				}
			}

			return nil
		}
	}

	for _, fn := range []func(kv.Txn) error{
		write([]string{"a", "b"}, []string{"c", "d"}),
		write([]string{"e", "f", "g"}, nil),
	} {
		if err := c.Update(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}

	if err := c.View(ctx, func(r kv.Reader) error {
		_, err := r.Get([]byte("a"))

		return err
	}); err != nil {
		t.Fatal(err)
	}

	want := kv.UpdateCounts{
		Updates:    2,
		MostWrites: 4,
	}

	if got := c.TakeUpdates(); got != want {
		t.Errorf("TakeUpdates() = %+v, want %+v", got, want)
	}

	if got := c.TakeUpdates(); got != (kv.UpdateCounts{}) {
		t.Errorf("TakeUpdates() after a take = %+v, want zero", got)
	}
}

// TakeReads counts each read call once, however many keys a GetMany names,
// and resets.
func TestCountingCountsReadCalls(t *testing.T) {
	s, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	c := &kv.Counting{KV: s}
	if err := c.Update(context.Background(), func(tx kv.Txn) error {
		if _, err := tx.GetMany([][]byte{[]byte("a"), []byte("b"), []byte("c")}); err != nil {
			return err
		}
		if _, err := tx.Get([]byte("a")); err != nil && err != kv.ErrNotFound {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := c.TakeReads(); got != 2 {
		t.Errorf("read calls for a GetMany of three keys and a Get: %d, want 2", got)
	}
	if got := c.TakeReads(); got != 0 {
		t.Errorf("read calls after TakeReads: %d, want 0", got)
	}
}
