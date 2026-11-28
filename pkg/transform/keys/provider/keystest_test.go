package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// harness adapts the provider to keystest.Harness over the in-memory view.
// Give, Replace and Destroy write rows as another writer of the metadata
// store would, then reload the provider, so one provider serves the whole
// run and Calls is its own.
type harness struct {
	*Provider
	t       *testing.T
	records *keystest.Records
}

func newHarness(t *testing.T) keystest.Harness {
	t.Helper()

	path, _, _ := keyFile(t)
	records := keystest.NewRecords()
	p, err := New(context.Background(), Config{KeyFile: path, Passphrase: passphrase}, Deps{Records: records, Rand: rand.Reader})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return harness{Provider: p, t: t, records: records}
}

// write runs fn on ns's rows through the view, then reloads the provider.
func (h harness) write(ns entity.NamespaceID, fn func(rows []Row) []Row) {
	h.t.Helper()

	ctx := context.Background()
	if err := h.records.Update(ctx, ns, func(rows []Row) ([]Row, error) { return fn(rows), nil }); err != nil {
		h.t.Fatalf("Update of namespace %s: %v", ns, err)
	}
	h.reload(ctx)
}

// row is id's row among rows, failing the test when there is none.
func (h harness) row(rows []Row, id keys.MaterialID) Row {
	h.t.Helper()

	i := slices.IndexFunc(rows, func(r Row) bool { return r.Kind == id.Kind && r.ID == id.ID })
	if i < 0 {
		h.t.Fatalf("%s %q of namespace %s was never given", id.Kind, id.ID, id.Namespace)
	}
	return rows[i]
}

// sealed is a wrapped record of material under ns, kind and id, recorded
// with fingerprint fp.
func (h harness) sealed(ns entity.NamespaceID, kind, id string, fp [32]byte, material []byte) []byte {
	h.t.Helper()

	w, err := h.wrap(context.Background(), sealIn{aad: wrapAAD(ns[:], kind, id), key: material})
	if err != nil {
		h.t.Fatalf("wrap: %v", err)
	}
	return h.record(fp, h.masterKeyID[:], w[0])
}

// Give writes material as the current record of kind, marking retired the
// kind's previous current record.
func (h harness) Give(ns entity.NamespaceID, kind, id string, material []byte) keys.MaterialID {
	fp := fingerprint(material)
	rec := h.sealed(ns, kind, id, fp, material)
	h.write(ns, func(rows []Row) []Row {
		out := []Row{{Kind: kind, ID: id, State: stateCurrent, Record: rec}}
		for _, r := range rows {
			if r.Kind == kind && r.ID != id && r.State == stateCurrent {
				r.State = "retired"
				out = append(out, r)
			}
		}
		return out
	})
	return keys.MaterialID{Namespace: ns, Kind: kind, ID: id, Fingerprint: fp}
}

// Replace rewrites id's record with material's wrap and the fingerprint it
// recorded.
func (h harness) Replace(id keys.MaterialID, material []byte) {
	h.write(id.Namespace, func(rows []Row) []Row {
		r := h.row(rows, id)
		old, err := decodeKeyRecord(r.Record)
		if err != nil {
			h.t.Fatalf("decode %q's record: %v", id.ID, err)
		}
		r.Record = h.sealed(id.Namespace, id.Kind, id.ID, old.fingerprint, material)
		return []Row{r}
	})
}

// Destroy marks id's record destroyed, keeping only its fingerprint.
func (h harness) Destroy(id keys.MaterialID) {
	h.write(id.Namespace, func(rows []Row) []Row {
		r := h.row(rows, id)
		old, err := decodeKeyRecord(r.Record)
		if err != nil {
			h.t.Fatalf("decode %q's record: %v", id.ID, err)
		}
		return []Row{{Kind: r.Kind, ID: r.ID, State: stateDestroyed, Record: h.record(old.fingerprint, nil, nil)}}
	})
}

// The provider passes the provider suite every keys.Provider is held to.
func TestProviderSuite(t *testing.T) {
	prof.Watch(t, prof.Options{})

	keystest.Run(t, newHarness)
}

