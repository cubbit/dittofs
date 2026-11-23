package api_test

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/settings"
)

// The identity keys the node's table gives to admin alone stay refused to an
// operator with the table handed in to the settings routes, and an admin's
// put of identity.ntlm beside them is accepted. identity.keytab is taken by
// its own route, whose refusals its own tests cover.
func TestKeyRolesOnTheSettingsRoutes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		role       entity.APIRole
		ntlmStatus int
	}{
		{"an operator", entity.RoleOperator, http.StatusForbidden},
		{"an admin", entity.RoleAdmin, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := entity.SettingScope{Kind: entity.ScopeInstallation}
			s := serve(t, as(tc.role, false), func(d *api.Deps) {
				d.Settings = settings.Table{identity.Section()}
			})
			gen := s.putSetting(t, scope, "identity.ntlm", "true")
			tag := strconv.Quote(strconv.FormatUint(gen, 10))

			r := s.do(t, http.MethodPut, "/api/v1/settings/identity.ntlm",
				`{"spec":{"value":true}}`, "If-Match", tag, "Idempotency-Key", "ntlm-"+tc.name)
			if r.status != tc.ntlmStatus {
				t.Errorf(
					"a put of identity.ntlm: answered %d %s, want %d",
					r.status,
					r.body,
					tc.ntlmStatus,
				)
			}

			r = s.do(t, http.MethodPut, "/api/v1/settings/identity.keytab",
				`{"spec":{"value":true}}`, "If-None-Match", "*")
			if r.status != http.StatusBadRequest {
				t.Errorf(
					"a put of identity.keytab on the settings route: answered %d %s, want 400",
					r.status,
					r.body,
				)
			}
		})
	}
}
