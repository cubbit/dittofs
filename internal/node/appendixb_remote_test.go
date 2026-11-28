package node

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/node/content"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
)

// remoteTable is a table of the remote tier's sections alone.
func remoteTable() settings.Table {
	return settings.Table(content.Sections())
}

// remoteKeys is every key RFC 13 Appendix B gives the remote store and the
// namespace for the remote tier.
var remoteKeys = []string{
	"remote.block_format_version", "remote.block_target", "remote.bucket", "remote.credential",
	"remote.endpoint", "remote.prefix", "remote.region", "remote.storage_class", "remote.trust_roots",
}

// The remote tier's sections agree with Appendix B on every key's scope,
// class and write role, declare exactly its nine remote keys, and leave no
// remote key of Appendix B undeclared.
func TestAppendixBMatchesTheRemoteSections(t *testing.T) {
	t.Parallel()
	rows, err := parseAppendixB(readRFC13(t))
	if err != nil {
		t.Fatalf("parse Appendix B: %v", err)
	}

	failures, reported := compareAppendixB(t, rows, remoteTable())
	if len(failures) > 0 {
		t.Errorf("the remote sections and Appendix B disagree:\n%s", strings.Join(failures, "\n"))
	}
	for _, key := range reported {
		if strings.HasPrefix(key, "remote.") {
			t.Errorf("reported keys %v: %s is an Appendix B key the remote sections lack", reported, key)
		}
	}

	var got []string
	for _, k := range tableKeys(t, tableStore(t), remoteTable()) {
		got = append(got, k.Key)
	}
	slices.Sort(got)
	if !slices.Equal(got, remoteKeys) {
		t.Errorf("the remote sections declare %v, want %v", got, remoteKeys)
	}
}

// remoteFixture is a control plane holding one remote store's credential
// secrets, for the remote sections' writes to be judged over.
type remoteFixture struct {
	st *kvstore.Store
	tb settings.Table
	// creds are two references to remote credential secrets the store's
	// remote.credential row owns.
	creds [2]entity.SecretRef
}

// fixtureStore and fixtureNamespace are the instances a fixture writes.
var (
	fixtureStore     = entity.SettingScope{Kind: entity.ScopeRemote, ID: [16]byte{0xb7, 15: 1}}
	fixtureNamespace = entity.SettingScope{Kind: entity.ScopeNamespace, ID: [16]byte{0xb7, 15: 2}}
)

func newRemoteFixture(t *testing.T) remoteFixture {
	t.Helper()
	f := remoteFixture{
		st:    tableStore(t),
		tb:    remoteTable(),
		creds: [2]entity.SecretRef{{0xc1, 15: 1}, {0xc1, 15: 2}},
	}
	err := f.st.ControlPlane().Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		for _, ref := range f.creds {
			err := tx.PutSecret(entity.Secret{
				ID: ref, Kind: entity.SecretRemoteCredential, Version: 1, KeyID: "test",
				Owner: entity.SecretOwner{Scope: fixtureStore, Key: "remote.credential"},
				Value: []byte("sealed"),
			}, 0)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("put the credential secrets: %v", err)
	}
	return f
}

// refValue is a reference row's value: the JSON list of the ref's 16 bytes.
func refValue(t *testing.T, ref entity.SecretRef) string {
	t.Helper()
	b, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// put writes key's value at scope beneath Check, as a stored record holds it.
func (f remoteFixture) put(t *testing.T, scope entity.SettingScope, key, value string) {
	t.Helper()
	putBeneathCheck(t, f.st, entity.Setting{Scope: scope, Key: key, Value: []byte(value)})
}

// record writes a whole store record and its namespace beneath Check.
func (f remoteFixture) record(t *testing.T) {
	t.Helper()
	for key, value := range f.storeRecord(t) {
		f.put(t, fixtureStore, key, value)
	}
	f.put(t, fixtureNamespace, "remote.prefix", `"ns/"`)
}

// storeRecord is a store record holding its required keys and the region.
func (f remoteFixture) storeRecord(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"remote.endpoint":   `"https://s3.example.test"`,
		"remote.bucket":     `"bucket"`,
		"remote.credential": refValue(t, f.creds[0]),
		"remote.region":     `"us-east-1"`,
	}
}

