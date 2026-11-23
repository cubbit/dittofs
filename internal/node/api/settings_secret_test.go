package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/settings"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
)

// The directory section's two keys as the settings-secret tests write them.
const (
	dirHost = "dir.host"
	dirCred = "dir.cred"
)

// installScope is the one scope the section's rows live at.
var installScope = entity.SettingScope{Kind: entity.ScopeInstallation}

// dirFields is a directory section as these tests declare it: a host and a
// bind credential, whose Validate refuses a credential with no host,
// printing the whole record, which a reference field never holds a value of.
type dirFields struct {
	Host string           `json:"host" class:"live"`
	Cred entity.SecretRef `json:"cred" class:"live"`
}

// Validate refuses a credential set beside no host.
func (d dirFields) Validate() error {
	if d.Cred != (entity.SecretRef{}) && d.Host == "" {
		return fmt.Errorf("a directory credential states its host too, record %+v", d)
	}

	return nil
}

// dirSection is the directory section these tests write: an admin-only
// record whose bind credential is a directory-credential reference.
func dirSection() settings.Section {
	return settings.Of("dir", settings.At(entity.ScopeInstallation), dirFields{},
		settings.WriteRole(entity.RoleAdmin),
		settings.SecretRef("Cred", entity.SecretDirectoryCredential))
}

// dirServer is the API as the settings-secret tests drive it: served as the
// handler tests serve it, over the directory section and a recording sealer,
// its store Badger on disk behind a FaultKV, so a test can fail one row's
// set and search the raw keys and values.
type dirServer struct {
	*served

	kv    kv.KV
	fault *kvtest.FaultKV
	seals *sealRecorder
}

// dirServe serves the API as role over the directory section, a real
// provider's sealer that records each seal, and Badger on disk behind a
// FaultKV, each further edit on top. One fixed account signs every call of
// one server, so a retried request replays. The settings table is the
// edit's to replace, for the tests that write a real section.
func dirServe(t *testing.T, role entity.APIRole, edit ...func(*api.Deps)) *dirServer {
	t.Helper()

	disk := newOnDisk(t)
	authenticate, _ := one(role)
	seals := &sealRecorder{}

	edits := append([]func(*api.Deps){func(d *api.Deps) {
		disk.use(d)
		d.Settings = settings.Table{dirSection()}
		seals.credSealer = storeSealer(t, d)
		d.Sealer = seals
	}}, edit...)

	s := serve(t, authenticate, edits...)

	return &dirServer{served: s, kv: disk.raw, fault: disk.fault, seals: seals}
}

// sealRecorder is a real provider's sealer as these tests hand it in,
// keeping every sealed version it returned, so a test can count the seals
// and read the reference one was sealed under: the seal is the count being
// asserted, so the recorder holds every call and answers none in place.
type sealRecorder struct {
	credSealer

	mu       sync.Mutex
	recorded []entity.Secret
}

// Seal seals as the provider does and records what it returned.
func (s *sealRecorder) Seal(
	ctx context.Context,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
	value []byte,
	handedOver bool,
) (entity.Secret, error) {
	sec, err := s.credSealer.Seal(ctx, kind, owner, ref, value, handedOver)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err == nil {
		s.recorded = append(s.recorded, sec)
	}

	return sec, err
}

// seals is every version the sealer sealed, in the order it returned them.
func (s *sealRecorder) seals() []entity.Secret {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.recorded)
}

// sealedVersions is how many sealed secret versions the raw KV holds.
func (d *dirServer) sealedVersions(t *testing.T) int {
	t.Helper()

	n := 0

	if err := d.kv.View(t.Context(), func(r kv.Reader) error {
		for _, err := range r.Scan([]byte{keys.KindSEC}, nil) {
			if err != nil {
				return err
			}

			n++
		}

		return nil
	}); err != nil {
		t.Fatalf("count the sealed versions: %v", err)
	}

	return n
}

