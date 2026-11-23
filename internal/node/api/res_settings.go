package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/settings"
)

// settingsTable is the settings table the node hands in: the components'
// validators, by key prefix, which judge a setting's write inside its
// transaction.
type settingsTable interface {
	Check(tx entity.ControlPlaneTx, caller entity.APIRole, w settings.Write, holds settings.HoldsContent) error
}

var _ settingsTable = settings.Table{}

// componentError is a write a component refused, naming the field it
// refused and, in a fixed text, what refused it; the component's error is
// what the problem table answers.
type componentError struct {
	field, detail string
	err           error
}

func (e *componentError) Error() string { return e.field + ": " + e.err.Error() }
func (e *componentError) Unwrap() error { return e.err }

// refusedKey is key refused by the settings table with err.
func refusedKey(key string, err error) error {
	return &componentError{key, "refused by the settings table", err}
}

// checkSetting judges the write w builds for the caller ctx carries through
// the table d holds, inside tx, a refusal naming key. w runs only once a
// table is handed in.
//
// decision: with no table handed in, every key is refused ErrUnknownKey, as
// the empty table refuses a prefix with no validator, so a setting-backed
// write fails closed until the node hands the table in. A secret's set is
// refused 503 before it gets here, in secretSetting.seal, so no seal is spent
// on it. Overturned once the node always hands one, when nil should refuse
// 503 unavailable instead.
func checkSetting(ctx context.Context, d Deps, tx entity.ControlPlaneTx, key string, w func() settings.Write) error {
	if d.Settings == nil {
		return refusedKey(key, settings.ErrUnknownKey)
	}
	c, _ := auth.CallerFrom(ctx)
	if err := d.Settings.Check(tx, c.Role, w(), holdsContent); err != nil {
		return refusedKey(key, err)
	}
	return nil
}

// holdsContent answers that every scope holds content.
//
// decision: a bound setting is refused at every scope as though content
// were there, since nothing records what a scope holds: a namespace no share
// names still holds blocks until GC drains it, and a store holds whatever its
// namespaces left. So a bound setting fails closed, never open. Overturned
// once the store records a scope's content, read in the write's transaction
// as RFC 13's HoldsContent describes.
func holdsContent(entity.SettingScope) (bool, error) { return true, nil }

// putSettings writes each of values at scope, in key order, each judged by
// the settings table first, under the generation tx reads it at.
func putSettings(ctx context.Context, d Deps, tx entity.ControlPlaneTx, scope entity.SettingScope, values map[string]json.RawMessage) error {
	for _, k := range slices.Sorted(maps.Keys(values)) {
		old, _, err := tx.Setting(scope, k)
		if err != nil {
			return err
		}
		if _, err := putSetting(ctx, d, tx, entity.Setting{Scope: scope, Key: k, Value: values[k]}, old.Generation); err != nil {
			return err
		}
	}
	return nil
}

// putSetting writes s under expect, judged by the settings table first, and
// returns its new generation.
func putSetting(ctx context.Context, d Deps, tx entity.ControlPlaneTx, s entity.Setting, expect uint64) (uint64, error) {
	if err := checkSetting(ctx, d, tx, s.Key, func() settings.Write { return settings.Put(s) }); err != nil {
		return 0, err
	}
	return tx.PutSetting(s, expect)
}

// deleteSetting removes key's value at scope under expect, judged by the
// settings table first.
func deleteSetting(ctx context.Context, d Deps, tx entity.ControlPlaneTx, scope entity.SettingScope, key string, expect uint64) error {
	if err := checkSetting(ctx, d, tx, key, func() settings.Write { return settings.Delete(scope, key) }); err != nil {
		return err
	}
	return tx.DeleteSetting(scope, key, expect)
}

// deleteSettings removes each of keys at scope, each judged by the settings
// table first, under the generation tx reads it at; a key already gone is
// left.
func deleteSettings(ctx context.Context, d Deps, tx entity.ControlPlaneTx, scope entity.SettingScope, keys []string) error {
	for _, k := range keys {
		old, ok, err := tx.Setting(scope, k)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := deleteSetting(ctx, d, tx, scope, k, old.Generation); err != nil {
			return err
		}
	}
	return nil
}

// scopeSettings is every setting at scope, by key, read outside any write.
func scopeSettings(ctx context.Context, st reads, scope entity.SettingScope) (map[string]entity.Setting, error) {
	out := map[string]entity.Setting{}
	for s, err := range st.Settings(ctx, scope) {
		if err != nil {
			return nil, err
		}
		out[s.Key] = s
	}
	return out, nil
}

