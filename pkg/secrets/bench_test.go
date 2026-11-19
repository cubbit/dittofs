package secrets

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// genWriters is how many writers commit at once.
const genWriters = 8

// genKind is the kind every writer and the model record seal: a kind that
// takes the protocol role and is not a remote store's credential, which Seal
// refuses a new version of.
const genKind = entity.SecretNTHash

// genRig is a store with eight writers' references and the stored bytes of a
// model record, which the baseline writes in place of PutSecret.
type genRig struct {
	db    kv.KV
	cp    kvstore.ControlPlane
	st    *kvstore.Store
	p     *Provider
	refs  [genWriters]entity.SecretRef
	model []byte
}

// newGenRig builds the rig outside any timer. The model is a first version
// sealed and committed under another reference with the writers' kind and
// owner, and its stored bytes are read back: the record holds no reference
// or version, so it decodes under any reference, which the baseline's raw
// write needs, since Seal reads the current version through the view.
func newGenRig(tb testing.TB) *genRig {
	tb.Helper()

	ctx := context.Background()
	db := sealStore(tb, tb.TempDir())
	cp := sealControlPlane(tb, db)

	st, err := kvstore.Open(ctx, db)
	if err != nil {
		tb.Fatalf("open the store a second time: %v", err)
	}

	r := &genRig{
		db: db,
		cp: cp,
		st: st,
		p:  sealProvider(tb, cp, sealKeys(tb)),
	}

	for i := range r.refs {
		r.refs[i] = sealRef(tb)
	}

	modelRef := sealRef(tb)

	s, err := r.p.Seal(ctx, Protocol, genKind, sealOwner(), modelRef, genValue(), Meta{})
	if err != nil {
		tb.Fatalf("seal the model: %v", err)
	}

	err = cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return tx.PutSecret(s, 0)
	})
	if err != nil {
		tb.Fatalf("commit the model: %v", err)
	}

	err = db.View(ctx, func(rd kv.Reader) error {
		r.model, err = rd.Get(genKey(modelRef, 1))

		return err
	})
	if err != nil || len(r.model) == 0 {
		tb.Fatalf("read the model's stored bytes: %v (%d bytes)", err, len(r.model))
	}

	return r
}

func genValue() []byte {
	return bytes.Repeat([]byte{'x'}, 32)
}

// genKey is the key of one version of a secret, as the store lays it out.
func genKey(ref entity.SecretRef, version uint64) []byte {
	return keys.Uint64(keys.ID([]byte{keys.KindSEC}, ref[:]), version)
}

// genRun commits the given number of new versions, eight writers at once,
// writer w taking commits w, w+8, w+16 and so on, each under its own
// reference. A commit is a Seal and then either the store's PutSecret, which
// raises the settings generation, or, when raise is false, a raw write of
// the model's stored bytes at the version's key, which raises nothing. It
// returns how many commits succeeded and the writers' errors joined.
func genRun(ctx context.Context, r *genRig, raise bool, commits int) (int, error) {
	var (
		wg   sync.WaitGroup
		done [genWriters]int
		errs [genWriters]error
	)

	for w := range genWriters {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := w; i < commits; i += genWriters {
				if err := genCommit(ctx, r, raise, r.refs[w]); err != nil {
					errs[w] = err

					return
				}

				done[w]++
			}
		}()
	}

	wg.Wait()

	total := 0
	for _, n := range done {
		total += n
	}

	return total, errors.Join(errs[:]...)
}

func genCommit(ctx context.Context, r *genRig, raise bool, ref entity.SecretRef) error {
	s, err := r.p.Seal(ctx, Protocol, genKind, sealOwner(), ref, genValue(), Meta{})
	if err != nil {
		return err
	}

	if !raise {
		return r.db.Update(ctx, func(tx kv.Txn) error {
			return tx.Set(genKey(ref, s.Version), r.model)
		})
	}

	return r.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return tx.PutSecret(s, s.Version-1)
	})
}

// BenchmarkSettingsGenerationCost is the cost of the settings generation's
// single counter: the commit rate of eight concurrent Seal and PutSecret
// writers, with the raise (mode=raise) and without it (mode=baseline). One
// iteration is one commit, so allocs/op is allocations per commit, counted
// over the whole process. Compare with: benchstat -col /mode bench.txt, and
// read the commits/s of mode=raise against that of mode=baseline. The temp
// dir is often tmpfs, where a sync costs nothing: point TMPDIR at a directory
// on the device under test.
func BenchmarkSettingsGenerationCost(b *testing.B) {
	modes := []struct {
		name  string
		raise bool
	}{
		{
			name:  "raise",
			raise: true,
		},
		{
			name:  "baseline",
			raise: false,
		},
	}

	for _, m := range modes {
		b.Run("mode="+m.name, func(b *testing.B) {
			ctx := context.Background()
			r := newGenRig(b)

			before, err := r.st.Nodes().Counters(ctx)
			if err != nil {
				b.Fatalf("read the settings generation: %v", err)
			}

			b.ReportAllocs()
			b.ResetTimer()

			done, err := genRun(ctx, r, m.raise, b.N)
			elapsed := b.Elapsed()

			b.StopTimer()

			if err != nil {
				b.Fatalf("writers: %v", err)
			}

			if done != b.N {
				b.Fatalf("committed %d versions, want %d", done, b.N)
			}

			after, err := r.st.Nodes().Counters(ctx)
			if err != nil {
				b.Fatalf("read the settings generation: %v", err)
			}

			// Each sub-benchmark has its own store and no other writer, so the
			// generation moves by exactly the raises of this run. What it must
			// do follows the mode's name, not its flag, so a row whose flag
			// disagrees with its name is caught.
			want := uint64(0)
			if m.name == "raise" {
				want = uint64(b.N)
			}

			if got := after.Settings - before.Settings; got != want {
				b.Fatalf("mode %s: the settings generation moved by %d, want %d", m.name, got, want)
			}

			b.ReportMetric(float64(done)/elapsed.Seconds(), "commits/s")
		})
	}
}

// The baseline must not raise the settings generation and the raise mode must,
// once per commit, or the benchmark compares nothing.
func TestSettingsGenRunRaisesOnlyInRaiseMode(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const commits = 24

	ctx := context.Background()

	for _, m := range []struct {
		name  string
		raise bool
		want  uint64
	}{
		{
			name:  "raise",
			raise: true,
			want:  commits,
		},
		{
			name:  "baseline",
			raise: false,
			want:  0,
		},
	} {
		t.Run(m.name, func(t *testing.T) {
			r := newGenRig(t)

			before, err := r.st.Nodes().Counters(ctx)
			if err != nil {
				t.Fatal(err)
			}

			done, err := genRun(ctx, r, m.raise, commits)
			if err != nil {
				t.Fatal(err)
			}

			if done != commits {
				t.Errorf("committed %d versions, want %d", done, commits)
			}

			after, err := r.st.Nodes().Counters(ctx)
			if err != nil {
				t.Fatal(err)
			}

			if got := after.Settings - before.Settings; got != m.want {
				t.Errorf("the settings generation moved by %d, want %d", got, m.want)
			}

			for i, ref := range r.refs {
				cur, err := r.cp.Secret(ctx, ref, 0)
				if err != nil {
					t.Fatalf("writer %d: read the current version: %v", i, err)
				}

				if want := uint64(commits / genWriters); cur.Version != want {
					t.Errorf("writer %d holds version %d, want %d", i, cur.Version, want)
				}

				if _, err := r.cp.Secret(ctx, ref, 1); err != nil {
					t.Errorf("writer %d: version 1 not held: %v", i, err)
				}
			}
		})
	}
}