// leak returns every place value appears among what the served writes left:
// the raw KV's keys and values, the captured log, the metrics the server
// gathers, and each further body the test names by its place. No place at
// all is a clean write.
func (d *dirServer) leak(t *testing.T, value []byte, bodies map[string][]byte) []string {
	t.Helper()

	var where []string

	if err := d.kv.View(t.Context(), func(r kv.Reader) error {
		for kve, err := range r.Scan(nil, nil) {
			if err != nil {
				return err
			}

			if bytes.Contains(kve.Key, value) || bytes.Contains(kve.Value, value) {
				where = append(where, "the raw kv")

				return nil
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("scan the raw KV: %v", err)
	}

	if bytes.Contains([]byte(d.log.String()), value) {
		where = append(where, "the log")
	}

	if m := d.metricsText(t); bytes.Contains(m, value) {
		where = append(where, "the metrics")
	}

	for name, body := range bodies {
		if bytes.Contains(body, value) {
			where = append(where, name)
		}
	}

	slices.Sort(where)

	return slices.Compact(where)
}

// metricsText is every metric the server gathered, in one plain rendering
// the searches read.
func (d *dirServer) metricsText(t *testing.T) []byte {
	t.Helper()

	mfs, err := d.reg.Registry().Gather()
	if err != nil {
		t.Fatalf("gather the metrics: %v", err)
	}

	return []byte(fmt.Sprint(mfs))
}

// seedHost writes the section's host row straight through the store, as an
// admin write that already succeeded would have left it, so a caller below
// the section's write role meets a record that holds one.
func (d *dirServer) seedHost(t *testing.T) {
	t.Helper()

	st := entity.Setting{Scope: installScope, Key: dirHost, Value: []byte(`"host-a"`)}
	if _, err := d.store.PutSetting(t.Context(), st, 0); err != nil {
		t.Fatalf("seed the host row: %v", err)
	}
}

// putCred PUTs the credential's value with the headers hdr names in pairs.
func (d *dirServer) putCred(t *testing.T, value string, hdr ...string) reply {
	t.Helper()

	return d.do(
		t,
		http.MethodPut,
		"/api/v1/settings/"+dirCred,
		`{"spec":{"value":"`+value+`"}}`,
		hdr...)
}

// refVersionAnswer decodes a credential setting's answer, as PUT and GET
// answer it alike: its spec's value exactly a reference and a version. It
// fails the test when the answer holds anything else.
func refVersionAnswer(t *testing.T, body []byte) (entity.SecretRef, uint64) {
	t.Helper()

	var got apiv1.Setting

	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the answer %s: %v", body, err)
	}

	var ref apiv1.SecretRef

	if err := json.Unmarshal(got.Spec.Value, &ref); err != nil || ref.Ref == "" ||
		ref.Version == 0 {
		t.Fatalf(
			"the answer's value %s: want a reference and a version, err %v",
			got.Spec.Value,
			err,
		)
	}

	id, err := entity.ParseShareID(ref.Ref)
	if err != nil {
		t.Fatalf("the answer's reference %s: %v", ref.Ref, err)
	}

	return entity.SecretRef(id), ref.Version
}

// audit is GET /audit's answer, the whole first page.
func (d *dirServer) audit(t *testing.T) []byte {
	t.Helper()

	r := d.do(t, http.MethodGet, "/api/v1/audit", "")

	if r.status != http.StatusOK {
		t.Fatalf("GET /audit: answered %d %s, want 200", r.status, r.body)
	}

	return r.body
}

// A secret written through the settings route leaves no byte of its value in
// anything the write reached: not the raw store, keys or values, not the
// answer, not the audit records the raw store holds, not the captured log,
// not the metrics gathered. The sealed version is in the store, so the
// search guards a value that reached anything.
func TestSettingSecretLeavesNoTrace(t *testing.T) {
	const canary = "dir-bind-password-canary"

	s := dirServe(t, entity.RoleAdmin)
	s.seedHost(t)

	r := s.putCred(t, canary, "If-None-Match", "*")
	if r.status != http.StatusOK {
		t.Fatalf("PUT the credential: answered %d %s, want 200", r.status, r.body)
	}

	ref, status := refVersionAnswer(t, r.body)

	if status != 1 || ref == (entity.SecretRef{}) {
		t.Fatalf("the answer: reference %v version %d, want one at 1", ref, status)
	}

	if sec, err := s.store.Secret(t.Context(), ref, 0); err != nil ||
		sec.Kind != entity.SecretDirectoryCredential || sec.Version != 1 {
		t.Fatalf(
			"the sealed credential: %+v err %v, want version 1 of the directory kind",
			sec,
			err,
		)
	}

	if where := s.leak(t, []byte(canary), map[string][]byte{"the answer": r.body}); len(
		where,
	) != 0 {
		t.Errorf("the value reached %s", where)
	}
}

// Each refused write of a secret leaves no byte of its value either: by an
// operator below the section's role, under an unknown key, and a credential
// the section's Validate refuses for its missing host. Each refuses without
// the value and writes no sealed version.
func TestRefusedSecretWriteLeavesNoTrace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    string
		value  string
		field  string
		role   entity.APIRole
		host   bool
		status int
		code   string
		seals  int
	}{
		{
			"an operator below the role", dirCred, "refused-bind-canary", dirCred,
			entity.RoleOperator, true, http.StatusForbidden, "forbidden", 1,
		},
		{
			"an unknown key", "dir.nope", "refused-bind-canary", "", entity.RoleAdmin, true,
			http.StatusBadRequest, "unknown_key", 0,
		},
		{
			"a credential with no host", dirCred, "refused-bind-canary", "",
			entity.RoleAdmin, false, http.StatusBadRequest, "invalid", 1,
		},
		{
			"an empty value", dirCred, "", "spec.value",
			entity.RoleAdmin, true, http.StatusBadRequest, "invalid", 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := dirServe(t, tc.role)
			if tc.host {
				s.seedHost(t)
			}

			before := s.sealedVersions(t)
			r := s.do(t, http.MethodPut, "/api/v1/settings/"+tc.key,
				`{"spec":{"value":"`+tc.value+`"}}`, "If-None-Match", "*")

			p := r.problem(t)
			if r.status != tc.status || string(p.Code) != tc.code ||
				(tc.field != "" && p.Field != tc.field) {
				t.Fatalf(
					"PUT %s: answered %d %s, want %d %s naming %s",
					tc.key,
					r.status,
					r.body,
					tc.status,
					tc.code,
					tc.field,
				)
			}

			// The empty value alone is refused before the sealer, so an
			// empty string reaches no wrapping key: LDAP reads a bind with
			// an empty password as the anonymous bind, and no credential may
			// be written as one.
			if seals := s.seals.seals(); tc.value == "" && len(seals) != 0 {
				t.Errorf("the refused empty put sealed %d versions, want exactly 0", len(seals))
			}

			if tc.value != "" {
				leaks := s.leak(t, []byte(tc.value), map[string][]byte{"the answer": r.body})
				if len(leaks) != 0 {
					t.Errorf("the value reached %s", leaks)
				}
			}

			if s.sealedVersions(t) != before {
				t.Errorf(
					"sealed versions after the refused put: %d, want the %d there were",
					s.sealedVersions(t),
					before,
				)
			}
		})
	}
}

