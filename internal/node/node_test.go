package node_test

import (
	"cmp"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/google/uuid"
)

// New starts nothing: a node built and stopped without a Start leaves no
// goroutine, descriptor or heap behind, so no journal was opened and no
// component started.
func TestNewStartsNothing(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	if err := h.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop after New: got %v, want nil", err)
	}
}

// Stop closes the storage key, and answers its Close's error.
func TestStopClosesTheStorageKey(t *testing.T) {
	h := nodetest.New(t)
	boom := errors.New("the storage key did not close")
	h.Node().FailStorageKeyClose(boom)
	if err := h.Node().Stop(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("Stop with a storage key whose Close fails: got %v, want %v", err, boom)
	}
}

// Stop clears the bytes of every signing-key version the access tokens
// read.
func TestStopClearsTheSigningKey(t *testing.T) {
	h := nodetest.New(t)
	kept, err := h.Node().KeepSigningKeys(t.Context())
	if err != nil {
		t.Fatalf("keep the signing key's versions: %v", err)
	}
	if _, err := h.Node().Tokens().Issue(t.Context(), entity.NewUser(), [16]byte(uuid.New()), h.Clock().Now()); err != nil {
		t.Fatalf("issue a token: %v", err)
	}
	values := kept()
	if len(values) == 0 || !slices.ContainsFunc(values, func(v []byte) bool { return slices.ContainsFunc(v, func(b byte) bool { return b != 0 }) }) {
		t.Fatalf("versions read before Stop: %d, want one holding a key", len(values))
	}
	if err := h.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for i, v := range kept() {
		if slices.ContainsFunc(v, func(b byte) bool { return b != 0 }) {
			t.Errorf("version read %d after Stop: %d bytes not all zero, want cleared", i, len(v))
		}
	}
}

// New over a KV holding no installation refuses, and closes the KV it was
// handed: the watch finds nothing of it left open.
func TestNewRefusesAKVWithNoInstallation(t *testing.T) {
	prof.Watch(t, prof.Options{})
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = node.New(
		node.Config{
			Bootstrap: listening("127.0.0.1:0", "127.0.0.1:0"),
			KV:        db,
			Clock:     sim.NewTestClock(time.Unix(0, 0)),
			Metrics:   metrics.New("test", "test"),
			Logger:    slog.New(slog.NewTextHandler(t.Output(), nil)),
		},
	)
	if !errors.Is(err, kvstore.ErrNotInitialised) {
		t.Fatalf("New over an empty KV: got %v, want ErrNotInitialised", err)
	}
	if err := db.View(t.Context(), func(kv.Reader) error { return nil }); err == nil {
		t.Error("the KV after a refused New: a View succeeded, want it closed")
	}
}

