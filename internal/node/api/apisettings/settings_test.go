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
	want := apisettings.Settings{SessionLifetime: 12 * time.Hour, TokenMaxLifetime: 365 * day}
	if got := apisettings.Defaults(); got != want {
		t.Errorf("Defaults() = %+v, want %+v", got, want)
	}
	if got, want := apisettings.AuditDefaults(), (apisettings.Audit{Retention: 400 * day}); got != want {
		t.Errorf("AuditDefaults() = %+v, want %+v", got, want)
	}
}

// The api section's Validate accepts the defaults and refuses a lifetime that
// is not positive, naming every key it refuses.
func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*apisettings.Settings)
		refuse []string // what the refusal names, none when accepted
	}{
		{"defaults", func(*apisettings.Settings) {}, nil},
		{"no session lifetime", func(s *apisettings.Settings) { s.SessionLifetime = 0 }, []string{"api.session_lifetime"}},
		{"negative token lifetime", func(s *apisettings.Settings) { s.TokenMaxLifetime = -time.Hour },
			[]string{"api.token_max_lifetime"}},
		{"two keys broken", func(s *apisettings.Settings) { s.SessionLifetime, s.TokenMaxLifetime = 0, 0 },
			[]string{"api.session_lifetime", "api.token_max_lifetime"}},
	} {
		s := apisettings.Defaults()
		tc.change(&s)
		checkRefusal(t, tc.name, s.Validate(), tc.refuse)
	}
}

// The audit section's Validate accepts the default and an audit kept 90
// days, and refuses one kept 89 days, naming the key and the floor in days.
func TestAuditValidate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retention time.Duration
		refuse    []string
	}{
		{"default", apisettings.AuditDefaults().Retention, nil},
		{"audit kept 90 days", 90 * day, nil},
		{"audit kept 89 days", 89 * day, []string{"audit.retention", "under 90 days"}},
	} {
		checkRefusal(t, tc.name, apisettings.Audit{Retention: tc.retention}.Validate(), tc.refuse)
	}
}

// checkRefusal fails unless err is nil for no refuse, or names each of refuse.
func checkRefusal(t *testing.T, name string, err error, refuse []string) {
	t.Helper()
	if len(refuse) == 0 {
		if err != nil {
			t.Errorf("%s: refused %v, want accepted", name, err)
		}
		return
	}
	for _, want := range refuse {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want a refusal naming %q", name, err, want)
		}
	}
}
