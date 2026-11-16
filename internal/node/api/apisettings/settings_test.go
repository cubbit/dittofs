package apisettings_test

import (
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node/api/apisettings"
)

const day = 24 * time.Hour

// The defaults are RFC 23's: a 12 h session, a 365-day API token and a
// 400-day audit.
func TestDefaults(t *testing.T) {
	want := apisettings.Settings{SessionLifetime: 12 * time.Hour, TokenMaxLifetime: 365 * day, AuditRetention: 400 * day}
	if got := apisettings.Defaults(); got != want {
		t.Errorf("Defaults() = %+v, want %+v", got, want)
	}
}

// Validate accepts the defaults and an audit kept 90 days, and refuses one
// kept 89 days and a lifetime that is not positive, naming the key.
func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*apisettings.Settings)
		refuse string // the key named, empty when accepted
	}{
		{"defaults", func(*apisettings.Settings) {}, ""},
		{"audit kept 90 days", func(s *apisettings.Settings) { s.AuditRetention = 90 * day }, ""},
		{"audit kept 89 days", func(s *apisettings.Settings) { s.AuditRetention = 89 * day }, "audit.retention"},
		{"no session lifetime", func(s *apisettings.Settings) { s.SessionLifetime = 0 }, "api.session_lifetime"},
		{"negative token lifetime", func(s *apisettings.Settings) { s.TokenMaxLifetime = -time.Hour }, "api.token_max_lifetime"},
	} {
		s := apisettings.Defaults()
		tc.change(&s)
		err := s.Validate()
		switch {
		case tc.refuse == "" && err != nil:
			t.Errorf("%s: refused %v, want accepted", tc.name, err)
		case tc.refuse != "" && (err == nil || !strings.Contains(err.Error(), tc.refuse)):
			t.Errorf("%s: got %v, want a refusal naming %s", tc.name, err, tc.refuse)
		}
	}
}