// An operator's write of the real directory section's bind credential is
// refused by the table's write role and seals nothing into the store: no
// sealed version and no row.
func TestOperatorCannotWriteASecret(t *testing.T) {
	const canary = "operator-bind-canary"

	s := dirServe(t, entity.RoleOperator, func(d *api.Deps) {
		d.Settings = settings.Table{identity.DirectorySection()}
	})

	before := s.sealedVersions(t)
	r := s.do(t, http.MethodPut, "/api/v1/settings/identity.directory.bind_credential",
		`{"spec":{"value":"`+canary+`"}}`, "If-None-Match", "*")

	if p := r.problem(t); r.status != http.StatusForbidden || p.Code != "forbidden" ||
		p.Field != "identity.directory.bind_credential" {
		t.Fatalf(
			"PUT the bind credential as an operator: %d %s, want 403 forbidden naming the key",
			r.status,
			r.body,
		)
	}

	if s.sealedVersions(t) != before {
		t.Errorf(
			"sealed versions after the refused put: %d, want the %d there were",
			s.sealedVersions(t),
			before,
		)
	}

	if _, ok := s.settingRow(t, installScope, "identity.directory.bind_credential"); ok {
		t.Error("the refused put wrote the credential's row")
	}

	if where := s.leak(t, []byte(canary), map[string][]byte{"the answer": r.body}); len(
		where,
	) != 0 {
		t.Errorf("the value reached %s", where)
	}
}

