package apiv1

import "time"

// SignIn is the body of POST /auth/sign-in: an account's name and password.
type SignIn struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

// Tokens answers a sign-in, a refresh and a password change: an access token
// to present on each call, and the refresh token that gets the next pair.
type Tokens struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
}

// Refresh is the body of POST /auth/refresh: the session's current refresh
// token, which the answer replaces.
type Refresh struct {
	Token string `json:"refresh_token"`
}

// PasswordChange is the body of POST /auth/password: the caller's current
// password, required even when it must be changed, and the new one.
type PasswordChange struct {
	Current string `json:"current_password"`
	New     string `json:"new_password"`
}

// SetPassword is the body of a user's :set-password: the password to set.
type SetPassword struct {
	Password string `json:"password"`
}

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

// SessionStatus is what the server records of a sign-in's session: when it
// was created and when its refresh token stops working.
type SessionStatus struct {
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}
