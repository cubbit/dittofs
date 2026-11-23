package node

import (
	"fmt"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/node/api/apisettings"
	asettings "github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
)

// The adapters' sections' prefixes in the settings table, and the node's
// connection limit's own key.
const (
	adaptersNFS        = "adapters.nfs"
	adaptersSMB        = "adapters.smb"
	connectionLimitKey = "connection_limit"
)

// nfsSection is the NFS adapter's part of the settings table: the protocol's
// whole shape beside the one key the node owns, enabled, which no adapter's
// own shape carries (RFC 13 Appendix B, RFC 20 section 3.6). Enabled is off
// by default, and live with the shape's settings; Port and Enabled are the
// keys a node's record may replace, the rest holding one value at the
// installation's.
type nfsSection struct {
	asettings.NFS
	Enabled bool `json:"enabled" class:"live"`
}

// Validate is the protocol's own check: enabled is a bool, never a value out
// of shape (RFC 13 section 6).
func (s nfsSection) Validate() error {
	return s.NFS.Validate()
}

// smbSection is the SMB adapter's part of the settings table, as the NFS
// adapter's: its protocol's whole shape beside the node's enabled key.
type smbSection struct {
	asettings.SMB
	Enabled bool `json:"enabled" class:"live"`
}

// Validate is smbSection's own shape check that the NFS one's is.
func (s smbSection) Validate() error {
	return s.SMB.Validate()
}

// settingSchema opens the store to write settings rows up to the newest
// schema the settings table reads, so no node writes a row it would refuse.
func settingSchema() kvstore.Option {
	return kvstore.SettingSchema(settings.Versions()[entity.FormatSettings].Max)
}

// wantAdapters reads each adapter's section from the published snapshot into
// its slot's want. A section the node refused keeps there what was last
// published, so its slot is asked for that again; one refused since the node started has no value,
// and leaves its adapter disabled, never run on defaults a row refuses. The
// settings walk reports the refused rows (keepSettings).
func (n *Node) wantAdapters(snap *settings.Snapshot) {
	node := n.cfg.Bootstrap.Node.ID

	if nfs, err := settings.Get[nfsSection](snap, adaptersNFS, node); err == nil {
		n.nfs.want.Store(&desired[asettings.NFS]{
			enabled:  nfs.Enabled,
			settings: nfs.NFS,
		})
	}

	if smb, err := settings.Get[smbSection](snap, adaptersSMB, node); err == nil {
		n.smb.want.Store(&desired[asettings.SMB]{
			enabled:  smb.Enabled,
			settings: smb.SMB,
		})
	}
}

// connLimitSection is the node's connection_limit row of the settings table:
// one field whose key is the section's own name, as SelfKey declares it,
// the most connections a node's adapters hold at once, a quarter of it per
// address (RFC 13 Appendix B, RFC 24 section 4.4). Validate checks the
// shape alone, an integer at least 1: the bound derived from this host is
// the node's own check, which a shape check cannot see (RFC 13 section 6).
type connLimitSection struct {
	Limit int `json:"limit" class:"live"`
}

// Validate refuses only a value out of shape, printing it and never the key,
// which the refusal's own line already names (RFC 13 section 6).
func (s connLimitSection) Validate() error {
	if s.Limit < 1 {
		return fmt.Errorf("a count of connections must be at least 1, got %d", s.Limit)
	}

	return nil
}

// applyConnLimit sets the node's connection count to the connection_limit of
// the published snapshot. A value above the bound derived for this host,
// which Validate cannot see, is the node's own refusal: until the start has
// reached the adapters' step the apply fails, refusing the start, naming the
// key and the bound; once the node serves, the row is reported refused and
// the last limit set stands, waiting for the store to change (RFC 13
// section 6, RFC 24 section 4.4). A row the shape refuses is the settings
// walk's to report, once: the section keeps what was last published, which
// may be a value above the bound, so it is not judged again.
//
// decision: a row the shape refuses at start does not fail the start, as no
// refused section does: the section holds no value, so the derived limit the
// count was built with serves, and the status carries the refused row until
// the store is repaired. This is an accepted deviation from RFC 13 section
// 6's never a default, taken over failing the start on a corrupt stored row.
// Overturned if a node must fail its start on such a row.
//
// decision: the router publishes a value above the bound, as it publishes
// every section read at use, so the snapshot holds it while the count keeps
// the last limit. No other component reads the section. An apply in the node's map
// would keep it out, but its refusal is retried on the backoff and cannot
// fail the start.
func (n *Node) applyConnLimit(snap *settings.Snapshot, wk settingsRead) ([]refusal, error) {
	if _, ok := wk.refused[connectionLimitKey]; ok {
		return nil, nil
	}

	v, err := settings.Get[connLimitSection](snap, connectionLimitKey, n.cfg.Bootstrap.Node.ID)
	if err != nil {
		return nil, nil
	}

	if v.Limit > connectionLimit {
		tooHigh := fmt.Errorf("%d is above this node's derived bound %d", v.Limit, connectionLimit)
		if !n.adapting {
			return nil, fmt.Errorf("%s: %w", connectionLimitKey, tooHigh)
		}

		return wk.walk.holdRefusals(connectionLimitKey, wk.at.Settings, false, tooHigh, nil), nil
	}

	n.budgets.Conns.SetLimit(v.Limit)

	return nil, nil
}

