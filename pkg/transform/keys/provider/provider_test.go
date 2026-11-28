package provider

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// errOutage is the fault faultyLoad injects.
var errOutage = errors.New("injected outage")

// faultyLoad wraps the in-memory view and fails every Load while fail is set.
type faultyLoad struct {
	*keystest.Records
	fail atomic.Bool
}

func (f *faultyLoad) Load(ctx context.Context, ns entity.NamespaceID) ([]Row, error) {
	if f.fail.Load() {
		return nil, errOutage
	}
	return f.Records.Load(ctx, ns)
}

// failingOpen wraps a real AEAD and refuses every Open, so a test can see
// that nothing is unwrapped after a point.
type failingOpen struct{ cipher.AEAD }

func (failingOpen) Open([]byte, []byte, []byte, []byte) ([]byte, error) {
	return nil, errors.New("injected unwrap failure")
}

// readFixture is a provider over a key file and a faultyLoad view, with the
// key file's path so a fresh provider can be built over both.
type readFixture struct {
	p       *Provider
	records *faultyLoad
	path    string
}

func newReadFixture(t *testing.T) readFixture {
	t.Helper()

	path, _, _ := keyFile(t)
	f := readFixture{records: &faultyLoad{Records: keystest.NewRecords()}, path: path}
	f.p = f.fresh(t)
	return f
}