// check judges w with the store holding content or not; a namespace holds
// content whatever it holds, as the API's content check answers every
// namespace today.
func (f remoteFixture) check(t *testing.T, w settings.Write, storeHolds bool) error {
	t.Helper()
	holds := func(scope entity.SettingScope) (bool, error) {
		if scope.Kind == entity.ScopeRemote {
			return storeHolds, nil
		}
		return true, nil
	}
	return f.st.ControlPlane().Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return f.tb.Check(tx, entity.RoleOperator, w, holds)
	})
}

// putAt is the put of key's value at scope.
func putAt(scope entity.SettingScope, key, value string) settings.Write {
	return settings.Put(entity.Setting{
		Scope: scope, Key: key, Value: []byte(value),
		Schema: settings.Versions()[entity.FormatSettings].Max,
	})
}

// judged fails unless err wraps want and its text holds each of names; want
// nil asks for an accepted write.
func judged(t *testing.T, what string, err, want error, names ...string) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Errorf("%s: refused %v, want accepted", what, err)
		}
		return
	}
	if !errors.Is(err, want) {
		t.Errorf("%s: error %v, want %v", what, err, want)
		return
	}
	for _, n := range names {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("%s: refusal %q does not name %q", what, err, n)
		}
	}
}

// A record that omits each required key is refused, naming it.
func TestRemoteSectionsRequireTheirKeys(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"remote.endpoint", "remote.bucket", "remote.credential"} {
		t.Run(missing, func(t *testing.T) {
			t.Parallel()
			f := newRemoteFixture(t)
			for key, value := range f.storeRecord(t) {
				if key != missing {
					f.put(t, fixtureStore, key, value)
				}
			}
			err := f.check(t, putAt(fixtureStore, "remote.region", `"eu-west-1"`), false)
			judged(t, "a record without "+missing, err, settings.ErrRequired, missing)
		})
	}

	f := newRemoteFixture(t)
	f.record(t)
	err := f.check(t, settings.Delete(fixtureNamespace, "remote.prefix"), false)
	judged(t, "a namespace without remote.prefix", err, settings.ErrRequired, "remote.prefix")
}