// The sealed version commits only with the row naming it: a fault failing
// the row's set fails the whole write, so neither the row nor the sealed
// version reaches the store, though the step sealed exactly one.
func TestSecretSealCommitsWithItsWrite(t *testing.T) {
	const canary = "faulted-bind-canary"

	s := dirServe(t, entity.RoleAdmin)
	s.seedHost(t)

	before := s.sealedVersions(t)
	row := keys.String([]byte{keys.KindCFG, byte(entity.ScopeInstallation)}, []byte(dirCred))
	fault := sim.Rule{
		Op: kvtest.OpSet, Key: keys.Describe(row),
		Action: sim.FaultAction{Err: errors.New("injected")},
	}

	sched, err := sim.NewSchedule(1, nil, fault)
	if err != nil {
		t.Fatalf("fault schedule: %v", err)
	}

	s.fault.SetFault(sched)

	r := s.putCred(t, canary, "If-None-Match", "*")
	if r.status != http.StatusInternalServerError {
		t.Fatalf(
			"PUT the credential with its row's set failing: answered %d %s, want 500",
			r.status,
			r.body,
		)
	}

	if s.sealedVersions(t) != before {
		t.Errorf(
			"sealed versions after the failed put: %d, want the %d there were",
			s.sealedVersions(t),
			before,
		)
	}

	seals := s.seals.seals()
	if len(seals) != 1 {
		t.Fatalf("the step sealed %d versions, want exactly 1", len(seals))
	}

	if sec, err := s.store.Secret(t.Context(), seals[0].ID, 0); err == nil {
		t.Errorf("the sealed version %+v: in the store, want not found", sec)
	}
}

// apartSetting writes one setting row apart from the armed write, inside
// it: after the armed write's function first ran, in a write of its own, so
// the armed write's transaction conflicts with what it read and its
// function runs again.
type apartSetting struct {
	kvstore.ControlPlane

	armed bool
	row   entity.Setting
	runs  *int
}

func (a *apartSetting) Write(
	ctx context.Context,
	meta entity.WriteMeta,
	fn func(entity.ControlPlaneTx) error,
) error {
	if !a.armed {
		return a.ControlPlane.Write(ctx, meta, fn)
	}

	a.armed = false

	return a.ControlPlane.Write(ctx, meta, func(tx entity.ControlPlaneTx) error {
		*a.runs++

		if err := fn(tx); err != nil {
			return err
		}

		if *a.runs != 1 {
			return nil
		}

		old, _, err := tx.Setting(a.row.Scope, a.row.Key)
		if err != nil {
			return err
		}

		_, err = a.PutSetting(ctx, a.row, old.Generation)

		return err
	})
}

