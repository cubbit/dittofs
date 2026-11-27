package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

func TestDeleteRefusesAndDestroysNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	f := newResolveFixture(t)
	owner := sealOwner()
	ref := sealRef(t)

	resolvePut(t, f.p, f.cp, entity.SecretMachinePassword, owner, ref, "v1", Meta{})
	resolvePut(t, f.p, f.cp, entity.SecretMachinePassword, owner, ref, "v2", Meta{})

	other := sealOwner()
	other.Key = "password"

	cases := []struct {
		name  string
		kind  entity.SecretKind
		owner entity.SecretOwner
		ref   entity.SecretRef
	}{
		{"its own kind and owner", entity.SecretMachinePassword, owner, ref},
		{"another kind", entity.SecretNTHash, owner, ref},
		{"another owner", entity.SecretMachinePassword, other, ref},
		{"a reference never written", entity.SecretMachinePassword, owner, sealRef(t)},
		{"the zero reference", entity.SecretMachinePassword, owner, entity.SecretRef{}},
	}

	for _, c := range cases {
		f.rec.reset()

		err := f.p.Delete(ctx, c.kind, c.owner, c.ref)
		if err == nil {
			t.Errorf("%s: Delete returned nil, want a refusal", c.name)

			continue
		}

		if !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s: %v; want errors.ErrUnsupported", c.name, err)
		}

		if !strings.Contains(err.Error(), "RFC 16 §3") {
			t.Errorf("%s: %q does not name RFC 16 §3", c.name, err)
		}

		if ops := f.rec.taken(); len(ops) != 0 {
			t.Errorf("%s: store ops %v, want none", c.name, ops)
		}
	}

	got, err := f.p.Resolve(ctx, Protocol, entity.SecretMachinePassword, owner, ref)
	if err != nil || string(got) != "v2" {
		t.Errorf("Resolve after Delete: %q, %v; want \"v2\"", got, err)
	}

	list, err := f.p.Versions(ctx, Protocol, entity.SecretMachinePassword, owner, ref)
	if err != nil {
		t.Fatalf("Versions after Delete: %v", err)
	}

	if len(list) != 2 {
		t.Fatalf("Versions after Delete: %d versions, want 2", len(list))
	}

	if list[0].Number != 2 || string(list[0].Value) != "v2" {
		t.Errorf("Versions after Delete: first %d %q, want 2 \"v2\"", list[0].Number, list[0].Value)
	}

	if list[1].Number != 1 || string(list[1].Value) != "v1" {
		t.Errorf(
			"Versions after Delete: second %d %q, want 1 \"v1\"",
			list[1].Number,
			list[1].Value,
		)
	}

	for _, v := range []uint64{1, 2} {
		if rawGet(t, f.db, secretKVKey(ref, v)) == nil {
			t.Errorf("version %d is gone from the store after Delete", v)
		}
	}
}