// Each write of a remote key Check judges: the bound and location gates, the
// block target's range, the reference and the block format version.
func TestRemoteSectionsCheck(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		write      func(f remoteFixture) settings.Write
		storeHolds bool
		want       error
		names      []string
	}{
		{"region changed on a store holding content", func(remoteFixture) settings.Write {
			return putAt(fixtureStore, "remote.region", `"eu-west-1"`)
		}, true, nil, nil},
		{"trust roots changed back to the system's on a store holding content", func(remoteFixture) settings.Write {
			return putAt(fixtureStore, "remote.trust_roots", `""`)
		}, true, nil, nil},
		{"endpoint changed on a store holding content", func(remoteFixture) settings.Write {
			return putAt(fixtureStore, "remote.endpoint", `"https://other.example.test"`)
		}, true, settings.ErrUnproven, []string{"remote.endpoint"}},
		{"block target 512 KiB", func(remoteFixture) settings.Write {
			return putAt(fixtureStore, "remote.block_target", `"512KiB"`)
		}, false, settings.ErrInvalid, []string{"remote", "block_target", "1.00MiB to 64.00MiB"}},
		{"block target 128 MiB", func(remoteFixture) settings.Write {
			return putAt(fixtureStore, "remote.block_target", `"128MiB"`)
		}, false, settings.ErrInvalid, []string{"remote", "block_target", "1.00MiB to 64.00MiB"}},
		{"block target 1 MiB", func(remoteFixture) settings.Write {
			return putAt(fixtureStore, "remote.block_target", `"1MiB"`)
		}, false, nil, nil},
		{"block target 64 MiB", func(remoteFixture) settings.Write {
			return putAt(fixtureStore, "remote.block_target", `"64MiB"`)
		}, false, nil, nil},
		{"bucket changed with content", func(remoteFixture) settings.Write {
			return putAt(fixtureStore, "remote.bucket", `"other"`)
		}, true, settings.ErrBound, []string{"remote.bucket"}},
		{"prefix changed with content", func(remoteFixture) settings.Write {
			return putAt(fixtureNamespace, "remote.prefix", `"other/"`)
		}, true, settings.ErrBound, []string{"remote.prefix"}},
		{"endpoint changed without content", func(remoteFixture) settings.Write {
			return putAt(fixtureStore, "remote.endpoint", `"https://other.example.test"`)
		}, false, settings.ErrUnproven, []string{"remote.endpoint"}},
		{"credential changed without content", func(f remoteFixture) settings.Write {
			return putAt(fixtureStore, "remote.credential", refValue(t, f.creds[1]))
		}, false, settings.ErrUnproven, []string{"remote.credential"}},
		{"bucket changed without content", func(remoteFixture) settings.Write {
			return putAt(fixtureStore, "remote.bucket", `"other"`)
		}, false, settings.ErrUnproven, []string{"remote.bucket"}},
		{"prefix changed on a namespace, which holds content to the API", func(remoteFixture) settings.Write {
			return putAt(fixtureNamespace, "remote.prefix", `"other/"`)
		}, false, settings.ErrBound, []string{"remote.prefix"}},
		{"plaintext in the credential", func(remoteFixture) settings.Write {
			return putAt(fixtureStore, "remote.credential", `"AKIAPLAINTEXT:hunter2"`)
		}, false, settings.ErrInvalid, []string{"remote.credential", "must be a secret reference"}},
		{"block format version 2", func(remoteFixture) settings.Write {
			return putAt(fixtureNamespace, "remote.block_format_version", `2`)
		}, false, settings.ErrInvalid, []string{"remote.block_format_version"}},
		{"block format version 0", func(remoteFixture) settings.Write {
			return putAt(fixtureNamespace, "remote.block_format_version", `0`)
		}, false, settings.ErrInvalid, []string{"remote.block_format_version"}},
		{"block format version 1", func(remoteFixture) settings.Write {
			return putAt(fixtureNamespace, "remote.block_format_version", `1`)
		}, false, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRemoteFixture(t)
			f.record(t)
			err := f.check(t, tc.write(f), tc.storeHolds)
			judged(t, tc.name, err, tc.want, tc.names...)
			if err != nil && strings.Contains(err.Error(), "hunter2") {
				t.Errorf("%s: refusal %q prints the plaintext", tc.name, err)
			}
		})
	}
}

// Each first value is accepted as the API's create judges it: the record
// written whole, then each of its keys judged as a put, the namespace holding
// content as the API answers every namespace.
func TestRemoteSectionsAcceptFirstValues(t *testing.T) {
	t.Parallel()
	f := newRemoteFixture(t)
	f.record(t)
	for key, value := range f.storeRecord(t) {
		judged(t, "the first "+key, f.check(t, putAt(fixtureStore, key, value), false), nil)
	}
	judged(t, "the first remote.prefix", f.check(t, putAt(fixtureNamespace, "remote.prefix", `"ns/"`), false), nil)
}

// readRemote reads the remote sections over f's store, failing on any
// section Read refuses.
func (f remoteFixture) readRemote(t *testing.T) *settings.Snapshot {
	t.Helper()
	snap, refused, err := settings.Read(t.Context(), f.st.Nodes(), f.tb, probeNode)
	if err != nil {
		t.Fatalf("read the remote sections: %v", err)
	}
	for sec, err := range refused {
		t.Fatalf("section %s refused: %v", sec, err)
	}
	return snap
}