// A write of a secret that conflicts with another write and runs its
// function again seals only once: the step ran before the write, and the
// version it sealed commits with the retry's row.
func TestSecretSealedOnceAcrossRetries(t *testing.T) {
	apart := &apartSetting{runs: new(int)}

	s := dirServe(t, entity.RoleAdmin, func(d *api.Deps) {
		apart.ControlPlane = d.Store.(kvstore.ControlPlane)
		d.Store = apart
	})

	// The store the served tests read through stays the bare one, so the
	// seed and the reads of this test go through the wrapper as the server
	// does, over the same Badger.
	host := entity.Setting{Scope: installScope, Key: dirHost, Value: []byte(`"host-a"`)}
	if _, err := apart.PutSetting(t.Context(), host, 0); err != nil {
		t.Fatalf("seed the host row: %v", err)
	}

	apart.armed = true
	apart.row = entity.Setting{Scope: installScope, Key: dirHost, Value: []byte(`"host-b"`)}

	r := s.putCred(t, "retry-bind-canary", "If-None-Match", "*")
	if r.status != http.StatusOK {
		t.Fatalf(
			"PUT the credential against a host written apart: answered %d %s, want 200",
			r.status,
			r.body,
		)
	}

	if *apart.runs != 2 {
		t.Errorf("the write's function ran %d times, want exactly 2", *apart.runs)
	}

	seals := s.seals.seals()
	if len(seals) != 1 {
		t.Fatalf("the write sealed %d versions, want exactly 1", len(seals))
	}

	if sec, err := apart.Secret(t.Context(), seals[0].ID, 0); err != nil || sec.Version != 1 {
		t.Errorf("the sealed credential: version %d err %v, want 1", sec.Version, err)
	}
}

// A retried request under one idempotency key replays the recorded answer
// and seals the value only once: no second version, no second seal.
func TestSecretIdempotentRetry(t *testing.T) {
	s := dirServe(t, entity.RoleAdmin)
	s.seedHost(t)

	first := s.putCred(
		t,
		"idempotent-bind-canary",
		"If-None-Match",
		"*",
		"Idempotency-Key",
		"cred-set",
	)

	if first.status != http.StatusOK {
		t.Fatalf("PUT the credential: answered %d %s, want 200", first.status, first.body)
	}

	again := s.putCred(
		t,
		"idempotent-bind-canary",
		"If-None-Match",
		"*",
		"Idempotency-Key",
		"cred-set",
	)

	if again.status != http.StatusOK || !bytes.Equal(again.body, first.body) {
		t.Fatalf("PUT again under the same key: answered %d %s, want the recorded %d %s",
			again.status, again.body, first.status, first.body)
	}

	if seals := s.seals.seals(); len(seals) != 1 {
		t.Errorf("the replay sealed again: %d seals, want exactly 1", len(seals))
	}

	ref, version := refVersionAnswer(t, first.body)

	if sec, err := s.store.Secret(t.Context(), ref, 0); err != nil || sec.Version != version {
		t.Errorf("the sealed credential: version %d err %v, want %d", sec.Version, err, version)
	}
}

// A second write of the same key seals the next version of the same
// reference: version 1 stays resolvable under it, the row keeps its
// reference and its generation moves.
func TestSecondWriteSealsNextVersion(t *testing.T) {
	s := dirServe(t, entity.RoleAdmin)
	s.seedHost(t)

	first := s.putCred(t, "first-bind-canary", "If-None-Match", "*")

	if first.status != http.StatusOK {
		t.Fatalf("PUT the credential: answered %d %s, want 200", first.status, first.body)
	}

	ref, _ := refVersionAnswer(t, first.body)

	second := s.putCred(t, "second-bind-canary", "If-Match", first.header.Get("ETag"))

	if second.status != http.StatusOK {
		t.Fatalf("PUT the credential again: answered %d %s, want 200", second.status, second.body)
	}

	secondRef, secondVersion := refVersionAnswer(t, second.body)

	if secondRef != ref || secondVersion != 2 {
		t.Errorf(
			"the second write: reference %v version %d, want %v at 2",
			secondRef,
			secondVersion,
			ref,
		)
	}

	var firstDoc apiv1.Setting
	if err := json.Unmarshal(first.body, &firstDoc); err != nil {
		t.Fatalf("the first answer %s: %v", first.body, err)
	}

	var secondDoc apiv1.Setting

	if err := json.Unmarshal(second.body, &secondDoc); err != nil {
		t.Fatalf("the second answer %s: %v", second.body, err)
	}

	if secondDoc.Generation <= firstDoc.Generation {
		t.Errorf(
			"the row's generation %d after the first write's %d, want it moved",
			secondDoc.Generation,
			firstDoc.Generation,
		)
	}

	if _, ok := s.settingRow(t, installScope, dirCred); !ok {
		t.Error("no credential row after the second write")
	}

	held, err := s.store.Secret(t.Context(), ref, 1)
	if err != nil || held.Version != 1 {
		t.Errorf("version 1 under the reference: version %d err %v, want 1", held.Version, err)
	}
}