// A Get of an ID never given puts it in Status().Unknown until it is given
// and read; a Current of a kind never given, even with every Load failing,
// reports nothing. An unknown ID never reported, one that never leaves, or a
// kind miss reported with no ID fails here.
func TestStatusUnknown(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	ctx := context.Background()
	ns := entity.NewNamespaceID()
	put(t, f.records, ns, wrapped(t, f.p, ns, keys.KindDataKey, "k", stateCurrent, key32(1)))
	if err := f.p.Open(ctx, ns); err != nil {
		t.Fatalf("Open: %v", err)
	}

	f.records.fail.Store(true)
	if _, err := f.p.Current(ctx, ns, keys.KindChunkingKey); !isOnly(err, keys.ErrUnknownMaterial) {
		t.Fatalf("Current of a kind never given = %v, want ErrUnknownMaterial alone", err)
	}
	if st := f.p.Status(); len(st.Unknown) != 0 || len(st.Unavailable) != 0 {
		t.Errorf("Status after a Current of a kind never given, Loads failing = %+v, want nothing unknown or unavailable (a Current that reloads on a kind miss sees the outage)", st)
	}
	f.records.fail.Store(false)

	late := keys.MaterialID{Namespace: ns, Kind: keys.KindDataKey, ID: "late"}
	if _, err := f.p.Get(ctx, late); !isOnly(err, keys.ErrUnknownMaterial) {
		t.Fatalf("Get of an ID never given = %v, want ErrUnknownMaterial alone", err)
	}
	if got := f.p.Status().Unknown; !slices.Equal(got, []keys.MaterialID{late}) {
		t.Errorf("Status().Unknown after a Get of an ID never given = %+v, want %+v", got, []keys.MaterialID{late})
	}

	put(t, f.records, ns, wrapped(t, f.p, ns, keys.KindDataKey, "late", "retired", key32(2)))
	if _, err := f.p.Get(ctx, late); err != nil {
		t.Fatalf("Get once the ID is given: %v", err)
	}
	if got := f.p.Status().Unknown; len(got) != 0 {
		t.Errorf("Status().Unknown after the ID was given and read = %+v, want empty", got)
	}
}

// A replaced key, and a key read during a Load outage, are refused and held
// in Status().Unavailable, so encryption_keys_unavailable counts them, until
// a read serves them again. A refusal that is not reported, or one reported
// for good, fails here.
func TestStatusUnavailable(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	ctx := context.Background()
	ns := entity.NewNamespaceID()
	good := wrapped(t, f.p, ns, keys.KindDataKey, "k", stateCurrent, key32(1))
	put(t, f.records, ns, good)
	id := keys.MaterialID{Namespace: ns, Kind: keys.KindDataKey, ID: "k"}

	bad := wrapped(t, f.p, ns, keys.KindDataKey, "k", stateCurrent, key32(2))
	r, err := decodeKeyRecord(bad.Record)
	if err != nil {
		t.Fatalf("decode the replacing record: %v", err)
	}
	r.fingerprint = fingerprint(key32(1))
	bad.Record = r.encode()
	put(t, f.records, ns, bad)

	if _, err := f.p.Get(ctx, id); !isOnly(err, keys.ErrMaterialUnavailable) {
		t.Fatalf("Get of a replaced key = %v, want ErrMaterialUnavailable alone", err)
	}
	if got := f.p.Status().Unavailable; !slices.Equal(got, []keys.MaterialID{id}) {
		t.Errorf("Status().Unavailable after a replaced key was refused = %+v, want %+v, a gauge of 1", got, []keys.MaterialID{id})
	}

	put(t, f.records, ns, good)
	f.p.reload(ctx)
	if _, err := f.p.Get(ctx, id); err != nil {
		t.Fatalf("Get once the record is restored: %v", err)
	}
	if got := f.p.Status().Unavailable; len(got) != 0 {
		t.Errorf("Status().Unavailable after the key was served again = %+v, want empty", got)
	}

	other := keys.MaterialID{Namespace: entity.NewNamespaceID(), Kind: keys.KindDataKey, ID: "o"}
	f.records.fail.Store(true)
	if _, err := f.p.Get(ctx, other); !isOnly(err, keys.ErrMaterialUnavailable) {
		t.Fatalf("Get during a Load outage = %v, want ErrMaterialUnavailable alone", err)
	}
	if got := f.p.Status().Unavailable; !slices.Equal(got, []keys.MaterialID{other}) {
		t.Errorf("Status().Unavailable after a Get during a Load outage = %+v, want %+v", got, []keys.MaterialID{other})
	}
}

