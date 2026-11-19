package api

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// protocols are the adapters a path may name.
//
// ponytail: the list is written here; take it from the settings table's
// adapters.<protocol> sections once the table names them.
var protocols = []string{"nfs", "smb"}

var installation = entity.SettingScope{Kind: entity.ScopeInstallation}

// adapterRef is the ID an adapter's audit records name it by, its
// protocol, refusing 404 a protocol no adapter serves.
func adapterRef(_ [16]byte, protocol string) ([]byte, error) {
	if !slices.Contains(protocols, protocol) {
		return nil, errNotFound
	}
	return []byte(protocol), nil
}

// adapterKey is the installation-scope key of an adapter's field.
func adapterKey(protocol, field string) string { return "adapters." + protocol + "." + field }

// adapterDoc is an adapter as its PUT carries it: its spec alone.
type adapterDoc struct {
	Spec apiv1.AdapterSpec `json:"spec"`
}

// enabledOf is the adapter's enabled as its setting holds it, false for none.
func enabledOf(s entity.Setting) bool {
	var on bool
	_ = json.Unmarshal(s.Value, &on) // a value that is no boolean reads as disabled
	return on
}

// adapters is what the write adapter knows of an adapter: its enabled
// setting, whose generation is its tag. Every protocol's adapter exists,
// disabled at tag 0 until its enabled setting is first written, so the PUT
// that first writes it names If-Match "0", the tag its GET answered. Its
// row's target names the adapter by its protocol.
//
// ponytail: an adapter is tagged by its enabled setting, and its audit
// record holds that setting alone, since a write's transaction reads a
// setting by its key only, so a change of only another field moves no ETag.
// Tag the adapter by every adapters.<protocol> setting once the transaction
// can read them as one range.
var adapters = writing[adapterDoc]{kind: entity.ResourceAdapter, raises: true,
	read: func(tx entity.ControlPlaneTx, _ [16]byte, protocol string) (adapterDoc, uint64, bool, error) {
		s, _, err := tx.Setting(installation, adapterKey(protocol, "enabled"))
		return adapterDoc{apiv1.AdapterSpec{Enabled: enabledOf(s), Settings: map[string]json.RawMessage{}}}, s.Generation, true, err
	}}

// adapterFields is every adapters.<protocol> setting at installation scope,
// by field.
func adapterFields(ctx context.Context, d Deps, protocol string) (map[string]entity.Setting, error) {
	all, err := scopeSettings(ctx, d.Store, installation)
	out := map[string]entity.Setting{}
	for k, s := range all {
		if f, ok := strings.CutPrefix(k, adapterKey(protocol, "")); ok {
			out[f] = s
		}
	}
	return out, err
}

// adapterFieldsStep is putAdapter's pre-write step: the fields the adapter
// has, so the PUT can remove those it no longer names.
func adapterFieldsStep(ctx context.Context, rq request[adapterDoc]) ([]string, error) {
	if rq.d.Store == nil {
		return nil, errNoStore
	}
	f, err := adapterFields(ctx, rq.d, rq.Sub)
	return slices.Sorted(maps.Keys(f)), err
}

// putAdapter is PUT /adapters/{protocol}: its enabled and each of its
// settings written at installation scope, those the pre-write step listed
// and the spec no longer names removed, each judged by the settings table. A
// node override of a field, such as one host's port, is a node-scoped
// setting written through the setting resource. Its answer's status lists
// no node, which no node can have reached before the answer.
//
// ponytail: a field written between the listing and the commit outlives a
// PUT that does not name it; remove it in the transaction once it can list
// the adapter's settings.
func putAdapter(ctx context.Context, rq request[adapterDoc], had []string, tx entity.ControlPlaneTx) (apiv1.Adapter, [16]byte, error) {
	spec := rq.Body.Spec
	if _, ok := spec.Settings["enabled"]; ok {
		return apiv1.Adapter{}, rq.ID, &fieldError{field: "spec.settings.enabled", detail: "set by spec.enabled"}
	}
	on, _ := json.Marshal(spec.Enabled) // a boolean always encodes
	gen, err := putSetting(ctx, rq.d, tx, entity.Setting{Scope: installation, Key: adapterKey(rq.Sub, "enabled"), Value: on}, rq.Expect)
	if err != nil {
		return apiv1.Adapter{}, rq.ID, err
	}
	values := map[string]json.RawMessage{}
	for f, v := range spec.Settings {
		values[adapterKey(rq.Sub, f)] = v
	}
	if err := putSettings(ctx, rq.d, tx, installation, values); err != nil {
		return apiv1.Adapter{}, rq.ID, err
	}
	var gone []string
	for _, f := range had {
		if _, ok := spec.Settings[f]; !ok && f != "enabled" {
			gone = append(gone, adapterKey(rq.Sub, f))
		}
	}
	return apiv1.Adapter{ID: rq.Sub, Generation: gen, Spec: spec, Status: apiv1.AdapterStatus{Nodes: []apiv1.AdapterOnNode{}}}, rq.ID,
		deleteSettings(ctx, rq.d, tx, installation, gone)
}

// adapterRead is GET /adapters/{protocol}: whether it is enabled, its
// settings at installation scope, and, per node that reports it, its state
// and the settings generation it runs; its enabled setting's generation is
// the ETag, 0 before it is first written. A protocol no adapter serves is
// 404, refused by its row's target; ?wait= returns once the generation changes.
var adapterRead = watching[apiv1.Adapter]{
	read: func(ctx context.Context, d Deps, rq request[none]) (apiv1.Adapter, error) {
		if d.Nodes == nil {
			return apiv1.Adapter{}, errNoStore
		}
		fields, err := adapterFields(ctx, d, rq.Sub)
		if err != nil {
			return apiv1.Adapter{}, err
		}
		v := apiv1.Adapter{ID: rq.Sub, Spec: apiv1.AdapterSpec{Settings: map[string]json.RawMessage{}}, Status: apiv1.AdapterStatus{Nodes: []apiv1.AdapterOnNode{}}}
		for f, s := range fields {
			if f == "enabled" {
				v.Generation, v.Spec.Enabled = s.Generation, enabledOf(s)
				continue
			}
			v.Spec.Settings[f] = s.Value
		}
		statuses, err := d.Nodes.RegisteredStatuses(ctx)
		if err != nil {
			return apiv1.Adapter{}, err
		}
		for _, st := range statuses {
			for _, a := range st.Adapters {
				if a.Protocol == rq.Sub {
					v.Status.Nodes = append(v.Status.Nodes, apiv1.AdapterOnNode{Node: idString(st.Node), State: apiv1.AdapterState(a.State.String()), Generation: a.Generation})
				}
			}
		}
		return v, nil
	},
	tag: func(a apiv1.Adapter) uint64 { return a.Generation },
	until: changes(func(first, now apiv1.Adapter) bool {
		return first.Generation != now.Generation
	}),
}