// fresh is a new provider over f's key file and records, sharing no cache.
func (f readFixture) fresh(t *testing.T) *Provider {
	t.Helper()

	p, err := New(context.Background(), Config{KeyFile: f.path, Passphrase: passphrase}, Deps{Records: f.records, Rand: rand.Reader})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// wrapped is a row of ns holding key, wrapped by p, with key's fingerprint.
func wrapped(t *testing.T, p *Provider, ns entity.NamespaceID, kind, id, state string, key []byte) Row {
	t.Helper()

	w, err := p.wrap(context.Background(), sealIn{aad: wrapAAD(ns[:], kind, id), key: key})
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	return Row{Kind: kind, ID: id, State: state, Record: p.record(fingerprint(key), p.kf.masterKeyID[:], w[0])}
}

// put writes rows into ns's records through the view, as another writer would.
func put(t *testing.T, records KeyRecords, ns entity.NamespaceID, rows ...Row) {
	t.Helper()

	if err := records.Update(context.Background(), ns, func([]Row) ([]Row, error) { return rows, nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

// exportKey is the export key's kind, which no consumer outside the provider
// names.
const exportKey = "export-key"

// key32 is a 32-byte key of one repeated byte.
func key32(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// the material errors, so an assertion can say which one an error is not.
var materialErrs = []error{keys.ErrUnknownMaterial, keys.ErrMaterialUnavailable, keys.ErrMaterialDestroyed}

// isOnly reports whether err is want and no other material error.
func isOnly(err, want error) bool {
	for _, e := range materialErrs {
		if errors.Is(err, e) != (e == want) {
			return false
		}
	}
	return true
}

// Each answer of Get, and of Current for the same record made current, from
// rows written through the view: a matching record is served, and each fault
// is its own error. Errors folded into one, or a destroyed key reported as
// unavailable, fail here.
func TestGetAndCurrentAnswers(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	other := newReadFixture(t)
	ctx := context.Background()
	key := key32(0x5a)

	edit := func(row Row, fn func(*keyRecord)) Row {
		r, err := decodeKeyRecord(row.Record)
		if err != nil {
			t.Fatalf("decode the record to edit: %v", err)
		}
		fn(&r)
		row.Record = r.encode()
		return row
	}

	for _, tc := range []struct {
		name string
		row  func(ns entity.NamespaceID) Row
		want error // nil: served
	}{
		{"a matching wrapped record", func(ns entity.NamespaceID) Row {
			return wrapped(t, f.p, ns, keys.KindDataKey, "k", stateCurrent, key)
		}, nil},
		{"a chunk-ID key held in the clear", func(ns entity.NamespaceID) Row {
			return Row{Kind: keys.KindChunkIDKey, ID: "k", State: stateCurrent, Record: f.p.record(fingerprint(key), nil, key)}
		}, nil},
		{"a data key held in the clear", func(ns entity.NamespaceID) Row {
			return Row{Kind: keys.KindDataKey, ID: "k", State: stateCurrent, Record: f.p.record(fingerprint(key), nil, key)}
		}, keys.ErrMaterialUnavailable},
		{"a destroyed record", func(ns entity.NamespaceID) Row {
			return Row{Kind: keys.KindDataKey, ID: "k", State: stateDestroyed, Record: f.p.record(fingerprint(key), nil, nil)}
		}, keys.ErrMaterialDestroyed},
		{"a fingerprint that does not match", func(ns entity.NamespaceID) Row {
			return edit(wrapped(t, f.p, ns, keys.KindDataKey, "k", stateCurrent, key), func(r *keyRecord) { r.fingerprint[0] ^= 1 })
		}, keys.ErrMaterialUnavailable},
		{"two master key IDs that differ", func(ns entity.NamespaceID) Row {
			return edit(wrapped(t, f.p, ns, keys.KindDataKey, "k", stateCurrent, key), func(r *keyRecord) { r.masterKeyID = other.p.kf.masterKeyID[:] })
		}, keys.ErrMaterialUnavailable},
		{"a wrap under a master key not held", func(ns entity.NamespaceID) Row {
			return wrapped(t, other.p, ns, keys.KindDataKey, "k", stateCurrent, key)
		}, keys.ErrMaterialUnavailable},
		{"a wrap that fails to unwrap", func(ns entity.NamespaceID) Row {
			return edit(wrapped(t, f.p, ns, keys.KindDataKey, "k", stateCurrent, key), func(r *keyRecord) {
				r.key = bytes.Clone(r.key)
				r.key[len(r.key)-1] ^= 1
			})
		}, keys.ErrMaterialUnavailable},
		{"an unknown codec version", func(ns entity.NamespaceID) Row {
			row := wrapped(t, f.p, ns, keys.KindDataKey, "k", stateCurrent, key)
			row.Record = append([]byte{2}, row.Record[1:]...)
			return row
		}, keys.ErrMaterialUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := entity.NewNamespaceID()
			row := tc.row(ns)
			put(t, f.records, ns, row)
			p := f.fresh(t)
			id := keys.MaterialID{Namespace: ns, Kind: row.Kind, ID: row.ID}

			got, err := p.Get(ctx, id)
			if tc.want == nil {
				if err != nil || !bytes.Equal(got, key) {
					t.Errorf("Get = %x, %v; want the key", got, err)
				}
			} else if !isOnly(err, tc.want) || got != nil {
				t.Errorf("Get = %x, %v; want no bytes and %v alone", got, err, tc.want)
			}

			// Current of its kind answers as Get did, on a provider that has
			// not served it yet.
			mid, err := f.fresh(t).Current(ctx, ns, row.Kind)
			want := keys.MaterialID{Namespace: ns, Kind: row.Kind, ID: row.ID, Fingerprint: fingerprint(key)}
			if tc.want == nil && (err != nil || mid != want) {
				t.Errorf("Current = %+v, %v; want %+v", mid, err, want)
			}
			if tc.want != nil && !isOnly(err, tc.want) {
				t.Errorf("Current = %v, want %v alone", err, tc.want)
			}
		})
	}

	ns := entity.NewNamespaceID()
	put(t, f.records, ns, wrapped(t, f.p, ns, keys.KindDataKey, "k", stateCurrent, key))
	if _, err := f.p.Get(ctx, keys.MaterialID{Namespace: ns, Kind: keys.KindDataKey, ID: "never"}); !isOnly(err, keys.ErrUnknownMaterial) {
		t.Errorf("Get of an ID never given = %v, want ErrUnknownMaterial alone", err)
	}
	if _, err := f.p.Get(ctx, keys.MaterialID{Namespace: entity.NewNamespaceID(), Kind: keys.KindDataKey, ID: "k"}); !isOnly(err, keys.ErrUnknownMaterial) {
		t.Errorf("Get of the ID in another namespace = %v, want ErrUnknownMaterial alone", err)
	}
	if _, err := f.p.Current(ctx, ns, keys.KindChunkingKey); !isOnly(err, keys.ErrUnknownMaterial) {
		t.Errorf("Current of a kind never given = %v, want ErrUnknownMaterial alone", err)
	}
}

// A Load that fails is an outage, not an absence, and caches nothing: once
// it clears, the same Get succeeds. An outage reported as an absence, or a
// failure cached, fails here.
func TestLoadOutageIsUnavailableUntilItClears(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	ctx := context.Background()
	ns := entity.NewNamespaceID()
	row := wrapped(t, f.p, ns, keys.KindDataKey, "k", stateCurrent, key32(1))
	put(t, f.records, ns, row)
	id := keys.MaterialID{Namespace: ns, Kind: row.Kind, ID: row.ID}

	f.records.fail.Store(true)
	if got, err := f.p.Get(ctx, id); !isOnly(err, keys.ErrMaterialUnavailable) || got != nil {
		t.Fatalf("Get during a Load outage = %x, %v; want no bytes and ErrMaterialUnavailable alone", got, err)
	}
	if _, err := f.p.Current(ctx, ns, keys.KindDataKey); !isOnly(err, keys.ErrMaterialUnavailable) {
		t.Fatalf("Current during a Load outage = %v, want ErrMaterialUnavailable alone", err)
	}

	f.records.fail.Store(false)
	if got, err := f.p.Get(ctx, id); err != nil || !bytes.Equal(got, key32(1)) {
		t.Errorf("Get once the outage cleared = %x, %v; want the key", got, err)
	}
}

// A record another writer adds after a first read is found by Get on the
// same provider, through one reload. A miss that never reloads fails here.
func TestGetReloadsOnAMiss(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	ctx := context.Background()
	ns := entity.NewNamespaceID()
	put(t, f.records, ns, wrapped(t, f.p, ns, keys.KindDataKey, "first", stateCurrent, key32(1)))
	if _, err := f.p.Get(ctx, keys.MaterialID{Namespace: ns, Kind: keys.KindDataKey, ID: "first"}); err != nil {
		t.Fatalf("first Get: %v", err)
	}

	put(t, f.records, ns, wrapped(t, f.p, ns, keys.KindDataKey, "added", "retired", key32(2)))
	if got, err := f.p.Get(ctx, keys.MaterialID{Namespace: ns, Kind: keys.KindDataKey, ID: "added"}); err != nil || !bytes.Equal(got, key32(2)) {
		t.Errorf("Get of a record added after the first read = %x, %v; want its key", got, err)
	}
}

// Current of a kind the opened namespace was never given is answered from the
// cache: with every Load failing after Open, it is ErrUnknownMaterial, not
// ErrMaterialUnavailable. A kind miss that reloads on every call fails here.
func TestCurrentOfAKindNeverGivenDoesNotReload(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	ctx := context.Background()
	ns := entity.NewNamespaceID()
	put(t, f.records, ns, wrapped(t, f.p, ns, keys.KindDataKey, "k", stateCurrent, key32(1)))
	if err := f.p.Open(ctx, ns); err != nil {
		t.Fatalf("Open: %v", err)
	}

	f.records.fail.Store(true)
	for range 3 {
		if _, err := f.p.Current(ctx, ns, keys.KindChunkingKey); !isOnly(err, keys.ErrUnknownMaterial) {
			t.Fatalf("Current of a kind never given, Loads failing = %v, want ErrUnknownMaterial alone", err)
		}
	}
}

// A row the provider's own update marks destroyed stops being served at once,
// by the same provider, with no reload possible. Cached rows that keep what fn
// saw beside what it rewrote, or a cached key served before its row's state,
// fail here.
func TestUpdateMarkingDestroyedIsSeenAtOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	ctx := context.Background()
	ns := entity.NewNamespaceID()
	row := wrapped(t, f.p, ns, keys.KindDataKey, "k", stateCurrent, key32(1))
	put(t, f.records, ns, row)
	id := keys.MaterialID{Namespace: ns, Kind: row.Kind, ID: row.ID}
	if _, err := f.p.Get(ctx, id); err != nil {
		t.Fatalf("Get before the update: %v", err)
	}

	err := f.p.update(ctx, ns, func([]Row) ([]Row, error) {
		return []Row{{Kind: row.Kind, ID: row.ID, State: stateDestroyed, Record: f.p.record(fingerprint(key32(1)), nil, nil)}}, nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	f.records.fail.Store(true)
	if got, err := f.p.Get(ctx, id); !isOnly(err, keys.ErrMaterialDestroyed) || got != nil {
		t.Errorf("Get after the update marked it destroyed = %x, %v; want no bytes and ErrMaterialDestroyed alone", got, err)
	}
	if _, err := f.p.Current(ctx, ns, keys.KindDataKey); !isOnly(err, keys.ErrMaterialDestroyed) {
		t.Errorf("Current after its one data key was destroyed = %v, want ErrMaterialDestroyed alone", err)
	}
}

// One namespace's wrapped data key copied, as a row, into another's records
// is refused there and returns no bytes. An AAD checked at wrap time only
// fails here.
func TestRecordCopiedToAnotherNamespaceIsRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	ctx := context.Background()
	a, b := entity.NewNamespaceID(), entity.NewNamespaceID()
	row := wrapped(t, f.p, a, keys.KindDataKey, "k", stateCurrent, key32(1))
	put(t, f.records, a, row)
	put(t, f.records, b, row)

	if got, err := f.p.Get(ctx, keys.MaterialID{Namespace: b, Kind: row.Kind, ID: row.ID}); !isOnly(err, keys.ErrMaterialUnavailable) || got != nil {
		t.Errorf("Get of the copied record = %x, %v; want no bytes and ErrMaterialUnavailable alone", got, err)
	}
	if got, err := f.p.Get(ctx, keys.MaterialID{Namespace: a, Kind: row.Kind, ID: row.ID}); err != nil || !bytes.Equal(got, key32(1)) {
		t.Errorf("Get of the original = %x, %v; want its key", got, err)
	}
}

// A chunking key's row rewritten with another key's wrap under the same ID
// and the old fingerprint is refused by a fresh provider at Open, Get and
// Current. A fingerprint taken from the unwrapped bytes rather than the
// record, a Current that serves a replaced key, or a namespace that opens
// over one fails here.
func TestReplacedChunkingKeyIsRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	ctx := context.Background()
	ns := entity.NewNamespaceID()
	row := wrapped(t, f.p, ns, keys.KindChunkingKey, "k", stateCurrent, key32(1))
	put(t, f.records, ns, row)

	replaced := wrapped(t, f.p, ns, keys.KindChunkingKey, "k", stateCurrent, key32(2))
	r, err := decodeKeyRecord(replaced.Record)
	if err != nil {
		t.Fatalf("decode the replacing record: %v", err)
	}
	r.fingerprint = fingerprint(key32(1))
	replaced.Record = r.encode()
	put(t, f.records, ns, replaced)

	if err := f.fresh(t).Open(ctx, ns); !isOnly(err, keys.ErrMaterialUnavailable) {
		t.Errorf("Open over a replaced chunking key = %v, want ErrMaterialUnavailable alone", err)
	}
	if got, err := f.fresh(t).Get(ctx, keys.MaterialID{Namespace: ns, Kind: row.Kind, ID: row.ID}); !isOnly(err, keys.ErrMaterialUnavailable) || got != nil {
		t.Errorf("Get of a replaced chunking key = %x, %v; want no bytes and ErrMaterialUnavailable alone", got, err)
	}
	if _, err := f.fresh(t).Current(ctx, ns, keys.KindChunkingKey); !isOnly(err, keys.ErrMaterialUnavailable) {
		t.Errorf("Current of a replaced chunking key = %v, want ErrMaterialUnavailable alone", err)
	}
}

// Open unwraps every current key before the namespace opens: one whose
// current row names a master key the provider does not hold fails Open, and
// one whose rows all unwrap opens and then serves Get with no unwrap left to
// make. Keys loaded only on first use fail here.
func TestOpenUnwrapsEveryCurrentKey(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	other := newReadFixture(t)
	ctx := context.Background()

	bad := entity.NewNamespaceID()
	// Both current rows are bad, so a walk that checks only one of them,
	// whichever the map yields first, still fails.
	put(t, f.records, bad,
		wrapped(t, other.p, bad, keys.KindChunkIDKey, "c", stateCurrent, key32(1)),
		wrapped(t, other.p, bad, keys.KindDataKey, "d", stateCurrent, key32(2)),
		wrapped(t, f.p, bad, exportKey, "e", "retired", key32(9)))
	if err := f.p.Open(ctx, bad); !isOnly(err, keys.ErrMaterialUnavailable) {
		t.Errorf("Open of a namespace with a key under another master key = %v, want ErrMaterialUnavailable alone", err)
	}

	good := entity.NewNamespaceID()
	put(t, f.records, good,
		wrapped(t, f.p, good, keys.KindChunkIDKey, "c", stateCurrent, key32(3)),
		wrapped(t, f.p, good, keys.KindDataKey, "d", stateCurrent, key32(4)))
	p := f.fresh(t)
	if err := p.Open(ctx, good); err != nil {
		t.Fatalf("Open of a namespace whose keys all unwrap: %v", err)
	}

	p.kf.aead = failingOpen{p.kf.aead}
	for _, tc := range []struct {
		kind, id string
		key      []byte
	}{{keys.KindChunkIDKey, "c", key32(3)}, {keys.KindDataKey, "d", key32(4)}} {
		if got, err := p.Get(ctx, keys.MaterialID{Namespace: good, Kind: tc.kind, ID: tc.id}); err != nil || !bytes.Equal(got, tc.key) {
			t.Errorf("Get of %s after Open, unwrapping refused = %x, %v; want the key Open unwrapped", tc.kind, got, err)
		}
	}
}

// Get hands the caller a copy: a change to it never reaches the provider. A
// Get returning the cached slice fails here.
func TestGetReturnsACopy(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	ctx := context.Background()
	ns := entity.NewNamespaceID()
	put(t, f.records, ns, wrapped(t, f.p, ns, keys.KindDataKey, "k", stateCurrent, key32(1)))
	id := keys.MaterialID{Namespace: ns, Kind: keys.KindDataKey, ID: "k"}
	if err := f.p.Open(ctx, ns); err != nil {
		t.Fatalf("Open: %v", err)
	}

	got, err := f.p.Get(ctx, id)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	clear(got)
	if again, err := f.p.Get(ctx, id); err != nil || !bytes.Equal(again, key32(1)) {
		t.Errorf("Get after the caller cleared its first answer = %x, %v; want the key unchanged", again, err)
	}
}

// A Get of a cached key allocates only the caller's copy, which
// keys.Provider.Get makes the caller's own. A map key built per call, a
// record decoded again, or a second copy fails here.
//
// It runs without prof.Watch, whose own sampling would count here.
func TestAllocsProviderGet(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector's allocations make the count meaningless")
	}

	f := newReadFixture(t)
	ctx := context.Background()
	ns := entity.NewNamespaceID()
	put(t, f.records, ns, wrapped(t, f.p, ns, keys.KindDataKey, "k", stateCurrent, key32(1)))
	id := keys.MaterialID{Namespace: ns, Kind: keys.KindDataKey, ID: "k"}
	if err := f.p.Open(ctx, ns); err != nil {
		t.Fatalf("Open: %v", err)
	}

	allocs := testing.AllocsPerRun(100, func() {
		if _, err := f.p.Get(ctx, id); err != nil {
			t.Fatalf("cached Get: %v", err)
		}
	})
	if allocs != 1 {
		t.Errorf("a cached Get allocates %v times, want 1, the caller's copy", allocs)
	}
}

// A record that cannot be served is logged once at Error, by ID in hex, however
// often it is asked for. A failure logged on every call, or one never logged,
// fails here.
func TestAnUnservableRecordIsLoggedOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})

	path, _, _ := keyFile(t)
	records := keystest.NewRecords()
	var log bytes.Buffer
	p, err := New(context.Background(), Config{KeyFile: path, Passphrase: passphrase}, Deps{
		Records: records, Rand: rand.Reader, Logger: slog.New(slog.NewTextHandler(&log, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	ns := entity.NewNamespaceID()
	row := wrapped(t, p, ns, keys.KindDataKey, "k", stateCurrent, key32(1))
	row.Record = append([]byte{2}, row.Record[1:]...)
	put(t, records, ns, row)

	for range 3 {
		if _, err := p.Get(ctx, keys.MaterialID{Namespace: ns, Kind: keys.KindDataKey, ID: "k"}); !isOnly(err, keys.ErrMaterialUnavailable) {
			t.Fatalf("Get of an unreadable record = %v, want ErrMaterialUnavailable alone", err)
		}
	}
	if n := strings.Count(log.String(), "level=ERROR"); n != 1 {
		t.Errorf("three refused Gets logged %d Error lines, want 1:\n%s", n, log.String())
	}
	if want := fmt.Sprintf("id=%x", "k"); !strings.Contains(log.String(), want) {
		t.Errorf("the log line does not name the record by %s:\n%s", want, log.String())
	}
}

// A clear chunk-ID record in a namespace that holds a data or chunking key is
// refused, since only a non-encrypting namespace holds its chunk-ID key in the
// clear and nothing binds a clear key to its record. A writer of the metadata
// store could otherwise swap an encrypting namespace's chunk-ID key for one it
// knows. A clear record accepted for any chunk-ID key fails here.
func TestClearChunkIDKeyInAnEncryptingNamespaceIsRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	ctx := context.Background()
	ns := entity.NewNamespaceID()
	put(t, f.records, ns,
		Row{Kind: keys.KindChunkIDKey, ID: "c", State: stateCurrent, Record: f.p.record(fingerprint(key32(1)), nil, key32(1))},
		wrapped(t, f.p, ns, keys.KindDataKey, "d", stateCurrent, key32(2)))

	if err := f.fresh(t).Open(ctx, ns); !isOnly(err, keys.ErrMaterialUnavailable) {
		t.Errorf("Open over a clear chunk-ID key beside a data key = %v, want ErrMaterialUnavailable alone", err)
	}
	if got, err := f.fresh(t).Get(ctx, keys.MaterialID{Namespace: ns, Kind: keys.KindChunkIDKey, ID: "c"}); !isOnly(err, keys.ErrMaterialUnavailable) || got != nil {
		t.Errorf("Get of a clear chunk-ID key beside a data key = %x, %v; want no bytes and ErrMaterialUnavailable alone", got, err)
	}
}