// Two current data keys in one namespace, which Current refuses, both count
// as unavailable, and the namespace has no current key age. A refusal
// Current makes before any record is served, left unreported, fails here.
func TestStatusTwoCurrentRecords(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	ctx := context.Background()
	ns := entity.NewNamespaceID()
	put(t, f.records, ns,
		wrapped(t, f.p, ns, keys.KindDataKey, "a", stateCurrent, key32(1)),
		wrapped(t, f.p, ns, keys.KindDataKey, "b", stateCurrent, key32(2)))

	if _, err := f.p.Current(ctx, ns, keys.KindDataKey); !isOnly(err, keys.ErrMaterialUnavailable) {
		t.Fatalf("Current over two current data keys = %v, want ErrMaterialUnavailable alone", err)
	}
	st := f.p.Status()
	want := []keys.MaterialID{{Namespace: ns, Kind: keys.KindDataKey, ID: "a"}, {Namespace: ns, Kind: keys.KindDataKey, ID: "b"}}
	if !slices.Equal(st.Unavailable, want) {
		t.Errorf("Status().Unavailable over two current data keys = %+v, want %+v", st.Unavailable, want)
	}
	if since, ok := st.CurrentSince[ns]; ok {
		t.Errorf("Status().CurrentSince over two current data keys = %v, want no entry", since)
	}
}

// An ID refused during a Load outage leaves Status().Unavailable once its
// record is found destroyed, since destroyed material is lost, not
// unavailable. A miss that only a successful read clears fails here.
func TestStatusMissFoundDestroyed(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	ctx := context.Background()
	ns := entity.NewNamespaceID()
	id := keys.MaterialID{Namespace: ns, Kind: keys.KindDataKey, ID: "k"}
	put(t, f.records, ns, Row{Kind: id.Kind, ID: id.ID, State: stateDestroyed, Record: f.p.record(fingerprint(key32(1)), nil, nil)})

	f.records.fail.Store(true)
	if _, err := f.p.Get(ctx, id); !isOnly(err, keys.ErrMaterialUnavailable) {
		t.Fatalf("Get during a Load outage = %v, want ErrMaterialUnavailable alone", err)
	}
	f.records.fail.Store(false)
	if _, err := f.p.Get(ctx, id); !isOnly(err, keys.ErrMaterialDestroyed) {
		t.Fatalf("Get once the outage cleared = %v, want ErrMaterialDestroyed alone", err)
	}
	if got := f.p.Status().Unavailable; len(got) != 0 {
		t.Errorf("Status().Unavailable once the ID was found destroyed = %+v, want empty", got)
	}
}

// CurrentSince is the time the clock read when the current data key's record
// was written, on the provider that wrote it and on a fresh one with another
// clock. An age measured from process start, or from the load, fails here.
func TestStatusCurrentSince(t *testing.T) {
	prof.Watch(t, prof.Options{})

	path, _, _ := keyFile(t)
	records := keystest.NewRecords()
	at := func(when time.Time) *Provider {
		p, err := New(context.Background(), Config{KeyFile: path, Passphrase: passphrase}, Deps{
			Records: records, Rand: rand.Reader, Now: func() time.Time { return when },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return p
	}

	ctx := context.Background()
	written := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	p := at(written)
	ns := entity.NewNamespaceID()
	// The chunking key's record is newer, so a Status that took any current
	// key's time, not the data key's, reads the wrong one.
	put(t, records, ns,
		wrapped(t, p, ns, keys.KindDataKey, "d", stateCurrent, key32(1)),
		wrapped(t, at(written.Add(time.Hour)), ns, keys.KindChunkingKey, "c", stateCurrent, key32(2)))

	for _, tc := range []struct {
		name string
		p    *Provider
	}{{"the writing provider", p}, {"a fresh provider an hour on", at(written.Add(time.Hour))}} {
		if err := tc.p.Open(ctx, ns); err != nil {
			t.Fatalf("%s: Open: %v", tc.name, err)
		}
		got := tc.p.Status().CurrentSince
		if len(got) != 1 || !got[ns].Equal(written) {
			t.Errorf("%s: Status().CurrentSince = %v, want only namespace %s at %v", tc.name, got, ns, written)
		}
	}
}

// Current and Get from 64 goroutines over two namespaces, on a provider that
// has loaded neither, give what sequential calls give, and Calls counts every
// one. An unguarded cache, or a count that loses calls, fails here under
// -race.
func TestConcurrentCurrentAndGet(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	ctx := context.Background()
	type want struct {
		ns   entity.NamespaceID
		kind string
		id   string
		key  []byte
	}
	var wants []want
	for i := range 2 {
		ns := entity.NewNamespaceID()
		for j, kind := range []string{keys.KindDataKey, keys.KindChunkingKey} {
			w := want{ns, kind, fmt.Sprintf("k%d", j), key32(byte(16*i + j + 1))}
			wants = append(wants, w)
			put(t, f.records, ns, wrapped(t, f.p, ns, kind, w.id, stateCurrent, w.key))
		}
	}

	p := f.fresh(t)
	const goroutines, rounds = 64, 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			<-start
			for r := range rounds {
				w := wants[(g+r)%len(wants)]
				id, err := p.Current(ctx, w.ns, w.kind)
				if err != nil || id.ID != w.id || id.Fingerprint != fingerprint(w.key) {
					t.Errorf("Current(%s, %s) = %+v, %v; want ID %q with its fingerprint", w.ns, w.kind, id, err, w.id)
					return
				}
				if got, err := p.Get(ctx, id); err != nil || !bytes.Equal(got, w.key) {
					t.Errorf("Get(%s %q) = %x, %v; want %x", w.kind, w.id, got, err, w.key)
					return
				}
			}
		})
	}
	close(start)
	wg.Wait()

	if got, want := p.Calls(), uint64(goroutines*rounds*2); !t.Failed() && got != want {
		t.Errorf("Calls = %d after %d concurrent Currents and Gets, want %d", got, want, want)
	}
}