// settingKeys is the keys of every setting at the scope rq's path names,
// read before the write as its pre-write step, for a write that removes
// them all: a write's transaction reads a setting by its key only.
func settingKeys(scope func([16]byte) entity.SettingScope) func(context.Context, request[none]) ([]string, error) {
	return func(ctx context.Context, rq request[none]) ([]string, error) {
		if rq.d.Store == nil {
			return nil, errNoStore
		}
		all, err := scopeSettings(ctx, rq.d.Store, scope(rq.ID))
		return slices.Sorted(maps.Keys(all)), err
	}
}

// settingDoc is a setting as its PUT carries it: its spec alone.
type settingDoc struct {
	Spec apiv1.SettingSpec `json:"spec"`
}

// settingScopes are the scopes a setting is written at, each with the path
// of its collection, the right its writes ask, the scope its path's ID
// names, and, for a scope of a record, whether that record exists, read in
// the write's transaction, so no setting is written for a node, namespace
// or store that has none. A share's is the store's own check of the share.
var settingScopes = []struct {
	path   string
	right  auth.Right
	scope  func([16]byte) entity.SettingScope
	exists func(tx entity.ControlPlaneTx, id [16]byte) (bool, error)
}{
	{"/api/v1/settings", auth.Stores, scopeOf(entity.ScopeInstallation), nil},
	{"/api/v1/nodes/{id}/settings", auth.Stores, scopeOf(entity.ScopeNode), func(tx entity.ControlPlaneTx, id [16]byte) (bool, error) {
		_, ok, err := tx.Node(entity.NodeID(id))
		return ok, err
	}},
	{"/api/v1/namespaces/{id}/settings", auth.Stores, scopeOf(entity.ScopeNamespace), func(tx entity.ControlPlaneTx, id [16]byte) (bool, error) {
		_, ok, err := tx.Namespace(entity.NamespaceID(id))
		return ok, err
	}},
	{"/api/v1/stores/{id}/settings", auth.Stores, scopeOf(entity.ScopeRemote), func(tx entity.ControlPlaneTx, id [16]byte) (bool, error) {
		_, ok, err := tx.Setting(storeScope(id), storeEndpoint)
		return ok, err
	}},
	{"/api/v1/shares/{id}/settings", auth.Shares, scopeOf(entity.ScopeShare), nil},
}

func scopeOf(k entity.ScopeKind) func([16]byte) entity.SettingScope {
	return func(id [16]byte) entity.SettingScope { return entity.SettingScope{Kind: k, ID: id} }
}

// ownKeys are the keys only a resource's own route writes, each with the
// route that does: a store is created with its endpoint, and a store's
// credential's and the keytab's references are each written with the secret
// they name, after their route's checks.
var ownKeys = map[string]string{
	storeEndpoint:   "POST /api/v1/stores",
	storeCredential: "PUT /api/v1/stores/{id}/credential",
	keytabKey:       "PUT /api/v1/installation/keytab",
}

// maxSettingKey is the longest setting key a route takes.
const maxSettingKey = 255