// New over a store holding a v1 key and no format record refuses it, closing
// the KV: over Badger with the one sentence every v1 find gives, naming the
// store's directory, and over FoundationDB, where v1 never ran, with
// ErrForeignStore's own wording.
func TestNewRefusesAV1Store(t *testing.T) {
	const dir = "/the v1 store"
	sentence := "refusing to start: a metadata store at " + dir + " is from v1; " +
		"upgrading from v1 is not supported: start a new installation with dfs init on empty storage"
	for _, tc := range []struct {
		backend string
		v1      bool
	}{{"", true}, {"badger", true}, {"foundationdb", false}} {
		t.Run(cmp.Or(tc.backend, "unset"), func(t *testing.T) {
			db, err := badgerkv.Open(badgerkv.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Update(t.Context(), func(tx kv.Txn) error { return tx.Set([]byte("f:1"), []byte("v1 record")) }); err != nil {
				t.Fatal(err)
			}
			b := listening("127.0.0.1:0", "127.0.0.1:0")
			b.Metadata.Backend = tc.backend
			b.Metadata.Badger.Path = dir
			_, err = node.New(node.Config{
				Bootstrap: b, KV: db, Clock: sim.NewTestClock(time.Unix(0, 0)), Metrics: metrics.New("test", "test"),
				Logger: slog.New(slog.NewTextHandler(t.Output(), nil)),
			})
			switch {
			case tc.v1 && (err == nil || err.Error() != sentence || !errors.Is(err, kvstore.ErrV1Store)):
				t.Errorf("New over a v1 store, backend %q: got %v, want %q, still kvstore.ErrV1Store", tc.backend, err, sentence)
			case !tc.v1 && (!errors.Is(err, kvstore.ErrForeignStore) || strings.Contains(err.Error(), "from v1")):
				t.Errorf("New over a foreign store, backend %q: got %v, want ErrForeignStore's wording, never the v1 sentence", tc.backend, err)
			}
			if err := db.View(t.Context(), func(kv.Reader) error { return nil }); err == nil {
				t.Error("the KV after a refused New: a View succeeded, want it closed")
			}
		})
	}
}

// New over a Badger store kvstore.Init created that has since lost its format
// record refuses it as foreign, never with the v1 sentence: its keys are this
// format's records, holding an Installation an operator must not be told to
// start over from.
func TestNewRefusesAStoreThatLostItsFormatRecord(t *testing.T) {
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := kvstore.Init(t.Context(), db, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(t.Context(), func(tx kv.Txn) error { return tx.Delete(keys.Format) }); err != nil {
		t.Fatal(err)
	}
	b := listening("127.0.0.1:0", "127.0.0.1:0")
	b.Metadata.Badger.Path = "/the store"
	_, err = node.New(node.Config{
		Bootstrap: b, KV: db, Clock: sim.NewTestClock(time.Unix(0, 0)), Metrics: metrics.New("test", "test"),
		Logger: slog.New(slog.NewTextHandler(t.Output(), nil)),
	})
	if !errors.Is(err, kvstore.ErrForeignStore) || strings.Contains(err.Error(), "from v1") {
		t.Errorf("New over a store that lost its format record: got %v, want ErrForeignStore's wording, never the v1 sentence", err)
	}
}

// New refuses a bootstrap that names no storage key, as dfs init does, and
// one whose storage key does not open, over an initialised store; each
// closes the KV it was handed.
func TestNewRefusesNoStorageKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string // empty names no storage key
		want string
	}{
		{"none named", "", "node: keys.storage is not set: the administrator's password and the signing key are sealed under it"},
		{"one that does not open", filepath.Join(t.TempDir(), "missing.key"), "node: the storage key: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := badgerkv.Open(badgerkv.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := kvstore.Init(t.Context(), db, uuid.New()); err != nil {
				t.Fatal(err)
			}
			b := listening("127.0.0.1:0", "127.0.0.1:0")
			b.Keys.Storage.Local.File = tc.file
			if tc.file == "" {
				b.Keys.Storage.Kind = ""
			}
			_, err = node.New(node.Config{
				Bootstrap: b, KV: db, Clock: sim.NewTestClock(time.Unix(0, 0)), Metrics: metrics.New("test", "test"),
				Logger: slog.New(slog.NewTextHandler(t.Output(), nil)),
			})
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("New with a storage key %s: got %v, want refused with %q", tc.name, err, tc.want)
			}
			if err := db.View(t.Context(), func(kv.Reader) error { return nil }); err == nil {
				t.Error("the KV after a refused New: a View succeeded, want it closed")
			}
		})
	}
}

// New refuses an empty API or metrics address, which would bind a random
// port on every interface, and closes the KV it was handed.
func TestNewRefusesAnEmptyAddress(t *testing.T) {
	for _, tc := range []struct {
		name    string
		metrics string
		api     []string
	}{
		{"api", "127.0.0.1:0", nil},
		{"metrics", "", []string{"127.0.0.1:0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := badgerkv.Open(badgerkv.Options{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = node.New(node.Config{
				Bootstrap: listening(tc.metrics, tc.api...),
				KV:        db, Clock: sim.NewTestClock(time.Unix(0, 0)), Metrics: metrics.New("test", "test"),
				Logger: slog.New(slog.NewTextHandler(t.Output(), nil)),
			})

			want := "api.listen and metrics.listen must both be set"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("New with %s address empty: got %v, want refused for the empty address", tc.name, err)
			}
			if err := db.View(t.Context(), func(kv.Reader) error { return nil }); err == nil {
				t.Error("the KV after a refused New: a View succeeded, want it closed")
			}
		})
	}
}

