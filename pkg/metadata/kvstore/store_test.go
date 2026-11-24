package kvstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/google/uuid"
)

func memKV(t *testing.T) kv.KV {
	t.Helper()
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

func set(t *testing.T, db kv.KV, key, value []byte) {
	t.Helper()
	if err := db.Update(context.Background(), func(tx kv.Txn) error { return tx.Set(key, value) }); err != nil {
		t.Fatal(err)
	}
}

// Open over a store holding nothing but clock ceilings — its own Open's and
// another node's — answers it not initialised, never foreign.
func TestOpenOverCeilingsOnlyIsNotInitialised(t *testing.T) {
	db := memKV(t)
	other := make([]byte, keys.IDLen)
	other[0] = 0xCD
	set(t, db, append(keys.ID([]byte{keys.KindN}, other), keys.NClk), []byte("ceiling"))
	if _, err := Open(context.Background(), db); !errors.Is(err, ErrNotInitialised) {
		t.Fatalf("Open over a store holding only clock ceilings: %v, want ErrNotInitialised", err)
	}
}

// Open over a store holding a v1 Badger key and no format record refuses it
// as foreign rather than answering it not initialised, which would send the
// operator to the initialisation step, and commits nothing.
func TestOpenRefusesAForeignStore(t *testing.T) {
	ctx := context.Background()
	for _, key := range []string{"f:1", "p:x", "c:y", "lock:z"} {
		t.Run(key, func(t *testing.T) {
			db := openBadger(t, t.TempDir())
			set(t, db, []byte(key), []byte("v1 record"))
			before := keysHash(t, db)
			_, err := Open(ctx, db)
			if !errors.Is(err, ErrV1Store) || !errors.Is(err, ErrForeignStore) || errors.Is(err, ErrNotInitialised) {
				t.Fatalf("Open over a store holding the v1 key %q: %v, want ErrV1Store and ErrForeignStore", key, err)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("Open's refusal over %q: %v, want it to name the key", key, err)
			}
			if after := keysHash(t, db); after != before {
				t.Errorf("Open refused over %q committed records after it: hash %s after %s before, want nothing committed", key, after, before)
			}
		})
	}
}

func TestInitThenOpen(t *testing.T) {
	ctx := context.Background()
	db := memKV(t)
	install := uuid.New()
	if err := Init(ctx, db, install); err != nil {
		t.Fatal(err)
	}
	if err := Init(ctx, db, uuid.New()); !errors.Is(err, ErrInitialised) {
		t.Fatalf("second Init: %v", err)
	}
	// Open reads the format record and nothing else.
	c := &kv.Counting{KV: db}
	if _, err := Open(ctx, c); err != nil {
		t.Fatal(err)
	}
	if n := c.Take(); n != (kv.Counts{Gets: 1}) {
		t.Errorf("Open read %+v", n)
	}
	var in, sl []byte
	if err := db.View(ctx, func(r kv.Reader) error {
		var err error
		if in, err = r.Get(installationKey); err != nil {
			return err
		}
		sl, err = r.Get(shareListKey)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := decodeInstallation(in); err != nil || got.ID != install || got.Instance == got.ID {
		t.Errorf("installation %+v, %v", got, err)
	}
	if v, err := decodeCounter(sl); err != nil || v != 0 {
		t.Errorf("share list %d, %v", v, err)
	}
}

// Two racing initialisations: exactly one commits.
func TestInitRace(t *testing.T) {
	db := memKV(t)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = Init(context.Background(), db, uuid.New())
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrInitialised):
			t.Errorf("Init: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d initialisations committed", ok)
	}
}

func TestOpenRefusesFormat(t *testing.T) {
	for name, f := range map[string]format{
		"too new":      {Version: MaxFormat + 1, Folds: knownFolds},
		"too old":      {Version: MinFormat - 1, Folds: knownFolds},
		"unknown fold": {Version: MaxFormat, Folds: []uint32{uint32(entity.FoldIdentity), 99}},
	} {
		t.Run(name, func(t *testing.T) {
			db := memKV(t)
			set(t, db, keys.Format, encodeFormat(f))
			if _, err := Open(context.Background(), db); !errors.Is(err, ErrFormat) {
				t.Fatalf("Open: %v", err)
			}
		})
	}
}

// A format record that does not decode is a damaged store, refused as such:
// never ErrFormat, which sends the operator to a newer binary.
func TestOpenRefusesAnUndecodableFormatRecord(t *testing.T) {
	db := memKV(t)
	set(t, db, keys.Format, []byte{0xff})
	_, err := Open(context.Background(), db)
	if !errors.Is(err, ErrFormatRecord) || errors.Is(err, ErrFormat) {
		t.Fatalf("Open over an undecodable format record: %v, want ErrFormatRecord and not ErrFormat", err)
	}
}

func TestFormatGolden(t *testing.T) {
	if got := encodeFormat(format{Version: 1, Folds: []uint32{1}}); string(got) != "\x01"+"\x00\x00\x00\x00\x00\x00\x00\x01"+"\x01"+"\x00\x00\x00\x01" {
		t.Errorf("format record %x", got)
	}
}

// keysHash hashes every committed record, key and value in key order. The
// foreign-store refusal is decided inside Init's one transaction, so the
// committed records before and after are the whole of what could have
// changed.
func keysHash(t *testing.T, db *badgerkv.Store) string {
	t.Helper()
	h := sha256.New()
	if err := db.View(context.Background(), func(r kv.Reader) error {
		for kv, err := range r.Scan(nil, nil) {
			if err != nil {
				return err
			}
			h.Write(kv.Key)
			h.Write(kv.Value)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// openStore opens a real Badger store on a fresh directory, whose Open writes
// the node's clock ceiling and nothing else.
func openBadger(t *testing.T, dir string) *badgerkv.Store {
	t.Helper()
	db, err := badgerkv.Open(badgerkv.Options{Dir: dir, NodeID: [keys.IDLen]byte{0xAB}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

// Init over a store holding only the node's clock ceiling — what a fresh
// Open wrote — initialises it: the ceiling is the one row Init rewrites.
func TestInitOverOnlyTheCeiling(t *testing.T) {
	db := openBadger(t, t.TempDir())
	if err := Init(context.Background(), db, uuid.New()); err != nil {
		t.Fatalf("Init over a store holding only the clock ceiling: %v, want nil", err)
	}
}

// Init over a store whose format record is lost but whose records remain —
// a share's own File row is enough — refuses, committing nothing: a new
// Installation would otherwise be written over data it can no longer name.
func TestInitRefusesAStoreHoldingThisFormatsRecords(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := openBadger(t, dir)
	file := append(keys.ID([]byte{keys.KindF}, make([]byte, keys.IDLen)), make([]byte, keys.IDLen)...)
	if err := db.Update(ctx, func(tx kv.Txn) error {
		return tx.Set(file, []byte("a record the initialisation step does not own"))
	}); err != nil {
		t.Fatal(err)
	}
	before := keysHash(t, db)
	err := Init(ctx, db, uuid.New())
	if !errors.Is(err, ErrForeignStore) || errors.Is(err, ErrV1Store) {
		t.Fatalf("Init over a store holding a File row and no format record: %v, want ErrForeignStore, never ErrV1Store", err)
	}
	if after := keysHash(t, db); after != before {
		t.Errorf("Init refused over a File row committed records after it: hash %s after %s before, want the refusal to commit nothing", after, before)
	}
}

// Init over a store holding a v1 Badger key refuses naming it as a foreign
// store, writing nothing of its own into it.
func TestInitRefusesAV1Store(t *testing.T) {
	ctx := context.Background()
	for _, key := range []string{"f:1", "p:x", "c:y", "lock:z"} {
		t.Run(key, func(t *testing.T) {
			dir := t.TempDir()
			db := openBadger(t, dir)
			if err := db.Update(ctx, func(tx kv.Txn) error {
				return tx.Set([]byte(key), []byte("v1 record"))
			}); err != nil {
				t.Fatal(err)
			}
			before := keysHash(t, db)
			err := Init(ctx, db, uuid.New())
			if !errors.Is(err, ErrV1Store) || !errors.Is(err, ErrForeignStore) {
				t.Fatalf("Init over a store holding the v1 key %q: %v, want ErrV1Store and ErrForeignStore", key, err)
			}
			if after := keysHash(t, db); after != before {
				t.Errorf("Init refused over %q committed records after it: hash %s after %s before, want the refusal to commit nothing", key, after, before)
			}
		})
	}
}

// Open over a store Init created that has since lost its format record
// refuses it as foreign, never as v1's: its keys are this format's records.
func TestOpenRefusesAStoreThatLostItsFormatRecord(t *testing.T) {
	ctx := context.Background()
	db := openBadger(t, t.TempDir())
	if err := Init(ctx, db, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(ctx, func(tx kv.Txn) error { return tx.Delete(keys.Format) }); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, db); !errors.Is(err, ErrForeignStore) || errors.Is(err, ErrV1Store) {
		t.Fatalf("Open over a store that lost its format record: %v, want ErrForeignStore, never ErrV1Store", err)
	}
}
