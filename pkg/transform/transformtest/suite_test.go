package transformtest

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// The correct test transforms pass the suite: frame, which uses no material,
// with the authenticating rows skipped, and seal, an encrypt-stage one, with
// them run.
func TestCorrectTransformsPassTheSuite(t *testing.T) {
	prof.Watch(t, prof.Options{})

	Run(t, frameSubject(asFrame))
	Run(t, sealSubject(fullMAC, asSeal))
}

// Every row fails on the broken subjects written for it, with the error it
// was written to give, and on no other; both correct transforms fail none;
// and every row has a broken subject that must fail it. Each row runs as Run
// runs it, through row.run.
func TestEachBrokenSubjectFailsItsRows(t *testing.T) {
	prof.Watch(t, prof.Options{})

	covered := make(map[string]bool)

	for _, b := range brokenSubjects() {
		if err := b.s.check(); err != nil {
			t.Fatalf("%s: %v", b.name, err)
		}

		for _, r := range rows {
			_, err := r.run(t.Context(), b.s)
			want, failing := b.want[r.name]

			switch {
			case err != nil && !failing:
				t.Errorf("%s, row %s: failed with %v, want it to pass", b.name, r.name, err)
			case err == nil && failing:
				t.Errorf("%s, row %s: passed, want it to fail with an error containing %q", b.name, r.name, want)
			case err != nil && (want == "" || !strings.Contains(err.Error(), want)):
				t.Errorf("%s, row %s: error %q, want one containing %q", b.name, r.name, err, want)
			case err != nil:
				t.Logf("%s, row %s: %v", b.name, r.name, err)
			}
		}

		for name := range b.want {
			covered[name] = true
		}
	}

	for _, r := range rows {
		if !covered[r.name] {
			t.Errorf("row %s has no broken subject that must fail it", r.name)
		}
	}
}

// The census a Decode is handed answers only the material it lists, of the
// namespace read, asking Memory for the census's entry whatever fingerprint
// the body's ID carries; any other ID, another namespace's of the same kind
// and ID included, and Current, are ErrMalformed and leave Memory uncalled.
func TestCensusAnswersOnlyWhatItLists(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := t.Context()
	mem := keystest.NewMemory()
	ns, other := entity.NamespaceID{15: 1}, entity.NamespaceID{15: 2}

	listedContent := bytes.Repeat([]byte{0xa1}, 32)
	listed := mem.Give(ns, sealKind, "listed", listedContent)
	unlisted := mem.Give(ns, sealKind, "unlisted", bytes.Repeat([]byte{0xb2}, 32))
	mem.Give(other, sealKind, "listed", bytes.Repeat([]byte{0xc3}, 32))

	view := census{mem, ns, []keys.MaterialID{listed}}

	before := mem.Calls()

	got, err := view.Get(ctx, keys.MaterialID{Namespace: ns, Kind: sealKind, ID: "listed"})
	if err != nil || !bytes.Equal(got, listedContent) {
		t.Errorf("Get of the listed ID by ID alone = %x, %v, want %x", got, err, listedContent)
	}

	if d := mem.Calls() - before; d != 1 {
		t.Errorf("Memory.Calls rose by %d over a listed Get, want 1", d)
	}

	for _, c := range []struct {
		name string
		id   keys.MaterialID
	}{
		{"an ID the census does not list", unlisted},
		{"the listed kind and ID of another namespace", keys.MaterialID{Namespace: other, Kind: sealKind, ID: "listed"}},
		{"the listed ID as another kind", keys.MaterialID{Namespace: ns, Kind: keys.KindDataKey, ID: "listed"}},
	} {
		before := mem.Calls()

		if _, err := view.Get(ctx, c.id); !errors.Is(err, transform.ErrMalformed) {
			t.Errorf("Get of %s: got %v, want %v", c.name, err, transform.ErrMalformed)
		}

		if d := mem.Calls() - before; d != 0 {
			t.Errorf("Get of %s: Memory.Calls rose by %d, want 0", c.name, d)
		}
	}

	before = mem.Calls()

	if _, err := view.Current(ctx, ns, sealKind); !errors.Is(err, transform.ErrMalformed) {
		t.Errorf("Current: got %v, want %v", err, transform.ErrMalformed)
	}

	if d := mem.Calls() - before; d != 0 {
		t.Errorf("Current: Memory.Calls rose by %d, want 0", d)
	}
}
