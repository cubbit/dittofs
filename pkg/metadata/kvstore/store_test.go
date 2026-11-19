package kvstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

func TestOpenRefusesUninitialised(t *testing.T) {
	db := memKV(t)
	set(t, db, []byte("other"), []byte("x"))
	if _, err := Open(context.Background(), db); !errors.Is(err, ErrNotInitialised) {
		t.Fatalf("Open: %v", err)
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

func TestFormatGolden(t *testing.T) {
	if got := encodeFormat(format{Version: 1, Folds: []uint32{1}}); string(got) != "\x01"+"\x00\x00\x00\x00\x00\x00\x00\x01"+"\x01"+"\x00\x00\x00\x01" {
		t.Errorf("format record %x", got)
	}
}

// hashBadger hashes what a live Badger directory holds that a committed
// write or a restructure changes: every file's name and size, and the whole
// content of every file below a mebibyte — MANIFEST, KEYREGISTRY, DISCARD and
// sealed tables, against a rename, rewrite or new file. A live memtable
// write-ahead log's pages settle asynchronously, so its raw bytes hash
// differently between two idle checks; its growth is its size, covered, and
// the records it already holds are the committed ones keysHash reads.
func hashBadger(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("hash %s: %v", dir, err)
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatalf("hash %s: %v", e.Name(), err)
		}
		h.Write([]byte(e.Name()))
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(fi.Size()))
		h.Write(size[:])
		if fi.Size() < 1<<20 {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("hash %s: %v", e.Name(), err)
			}
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// keysHash hashes every committed record, key and value in key order.
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

// settledHash waits until the store's directory stops changing on its own —
// the page-out of the live memtable's writes — and returns its hash with the
// committed records' hash. The poll is bounded: a directory that never settles
// fails the test, never hangs it.
func settledHash(t *testing.T, dir string, db *badgerkv.Store) string {
	t.Helper()
	const (
		poll   = 50 * time.Millisecond
		bound  = 30 * time.Second
		steady = 4 // consecutive equal hashes, far past the moment the settle lands
	)
	start := time.Now()
	prev, same := "", 0
	for same < steady {
		if cur := hashBadger(t, dir); cur == prev {
			same++
		} else {
			prev, same = cur, 0
		}
		if time.Since(start) > bound {
			t.Fatalf("the store directory in %s did not settle within %s: last hash %s", dir, bound, prev)
		}
		time.Sleep(poll)
	}
	return prev + "/" + keysHash(t, db)
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
// Open wrote — initialises it: the ceiling is this format's own kind.
func TestInitOverOnlyTheCeiling(t *testing.T) {
	db := openBadger(t, t.TempDir())
	if err := Init(context.Background(), db, uuid.New()); err != nil {
		t.Fatalf("Init over a store holding only the clock ceiling: %v, want nil", err)
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
			before := settledHash(t, dir, db)
			err := Init(ctx, db, uuid.New())
			if !errors.Is(err, ErrForeignStore) {
				t.Fatalf("Init over a store holding the v1 key %q: %v, want ErrForeignStore", key, err)
			}
			if after := hashBadger(t, dir) + "/" + keysHash(t, db); after != before {
				t.Errorf("Init refused over %q wrote into the store: hash %s after %s before, want unchanged", key, after, before)
			}
		})
	}
}