// encodings are the forms a secret could be formatted into a message in.
func encodings(secret []byte) map[string]string {
	return map[string]string{
		"raw":               string(secret),
		"hex":               hex.EncodeToString(secret),
		"upper hex":         strings.ToUpper(hex.EncodeToString(secret)),
		"base64":            base64.StdEncoding.EncodeToString(secret),
		"unpadded base64":   base64.RawStdEncoding.EncodeToString(secret),
		"URL base64":        base64.URLEncoding.EncodeToString(secret),
		"unpadded URL b64":  base64.RawURLEncoding.EncodeToString(secret),
		"Go byte slice %v":  fmt.Sprintf("%v", secret),
		"Go byte slice %#v": fmt.Sprintf("%#v", secret),
	}
}

// countedIDs passes every call through to the view beneath it and keeps the
// master key IDs whose wrap counts were raised, the one value of the view a
// test cannot read back.
type countedIDs struct {
	*faultyLoad
	mu  sync.Mutex
	ids [][]byte
}

func (c *countedIDs) RaiseWrapCount(ctx context.Context, masterKeyID []byte, n, limit uint64) (bool, error) {
	c.mu.Lock()
	c.ids = append(c.ids, bytes.Clone(masterKeyID))
	c.mu.Unlock()
	return c.faultyLoad.RaiseWrapCount(ctx, masterKeyID, n, limit)
}

