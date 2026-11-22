package apiv1

import (
	"fmt"
	"io"
	"log/slog"
	"time"
)

// The types below hold passwords and tokens. Each has a LogValue, which slog
// uses, and a Format, which fmt uses for every verb, and both leave the
// secret fields out, so neither a structured log nor a %v or %+v of the body
// itself prints a secret. encoding/json still writes every field, since it
// writes the wire.
//
// Neither method is called in two cases. slog's JSON handler marshals a
// struct that holds one of these bodies with encoding/json, so logging a
// Resource[APITokenSpec, APITokenStatus] writes status.token. And fmt prints
// an unexported field without calling its Format. So a type that holds one of
// these bodies, Resource included, needs its own LogValue before anyone logs it.

// redacted is what a body logs as when every field is a secret: slog drops a
// group with no attributes, and its key would vanish with it.
var redacted = slog.StringValue("[redacted]")

func show(f fmt.State, v slog.Value) { _, _ = io.WriteString(f, v.String()) }

// SignIn is the body of POST /auth/sign-in: an account's name and password.
type SignIn struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

func (s SignIn) LogValue() slog.Value {
	return slog.GroupValue(slog.String("name", s.Name))
}

func (s SignIn) Format(f fmt.State, _ rune) { show(f, s.LogValue()) }

// Tokens answers a sign-in, a refresh and a password change: an access token
// to present on each call, when it expires, the refresh token that gets the
// next pair, and whether the account must change its password before any
// call but that change and sign-out.
type Tokens struct {
	Access     string    `json:"access_token"`
	ExpiresAt  time.Time `json:"expires_at"`
	Refresh    string    `json:"refresh_token"`
	MustChange bool      `json:"must_change"`
}

func (Tokens) LogValue() slog.Value { return redacted }

func (t Tokens) Format(f fmt.State, _ rune) { show(f, t.LogValue()) }

// Refresh is the body of POST /auth/refresh: the session's current refresh
// token, which the answer replaces.
type Refresh struct {
	Token string `json:"refresh_token"`
}

func (Refresh) LogValue() slog.Value { return redacted }

func (r Refresh) Format(f fmt.State, _ rune) { show(f, r.LogValue()) }

// PasswordChange is the body of POST /auth/password: the caller's current
// password, required even when it must be changed, and the new one.
type PasswordChange struct {
	Current string `json:"current_password"`
	New     string `json:"new_password"`
}

func (PasswordChange) LogValue() slog.Value { return redacted }

func (p PasswordChange) Format(f fmt.State, _ rune) { show(f, p.LogValue()) }

// SetPassword is the body of a user's :set-password: the password to set.
type SetPassword struct {
	Password string `json:"password"`
}

func (SetPassword) LogValue() slog.Value { return redacted }

func (s SetPassword) Format(f fmt.State, _ rune) { show(f, s.LogValue()) }

// APITokenSpec is what a service account's API token is created with: its
// expiry, no later than api.token_max_lifetime from its creation.
type APITokenSpec struct {
	Expires time.Time `json:"expires"`
}

// APITokenStatus is what the server records of an API token. Token is the
// token itself, set only in the answer that created it.
type APITokenStatus struct {
	Created time.Time `json:"created"`
	Token   string    `json:"token,omitempty"`
}

func (s APITokenStatus) LogValue() slog.Value {
	return slog.GroupValue(slog.Time("created", s.Created))
}

func (s APITokenStatus) Format(f fmt.State, _ rune) { show(f, s.LogValue()) }
