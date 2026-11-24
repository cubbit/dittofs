package node

import (
	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/node/api/apisettings"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/settings"
)

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
}

// usedAt is the reach of a section its component reads where it uses it.
func usedAt() reach {
	return reach{atUse: true}
}

// newReaches is the node's map from each declared section to how its
// component reaches a change of it: the node holds one such map. The api
// and audit sections are read where their component uses them: the session
// lifetime and the token maximum by the sign-in and a token's creation, the
// retention by the sweep's turn, each loading the published snapshot of the
// moment, so a change reaches its reader at its next operation and no apply
// runs for them.
//
// decision: api is read at use though the node serves no sign-in and issues
// no token yet, so no reader exists: its routes, which load the published
// snapshot as they land, are its readers, and mapping it nowhere would
// refuse every api.* change as applied by nothing while it publishes.
// Overturned once the api section gains a component the node applies.
func newReaches() map[string]reach {
	return map[string]reach{
		"api":   usedAt(),
		"audit": usedAt(),
	}
}