// A planted data key, chunking key, master key and passphrase never reach a
// log line, an error, a Status value or a stored record, wrap counts
// included, raw, in hex or in base64: the provider reports IDs and
// fingerprints only. Key material formatted into a message, or a master key
// persisted, fails here.
func TestPlantedKeyNeverLeaks(t *testing.T) {
	prof.Watch(t, prof.Options{})

	path, _, master := keyFile(t)
	plant := func() []byte {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			t.Fatal(err)
		}
		return k
	}
	dataKey, chunkingKey := plant(), plant()

	var log bytes.Buffer
	records := &countedIDs{faultyLoad: &faultyLoad{Records: keystest.NewRecords()}}
	deps := Deps{Records: records, Rand: rand.Reader, Logger: slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	ctx := context.Background()
	p, err := New(ctx, Config{KeyFile: path, Passphrase: passphrase}, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Each step is named and must succeed or be refused as it says, so a
	// refusal that stops happening leaves nothing unsearched unnoticed.
	var errs []error
	step := func(name string, refusal bool, err error) {
		t.Helper()
		switch {
		case refusal && err == nil:
			t.Fatalf("%s: no error, want a refusal to search", name)
		case !refusal && err != nil:
			t.Fatalf("%s: %v", name, err)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	get := func(name string, refusal bool, id keys.MaterialID) {
		t.Helper()
		_, err := p.Get(ctx, id)
		step(name, refusal, err)
	}

	ns := entity.NewNamespaceID()
	data := keys.MaterialID{Namespace: ns, Kind: keys.KindDataKey, ID: "d"}
	chunking := keys.MaterialID{Namespace: ns, Kind: keys.KindChunkingKey, ID: "c"}
	put(t, records, ns,
		wrapped(t, p, ns, data.Kind, data.ID, stateCurrent, dataKey),
		wrapped(t, p, ns, chunking.Kind, chunking.ID, stateCurrent, chunkingKey))
	step("Open", false, p.Open(ctx, ns))
	get("Get of the data key", false, data)
	get("Get of the chunking key", false, chunking)

	// The chunking key's record replaced by the data key's wrap under its ID.
	replaced := wrapped(t, p, ns, chunking.Kind, chunking.ID, stateCurrent, dataKey)
	r, err := decodeKeyRecord(replaced.Record)
	if err != nil {
		t.Fatalf("decode the replacing record: %v", err)
	}
	r.fingerprint = fingerprint(chunkingKey)
	replaced.Record = r.encode()
	put(t, records, ns, replaced)
	p.reload(ctx)
	step("Open over the replaced chunking key", true, p.Open(ctx, ns))
	get("Get of the replaced chunking key", true, chunking)
	_, err = p.Current(ctx, ns, chunking.Kind)
	step("Current of the replaced chunking key", true, err)

	other := entity.NewNamespaceID()
	unreadable := wrapped(t, p, other, keys.KindDataKey, "u", stateCurrent, dataKey)
	unreadable.Record = append([]byte{2}, unreadable.Record[1:]...)
	put(t, records, other, unreadable)
	get("Get of an unreadable record", true, keys.MaterialID{Namespace: other, Kind: keys.KindDataKey, ID: "u"})
	get("Get of an ID never given", true, keys.MaterialID{Namespace: ns, Kind: keys.KindDataKey, ID: "never"})
	records.fail.Store(true)
	get("Get during a Load outage", true, keys.MaterialID{Namespace: entity.NewNamespaceID(), Kind: keys.KindDataKey, ID: "o"})
	records.fail.Store(false)
	step("update marking the data key destroyed", false, p.update(ctx, ns, func([]Row) ([]Row, error) {
		return []Row{{Kind: data.Kind, ID: data.ID, State: stateDestroyed, Record: p.record(fingerprint(dataKey), nil, nil)}}, nil
	}))
	get("Get of the destroyed data key", true, data)

	_, err = New(ctx, Config{KeyFile: path, Passphrase: passphrase + "!"}, deps)
	step("New with a wrong passphrase", true, err)
	storage := deps
	storage.StorageKeys = []StorageKey{{ID: "storage", Fingerprint: fingerprint(master)}}
	_, err = New(ctx, Config{KeyFile: path, Passphrase: passphrase}, storage)
	step("New over a storage key's bytes", true, err)
	otherFile, _, _ := keyFile(t)
	_, err = p.VerifyEscrow(ctx, readFile(t, otherFile))
	step("VerifyEscrow of another key file", true, err)
	step("Reconfigure to another key file", true, p.Reconfigure(ctx, Config{KeyFile: otherFile, Passphrase: passphrase}))
	step("Reconfigure to an absent key file", true, p.Reconfigure(ctx, Config{KeyFile: filepath.Join(t.TempDir(), "absent.key"), Passphrase: passphrase}))

	if !strings.Contains(log.String(), "level=ERROR") {
		t.Fatalf("the steps above logged no Error line to search:\n%s", log.String())
	}
	st := p.Status()
	if len(st.Unavailable) == 0 || len(st.Unknown) == 0 {
		t.Fatalf("Status = %+v, want unavailable and unknown material to search", st)
	}

	places := map[string]string{"the log": log.String(), "Status": fmt.Sprintf("%+v %#v", st, st)}
	for i, err := range errs {
		places[fmt.Sprintf("error %d", i)] = fmt.Sprintf("%v %+v %#v", err, err, err)
	}
	for _, n := range []entity.NamespaceID{ns, other} {
		rows, err := records.Load(ctx, n)
		if err != nil {
			t.Fatalf("Load of namespace %s: %v", n, err)
		}
		for _, row := range rows {
			places[fmt.Sprintf("the record of %s %q", row.Kind, row.ID)] = fmt.Sprintf("%s %#v", row.Record, row)
		}
	}
	if len(records.ids) == 0 {
		t.Fatal("no wrap count was raised, want its master key IDs to search")
	}
	for i, id := range records.ids {
		places[fmt.Sprintf("wrap count %d's master key ID", i)] = string(id)
	}

	for secret, b := range map[string][]byte{"the data key": dataKey, "the chunking key": chunkingKey, "the master key": master, "the passphrase": []byte(passphrase)} {
		for form, needle := range encodings(b) {
			for place, text := range places {
				if strings.Contains(text, needle) {
					t.Errorf("%s holds %s, %s", place, secret, form)
				}
			}
		}
	}
}
