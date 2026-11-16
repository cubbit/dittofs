package auth_test

import (
	"testing"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Every right, for every role, is RFC 23's role table: a role holds a right
// where its column is ticked, and reads what every other right guards except
// accounts, the audit and the quota events, which only their holders read.
// Each want is one letter per role, none, viewer, operator and admin in
// order: h holds the right, r only reads what it guards, - neither.
func TestRoleTable(t *testing.T) {
	roles := []entity.APIRole{entity.RoleNone, entity.RoleViewer, entity.RoleOperator, entity.RoleAdmin}
	for _, tc := range []struct {
		name  string
		right auth.Right
		want  string
	}{
		{"public", auth.Public, "hhhh"},
		{"view", auth.View, "-hhh"},
		{"shares", auth.Shares, "-rhh"},
		{"stores", auth.Stores, "-rhh"},
		{"principals", auth.Principals, "-rhh"},
		{"accounts", auth.Accounts, "---h"},
		{"drain", auth.Drain, "-rhh"},
		{"installation", auth.Installation, "-rrh"},
		{"audit", auth.Audit, "---h"},
		{"quota events", auth.QuotaEvents, "--hh"},
		{"self", auth.Self, "-hhh"},
	} {
		for i, role := range roles {
			read, hold := auth.Allowed(auth.Caller{Role: role}, tc.right)
			got := byte('-')
			switch {
			case hold && read:
				got = 'h'
			case read:
				got = 'r'
			case hold:
				got = '?'
			}
			if got != tc.want[i] {
				t.Errorf("%s for %v: got %c, want %c", tc.name, role, got, tc.want[i])
			}
		}
	}
}