// New refuses more than one API address, naming the key and never an
// address, which may come from the environment, and closes the KV it was
// handed.
func TestNewRefusesTwoAPIAddresses(t *testing.T) {
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = node.New(node.Config{
		Bootstrap: listening("127.0.0.1:0", "127.0.0.1:18080", "127.0.0.2:18081"),
		KV:        db,
		Clock:     sim.NewTestClock(time.Unix(0, 0)),
		Metrics:   metrics.New("test", "test"),
		Logger:    slog.New(slog.NewTextHandler(t.Output(), nil)),
	})

	want := "node: api.listen names 2 addresses, and the node's API binds one address for now"
	if err == nil || err.Error() != want {
		t.Fatalf("New with two API addresses: got %v, want %q", err, want)
	}

	for _, addr := range []string{"127.0.0.1:18080", "127.0.0.2:18081"} {
		if strings.Contains(err.Error(), addr) {
			t.Errorf(
				"the refusal %q prints the address %s, which may come from the environment",
				err,
				addr,
			)
		}
	}

	if err := db.View(t.Context(), func(kv.Reader) error { return nil }); err == nil {
		t.Error("the KV after a refused New: a View succeeded, want it closed")
	}
}

// listening is a bootstrap whose facts are the metrics address, the API's
// addresses and one journal device; none of its users starts a node over
// it, and each wants every refusal below the one it plants.
func listening(metrics string, api ...string) bootstrap.Bootstrap {
	var b bootstrap.Bootstrap

	b.Metrics.Listen = metrics
	b.API.Listen = api
	b.Journals = []bootstrap.Journal{{Path: "/a journal device the tests never open"}}
	b.Keys.Storage.Kind = keyprovider.KindLocal
	b.Keys.Storage.Local.File = "/a storage key the tests never open"

	return b
}

// The engine's series are in the registry the node was handed from New on,
// so a scrape answers them before anything starts.
func TestEngineSeriesRegistered(t *testing.T) {
	h := nodetest.New(t)
	mfs, err := h.Metrics().Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var got []string
	for _, mf := range mfs {
		got = append(got, mf.GetName())
	}
	for _, want := range []string{"dittofs_engine_self_fences_total", "dittofs_engine_stability_points_total", "dittofs_engine_pending_existence_ops", "dittofs_engine_read_seconds"} {
		if !slices.Contains(got, want) {
			t.Errorf("series after New: %s missing from %v", want, got)
		}
	}
}

// New refuses a bootstrap that names no journal device, and closes the KV it
// was handed.
func TestNewRefusesNoJournalDevice(t *testing.T) {
	for _, tc := range []struct {
		name      string
		withStore func(t *testing.T, db *badgerkv.Store)
	}{
		{"over an initialised store", func(t *testing.T, db *badgerkv.Store) {
			if err := kvstore.Init(t.Context(), db, uuid.New()); err != nil {
				t.Fatal(err)
			}
		}},
		{"over a store that is not initialised", func(t *testing.T, db *badgerkv.Store) {
			_ = db // initialised by no one: the store the row refuses over
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})
			db, err := badgerkv.Open(badgerkv.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			tc.withStore(t, db)
			b := listening("127.0.0.1:0", "127.0.0.1:0")
			b.Journals = nil
			b.Node.ID = entity.NodeID(uuid.New())
			_, err = node.New(
				node.Config{
					Bootstrap: b,
					KV:        db,
					Clock:     sim.NewTestClock(time.Unix(0, 0)),
					Metrics:   metrics.New("test", "test"),
					Logger:    slog.New(slog.NewTextHandler(t.Output(), nil)),
				},
			)
			// The refusal must not name the store, whose not-initialised
			// answer the operator would read as advice to run dfs init
			// against a node that has nowhere to keep journals.
			if err == nil || !strings.Contains(err.Error(), "no journal device") {
				t.Fatalf("New with no journals (%s): got %v, want refused for naming no journal device", tc.name, err)
			}
			if errors.Is(err, kvstore.ErrNotInitialised) {
				t.Errorf("New with no journals (%s): got %v, which must never wrap ErrNotInitialised", tc.name, err)
			}
			if err := db.View(t.Context(), func(kv.Reader) error { return nil }); err == nil {
				t.Error("the KV after a refused New: a View succeeded, want it closed")
			}
		})
	}
}
