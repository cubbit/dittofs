package settings_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/settings"
)

// A snapshot lists every reference it holds with the section, key, scope and
// kind of each: the base, each instance's and each thing's, in declaration
// order; a zero reference names none.
func TestSnapshotSecretsListsEachReference(t *testing.T) {
	st := newRdStore(t)

	table := settings.Table{
		settings.Of("cred", settings.At(entity.ScopeInstallation),
			rdCred{Limit: 1}, settings.SecretRef("Ref", entity.SecretRemoteCredential)),
		settings.Of("store", settings.At(entity.ScopeShare),
			rdCred{Limit: 1}, settings.SecretRef("Ref", entity.SecretRemoteCredential)),
		settings.Of("tape", settings.PerThing(entity.ScopeNode, "journal"),
			rdCred{Limit: 1}, settings.SecretRef("Ref", entity.SecretRemoteCredential)),
	}

	baseRef, shareRef, otherShareRef, jRef, otherJRef :=
		entity.SecretRef{0x11}, entity.SecretRef{0x22}, entity.SecretRef{0x33},
		entity.SecretRef{0x44}, entity.SecretRef{0x55}

	rows := []entity.Setting{
		{Scope: rdInstallation, Key: "cred.ref", Value: refRow(baseRef)},
		{Scope: rdShareScope(2), Key: "store.ref", Value: refRow(shareRef)},
		{Scope: rdShareScope(3), Key: "store.ref", Value: refRow(otherShareRef)},
		{Scope: rdNodeScope(rdNodeA), Key: "tape.dev.ref", Value: refRow(jRef)},
		{Scope: rdNodeScope(rdNodeA), Key: "tape.early.ref", Value: refRow(otherJRef)},
		{Scope: rdNodeScope(rdNodeB), Key: "tape.dev.ref", Value: refRow(entity.SecretRef{0x66})},
	}
	for _, row := range rows {
		st.raw(t, row.Scope, row.Key, rdRow(string(row.Value), 1))
	}

	snap, refused := rdRead(t, st.nodes, table, rdNodeA)
	if len(refused) != 0 {
		t.Fatalf("the walk refused sections: %v, want none", refused)
	}

	// A share with no rows runs the base, which names no instance, so it
	// lists nothing; a section with no rows runs the zero default.
	got := snap.Secrets()
	want := []settings.SecretUse{
		{
			Section: "cred",
			Key:     "cred.ref",
			Scope:   rdInstallation,
			Kind:    entity.SecretRemoteCredential,
			Ref:     baseRef,
		},
		{
			Section: "store",
			Key:     "store.ref",
			Scope:   rdShareScope(2),
			Kind:    entity.SecretRemoteCredential,
			Ref:     shareRef,
		},
		{
			Section: "store",
			Key:     "store.ref",
			Scope:   rdShareScope(3),
			Kind:    entity.SecretRemoteCredential,
			Ref:     otherShareRef,
		},
		{
			Section: "tape",
			Key:     "tape.dev.ref",
			Scope:   rdNodeScope(rdNodeA),
			Kind:    entity.SecretRemoteCredential,
			Ref:     jRef,
		},
		{
			Section: "tape",
			Key:     "tape.early.ref",
			Scope:   rdNodeScope(rdNodeA),
			Kind:    entity.SecretRemoteCredential,
			Ref:     otherJRef,
		},
	}

	// Node B's row is not this snapshot's, so its reference lists none. A
	// section at its defaults names no reference either: the zero value of
	// a secret-ref field lists nothing.
	if !slices.Equal(want, got) {
		t.Fatalf("the snapshot's references: got %+v, want %+v", got, want)
	}

	table = append(table, settings.Of("dumb", settings.At(entity.ScopeInstallation),
		rdCred{Limit: 1}, settings.SecretRef("Ref", entity.SecretRemoteCredential)))

	snap, refused = rdRead(t, st.nodes, table, rdNodeA)
	if len(refused) != 0 {
		t.Fatalf("the walk refused sections: %v, want none", refused)
	}

	if got := snap.Secrets(); len(got) != 5 || got[len(got)-1].Section != "tape" {
		t.Fatalf(
			"the references of a section at its zero default: got %+v,"+
				" want the five the rows name and none of it",
			got,
		)
	}
}

// refRow is ref's JSON, as a stored row holds a reference.
func refRow(ref entity.SecretRef) []byte {
	b, err := json.Marshal(ref)
	if err != nil {
		panic(err)
	}

	return b
}
