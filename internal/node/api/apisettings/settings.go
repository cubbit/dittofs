// Package apisettings is the management API's own settings, two sections: the
// api section, how long a sign-in's session and a service account's API token
// may last, and the audit section, how long the audit is kept.
package apisettings

import (
	"errors"
	"fmt"
	"time"
)

const day = 24 * time.Hour

// minAuditRetention is the shortest audit.retention a write may set, so no
// write can make the sweep delete its own record or the quarter's before it.
// minAuditRetentionText is how a refusal names it.
const (
	minAuditRetention     = 90 * day
	minAuditRetentionText = "90 days"
)

// Settings is the api section, each field under api.<json name>.
type Settings struct {
	// SessionLifetime is how long a session lasts after its sign-in.
	SessionLifetime time.Duration `json:"session_lifetime" class:"live"`
	// TokenMaxLifetime is the latest an API token may expire after its
	// creation.
	TokenMaxLifetime time.Duration `json:"token_max_lifetime" class:"live"`
}

// Audit is the audit section, each field under audit.<json name>.
type Audit struct {
	// Retention is how long an audit record is kept.
	Retention time.Duration `json:"retention" class:"live"`
}

// Defaults returns the api section's defaults: a working day's session and a
// year's API token.
func Defaults() Settings {
	return Settings{SessionLifetime: 12 * time.Hour, TokenMaxLifetime: 365 * day}
}

// AuditDefaults returns the audit section's default: a year and a quarter.
func AuditDefaults() Audit {
	return Audit{Retention: 400 * day}
}

// Validate refuses a lifetime that is not positive, naming the key; it never
// clamps or defaults.
func (s Settings) Validate() error {
	var errs []error
	for _, f := range []struct {
		key string
		v   time.Duration
	}{{"api.session_lifetime", s.SessionLifetime}, {"api.token_max_lifetime", s.TokenMaxLifetime}} {
		if f.v <= 0 {
			errs = append(errs, fmt.Errorf("settings: %s: %s is not positive", f.key, f.v))
		}
	}
	return errors.Join(errs...)
}

// Validate refuses an audit.retention under 90 days, naming the key and the
// floor; it never clamps or defaults.
func (a Audit) Validate() error {
	if a.Retention < minAuditRetention {
		return fmt.Errorf("settings: audit.retention: %s is under %s", a.Retention, minAuditRetentionText)
	}
	return nil
}