// newTable is the node's one settings table: every section the node declares,
// which reconcile reads and the API's write check judges a write by. Each
// component's section joins it with the component.
func newTable() settings.Table {
	return settings.Table{
		settings.Of("api", settings.At(entity.ScopeInstallation), apisettings.Defaults(),
			settings.WriteRole(entity.RoleAdmin)),
		settings.Of("audit", settings.At(entity.ScopeInstallation), apisettings.AuditDefaults(),
			settings.WriteRole(entity.RoleAdmin)),
		identity.Section(),
		identity.DirectorySection(),
		settings.Of(adaptersNFS, settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			nfsSection{NFS: asettings.DefaultNFS()},
			settings.OverrideOnly("Port", "Enabled")),
		settings.Of(adaptersSMB, settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			smbSection{SMB: asettings.DefaultSMB()},
			settings.OverrideOnly("Port", "Enabled")),
		settings.Of(connectionLimitKey, settings.At(entity.ScopeNode),
			connLimitSection{Limit: connectionLimit}, settings.SelfKey()),
	}
}

// reach is how the component of one declared section reaches each change of
// it: an apply the node calls, or "read at use", the reader loading the
// published snapshot at its next operation, which is what RFC 13 §5's live
// binding makes of a change nothing has to run. A section with live or
// bound keys and no entry holding one of the two has no way to its
// component, so a change of it fails rather than reporting a component
// configured that no code reached (RFC 23 §2.4).
type reach struct {
	// apply is handed the snapshot the walk is about to publish, once a
	// pass with a live or bound change of the section, however many of its
	// keys changed. Its error refuses the section whole. It is nil when the
	// section is read at use.
	//
	// decision: the snapshot carries a restart-class key's new value while
	// the status still lists that key as pending, so an apply reads only its
	// section's live and bound keys: one that decodes its whole struct would
	// run a restart value live. No section declared yet mixes restart keys
	// with an apply. Hand an apply its section with the restart keys taken
	// from what the section first ran, once one does.
	apply func(*settings.Snapshot) error
	// atUse is set when the component reads the section where it uses it.
	atUse bool
	// rebuilds is set when the component runs a restart-class change itself,
	// by building itself anew, so none of the section waits for the node's
	// restart.
	rebuilds bool
}

// usedAt is the reach of a section its component reads where it uses it.
func usedAt() reach {
	return reach{atUse: true}
}

// rebuilt is the reach of an adapter's section: read at use, and rebuilt by
// its slot for a restart-class change.
func rebuilt() reach {
	return reach{atUse: true, rebuilds: true}
}

// newReaches is the node's map from each declared section to how its
// component reaches a change of it: the node holds one such map. The api
// and audit sections are read where their component uses them: the session
// lifetime and the token maximum by the sign-in and a token's creation, the
// retention by the sweep's turn, each loading the published snapshot of the
// moment, so a change reaches its reader at its next operation and no apply
// runs for them.
//
// connection_limit is read at use by each pass, which sets the node's
// connection count from it (applyConnLimit).
//
// The adapters' sections are read at use too: each pass reads them into the
// slots' wants before it calls them (wantAdapters). A slot rebuilds its
// adapter for a new port itself, so that restart-class key is never pending
// (RFC 23 §2.3, RFC 20 §3.6).
//
// decision: api is read at use though the node serves no sign-in and issues
// no token yet, so no reader exists: its routes, which load the published
// snapshot as they land, are its readers, and mapping it nowhere would
// refuse every api.* change as applied by nothing while it publishes.
// Overturned once the api section gains a component the node applies.
func newReaches() map[string]reach {
	return map[string]reach{
		"api":       usedAt(),
		"audit":     usedAt(),
		adaptersNFS: rebuilt(),
		adaptersSMB: rebuilt(),

		connectionLimitKey: usedAt(),
	}
}