// settingRoutes are the generic setting resource's rows at every scope: a
// setting read, put and deleted by its key, {sub}, below its scope's path.
// A put states the value; a delete removes the record, so the wider
// scope's value or the default is effective. Every write is judged by the
// settings table, and refused 404 for a record of a scope that does not
// exist, and 400 for a key only a resource's own route writes. A write of
// the installation's identity.ntlm also runs the rule for turning NTLM back
// on, and answers 202 with the NT-hash deletion it starts; one of its
// identity.netbios_name is refused 409 joined while the installation is
// joined. A
// share-scoped setting of a share being removed is refused stale by the
// store.
func settingRoutes() []route {
	var out []route
	for _, sc := range settingScopes {
		// An installation setting's put may start the NT-hash deletion,
		// which identity.ntlm's rule starts; a delete turns NTLM on, which
		// starts none.
		var may apiv1.OpKind
		var desc string
		if sc.path == "/api/v1/settings" {
			may = apiv1.OpNTHashDelete
			desc = "A write of " + auth.NTLMKey + " takes the admin role, whatever x-dittofs-role names for every other key."
		}
		// The key is the audit target, which an index key holds, so it is
		// bounded before any record is written for it.
		target := func(id [16]byte, key string) ([]byte, error) {
			if key == "" || len(key) > maxSettingKey || !utf8.ValidString(key) {
				return nil, &fieldError{field: "key", detail: "a setting's key is valid UTF-8 of 1 to " + strconv.Itoa(maxSettingKey) + " bytes"}
			}
			return entity.SettingRef(sc.scope(id), key).ID, nil
		}
		w := writing[settingDoc]{kind: entity.ResourceSetting, raises: true,
			read: func(tx entity.ControlPlaneTx, id [16]byte, key string) (settingDoc, uint64, bool, error) {
				if via, ok := ownKeys[key]; ok {
					return settingDoc{}, 0, false, &fieldError{field: key, detail: "written by " + via}
				}
				if sc.exists != nil {
					if ok, err := sc.exists(tx, id); err != nil || !ok {
						if err == nil {
							err = errNotFound
						}
						return settingDoc{}, 0, false, err
					}
				}
				s, ok, err := tx.Setting(sc.scope(id), key)
				return settingDoc{apiv1.SettingSpec{Value: s.Value}}, s.Generation, ok, err
			}}
		path := sc.path + "/{sub}"
		out = append(out,
			route{method: http.MethodGet, pattern: path, kind: entity.ResourceSetting, right: auth.View, target: target, sub: subSetting, serve: watch(settingRead(sc.scope))},
			route{method: http.MethodPut, pattern: path, kind: entity.ResourceSetting, right: sc.right, mayStart: may, target: target, sub: subSetting, desc: desc, serve: write(w, nil,
				func(ctx context.Context, rq request[settingDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.Setting, [16]byte, error) {
					v := rq.Body.Spec.Value
					if len(v) == 0 || string(v) == "null" {
						return apiv1.Setting{}, rq.ID, &fieldError{field: "spec.value", detail: "a put states the value; a DELETE resets the setting to its default"}
					}
					var gen uint64
					st := entity.Setting{Scope: sc.scope(rq.ID), Key: rq.Sub, Value: v}
					if err := netbiosRule(tx, st.Scope, st.Key); err != nil {
						return apiv1.Setting{}, rq.ID, err
					}
					err := ntlmWrite(ctx, tx, rq.start, st.Scope, st.Key, func() (err error) {
						gen, err = putSetting(ctx, rq.d, tx, st, rq.Expect)
						return err
					})
					return apiv1.Setting{ID: rq.Sub, Generation: gen, Spec: apiv1.SettingSpec{Value: v}}, rq.ID, err
				})},
			route{method: http.MethodDelete, pattern: path, kind: entity.ResourceSetting, right: sc.right, target: target, sub: subSetting, desc: desc, serve: write(tagsOf(w), nil,
				func(ctx context.Context, rq request[none], _ struct{}, tx entity.ControlPlaneTx) (none, [16]byte, error) {
					scope := sc.scope(rq.ID)
					if err := netbiosRule(tx, scope, rq.Sub); err != nil {
						return none{}, rq.ID, err
					}
					return none{}, rq.ID, ntlmWrite(ctx, tx, rq.start, scope, rq.Sub, func() error {
						return deleteSetting(ctx, rq.d, tx, scope, rq.Sub, rq.Expect)
					})
				})},
		)
	}
	return out
}

// settingRead is GET of a setting at the scope its path names, its
// generation the ETag, with on how many nodes it is applied: those that
// have reconciled the settings generation through its last change. A key
// with no record at the scope is 404; ?wait= returns once its generation
// changes.
func settingRead(scope func([16]byte) entity.SettingScope) watching[apiv1.Setting] {
	return watching[apiv1.Setting]{
		read: func(ctx context.Context, d Deps, rq request[none]) (apiv1.Setting, error) {
			if d.Nodes == nil {
				return apiv1.Setting{}, errNoStore
			}
			all, err := scopeSettings(ctx, d.Store, scope(rq.ID))
			if err != nil {
				return apiv1.Setting{}, err
			}
			s, ok := all[rq.Sub]
			if !ok {
				return apiv1.Setting{}, errNotFound
			}
			pp := perPage{}
			if pp.statuses, err = d.Nodes.RegisteredStatuses(ctx); err != nil {
				return apiv1.Setting{}, err
			}
			a := pp.applied(entity.SettingRef(s.Scope, s.Key), s.Position, func(st entity.NodeStatus) uint64 { return st.Settings })
			return apiv1.Setting{ID: s.Key, Generation: s.Generation, Spec: apiv1.SettingSpec{Value: s.Value}, Status: apiv1.SettingStatus{Applied: &a}}, nil
		},
		tag: func(s apiv1.Setting) uint64 { return s.Generation },
		until: changes(func(first, now apiv1.Setting) bool {
			return first.Generation != now.Generation
		}),
	}
}
