// Package apisettings is the management API's own settings section: how long
// a sign-in's session and a service account's API token may last, and how
// long the audit is kept.
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

// Settings is the section, each field under its installation-wide key.
type Settings struct {
	SessionLifetime  time.Duration // api.session_lifetime: a session expires this long after its sign-in
	TokenMaxLifetime time.Duration // api.token_max_lifetime: the latest an API token may expire after its creation
	AuditRetention   time.Duration // audit.retention: how long an audit record is kept
}

// Defaults returns the section's defaults: a working day's session, a year's
// API token and a year and a quarter's audit.
func Defaults() Settings {
	return Settings{SessionLifetime: 12 * time.Hour, TokenMaxLifetime: 365 * day, AuditRetention: 400 * day}
}

// Validate refuses a lifetime that is not positive and an audit.retention
// under 90 days, naming the key and the rule; it never clamps or defaults.
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
	if s.AuditRetention < minAuditRetention {
		errs = append(errs, fmt.Errorf("settings: audit.retention: %s is under %s", s.AuditRetention, minAuditRetentionText))
	}
	return errors.Join(errs...)
}