// changedKeys is each key Diff lists from prev to cur.
func changedKeys(prev, cur *settings.Snapshot) []string {
	var out []string
	for _, ch := range settings.Diff(prev, cur) {
		out = append(out, ch.Key)
	}
	return out
}

// A record holding only the required keys reads as one holding Appendix B's
// defaults written out, so no key's value differs between the two; a value
// other than the default does differ, so the comparison can see one.
func TestRemoteSectionsDefaults(t *testing.T) {
	t.Parallel()
	f := newRemoteFixture(t)
	f.put(t, fixtureStore, "remote.endpoint", `"https://s3.example.test"`)
	f.put(t, fixtureStore, "remote.bucket", `"bucket"`)
	f.put(t, fixtureStore, "remote.credential", refValue(t, f.creds[0]))
	f.put(t, fixtureNamespace, "remote.prefix", `"ns/"`)
	required := f.readRemote(t)

	for key, value := range map[string]string{
		"remote.region":        `"us-east-1"`,
		"remote.trust_roots":   `""`,
		"remote.storage_class": `"STANDARD"`,
		"remote.block_target":  `"4MiB"`,
	} {
		f.put(t, fixtureStore, key, value)
	}
	f.put(t, fixtureNamespace, "remote.block_format_version", `1`)
	written := f.readRemote(t)
	if got := changedKeys(required, written); len(got) > 0 {
		t.Errorf("Appendix B's defaults written out change %v from the record of required keys alone", got)
	}

	f.put(t, fixtureStore, "remote.block_target", `"8MiB"`)
	eight := f.readRemote(t)
	if got := changedKeys(written, eight); !slices.Equal(got, []string{"remote.block_target"}) {
		t.Errorf("block target 8MiB over 4MiB changes %v, want [remote.block_target]", got)
	}
	f.put(t, fixtureStore, "remote.block_target", `"8192KiB"`)
	if got := changedKeys(eight, f.readRemote(t)); len(got) > 0 {
		t.Errorf("block target 8192KiB over 8MiB changes %v, want none", got)
	}
}

// A block target out of range in the store is refused at start, through Read,
// naming the key and the range, never clamped.
func TestRemoteSectionsReadRefusesBlockTarget(t *testing.T) {
	t.Parallel()
	f := newRemoteFixture(t)
	f.record(t)
	f.put(t, fixtureStore, "remote.block_target", `"128MiB"`)
	_, refused, err := settings.Read(t.Context(), f.st.Nodes(), f.tb, probeNode)
	if err != nil {
		t.Fatalf("read the remote sections: %v", err)
	}
	if got := slices.Sorted(maps.Keys(refused)); !slices.Equal(got, []string{"remote"}) {
		t.Fatalf("refused sections %v, want [remote]", got)
	}
	for _, n := range []string{"block_target", "128.00MiB", "1.00MiB to 64.00MiB"} {
		if !strings.Contains(refused["remote"].Error(), n) {
			t.Errorf("Read's refusal %q does not name %q", refused["remote"], n)
		}
	}
}

// No remote section prints its credential's reference in a refusal, and the
// one reference field is the credential.
func TestNoRemoteRefusalPrintsAReference(t *testing.T) {
	t.Parallel()
	refs, failures := refLeaks(t, remoteTable(), []entity.Setting{
		{Scope: probeRemote, Key: "remote.block_target", Value: []byte(`"128MiB"`)},
	})
	if len(failures) > 0 {
		t.Errorf("reference search:\n%s", strings.Join(failures, "\n"))
	}
	want := map[string][]string{"remote": {"remote.credential"}}
	if !maps.EqualFunc(refs, want, slices.Equal) {
		t.Errorf("reference fields found %v, want %v", refs, want)
	}
}