// A secret's setting answers, in PUT and GET alike, its reference and its
// current version, never its value; each write's audit change holds the
// value only as set, and no audit record names the reference in text.
func TestSecretReadsAsRefAndVersion(t *testing.T) {
	s := dirServe(t, entity.RoleAdmin)
	s.seedHost(t)

	first := s.putCred(t, "read-bind-canary-one", "If-None-Match", "*")

	if first.status != http.StatusOK {
		t.Fatalf("PUT the credential: answered %d %s, want 200", first.status, first.body)
	}

	ref, _ := refVersionAnswer(t, first.body)

	second := s.putCred(t, "read-bind-canary-two", "If-Match", first.header.Get("ETag"))

	if second.status != http.StatusOK {
		t.Fatalf("PUT the credential again: answered %d %s, want 200", second.status, second.body)
	}

	got := s.do(t, http.MethodGet, "/api/v1/settings/"+dirCred, "")

	if got.status != http.StatusOK {
		t.Fatalf("GET the credential: answered %d %s, want 200", got.status, got.body)
	}

	if readRef, readVersion := refVersionAnswer(t, got.body); readRef != ref || readVersion != 2 {
		t.Errorf(
			"GET the credential: reference %v version %d, want %v at 2",
			readRef,
			readVersion,
			ref,
		)
	}

	var page apiv1.Page[apiv1.AuditRecord]

	if err := json.Unmarshal(s.audit(t), &page); err != nil {
		t.Fatalf("the audit page: %v", err)
	}

	refString := entity.ShareID(ref).String()
	set := 0

	for _, record := range page.Items {
		if record.Target.Kind != "setting" {
			continue
		}

		if string(record.Change) == `{"spec.value":"set"}` {
			set++

			continue
		}

		if bytes.Contains(record.Change, []byte(refString)) {
			t.Errorf("an audit record holds the reference: %s", record.Change)
		}
	}

	if set != 2 {
		t.Errorf("audit changes holding the value as set: %d, want 2, one per write", set)
	}
}

// driftStore deletes one setting row inside the armed write's own
// transaction, before its handler runs, so the armed write seals a version
// of a reference its row, read in that same transaction, no longer names.
// The stale error aborts the transaction: nothing it deleted or wrote
// commits.
type driftStore struct {
	kvstore.ControlPlane

	armed bool
	scope entity.SettingScope
	key   string
}

func (a *driftStore) Write(
	ctx context.Context,
	meta entity.WriteMeta,
	fn func(entity.ControlPlaneTx) error,
) error {
	if !a.armed {
		return a.ControlPlane.Write(ctx, meta, fn)
	}

	a.armed = false

	return a.ControlPlane.Write(ctx, meta, func(tx entity.ControlPlaneTx) error {
		row, ok, err := tx.Setting(a.scope, a.key)
		if err != nil {
			return err
		}

		if !ok {
			return errors.New("driftStore: no row to delete")
		}

		if err := tx.DeleteSetting(a.scope, a.key, row.Generation); err != nil {
			return err
		}

		return fn(tx)
	})
}

