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