// A write whose row changed between its step and its transaction is
// refused, leaving the store as it was: no version sealed for a row that no
// longer exists, the first version still the current one.
func TestSealDriftRefused(t *testing.T) {
	drift := &driftStore{scope: installScope, key: dirCred}

	s := dirServe(t, entity.RoleAdmin, func(d *api.Deps) {
		drift.ControlPlane = d.Store.(kvstore.ControlPlane)
		d.Store = drift
	})

	// The store the served tests read through stays the bare one, so the
	// seed and the reads of this test go through the wrapper as the server
	// does, over the same Badger.
	host := entity.Setting{Scope: installScope, Key: dirHost, Value: []byte(`"host-a"`)}
	if _, err := drift.PutSetting(t.Context(), host, 0); err != nil {
		t.Fatalf("seed the host row: %v", err)
	}

	first := s.putCred(t, "drift-bind-canary", "If-None-Match", "*")

	if first.status != http.StatusOK {
		t.Fatalf("PUT the credential: answered %d %s, want 200", first.status, first.body)
	}

	ref, _ := refVersionAnswer(t, first.body)

	drift.armed = true
	before := s.sealedVersions(t)

	second := s.putCred(t, "drift-bind-canary-two", "If-None-Match", "*")

	if p := second.problem(t); second.status != http.StatusConflict || p.Code != "stale" {
		t.Fatalf(
			"PUT the credential over its deleted row: answered %d %s, want 409 stale",
			second.status,
			second.body,
		)
	}

	if s.sealedVersions(t) != before {
		t.Errorf(
			"sealed versions after the refused put: %d, want the %d there were",
			s.sealedVersions(t),
			before,
		)
	}

	if sec, err := drift.Secret(t.Context(), ref, 0); err != nil || sec.Version != 1 {
		t.Errorf(
			"the credential after the refused put: version %d err %v, want 1",
			sec.Version,
			err,
		)
	}
}

// The secret routes of their own keep their refusals: the keytab's set over
// a keytab already set answers 409 and writes no second version, and a
// store's credential route keeps its own refusal.
func TestOwnSecretRoutesKeepTheir409(t *testing.T) {
	t.Run("the keytab over a set keytab", func(t *testing.T) {
		good := keytabOf(t, "cifs/nas.ad.example", etypeID.AES256_CTS_HMAC_SHA1_96)

		s := serve(t, as(entity.RoleAdmin, false), func(d *api.Deps) {
			d.Settings = settings.Table{identity.Section()}
			d.Sealer = provider(t, d.Store.(kvstore.ControlPlane))
		})

		first := s.do(t, http.MethodPut, keytabPath, putKeytab(good), "If-None-Match", "*")
		if first.status != http.StatusOK {
			t.Fatalf("PUT the keytab: answered %d %s, want 200", first.status, first.body)
		}

		r := s.do(t, http.MethodPut, keytabPath, putKeytab(good), "If-None-Match", "*")

		if p := r.problem(t); r.status != http.StatusConflict || p.Code != "exist" ||
			p.Field != identity.KeytabKey {
			t.Fatalf(
				"PUT the set keytab: answered %d %s, want 409 exist naming the key",
				r.status,
				r.body,
			)
		}

		row, ok := s.settingRow(t, installScope, identity.KeytabKey)
		if !ok {
			t.Fatal("no identity.keytab row, want the first set's")
		}

		var ref entity.SecretRef
		if err := json.Unmarshal([]byte(row), &ref); err != nil {
			t.Fatalf("the identity.keytab row %s: %v", row, err)
		}

		if sec, err := s.store.Secret(t.Context(), ref, 0); err != nil || sec.Version != 1 {
			t.Errorf(
				"the keytab after the refused set: version %d err %v, want 1",
				sec.Version,
				err,
			)
		}
	})

	t.Run("a store's credential route", func(t *testing.T) {
		prov := writeServe(t)
		st := prov.apiCredentialStore(
			t,
			`{"remote.endpoint":"https://s3.example","remote.bucket":"b1"}`,
		)

		r := prov.do(t, http.MethodPut, "/api/v1/stores/"+st+"/credential",
			`{"spec":{"value":"bmV3"}}`, "If-None-Match", "*")

		if p := r.problem(t); r.status != http.StatusConflict || string(p.Code) != "exist" ||
			p.Field != "remote.credential" {
			t.Fatalf(
				"PUT the set credential: answered %d %s, want 409 exist naming it",
				r.status,
				r.body,
			)
		}
	})
}
