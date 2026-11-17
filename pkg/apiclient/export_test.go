package apiclient

// WithoutRefresh sends a call with refresh off, as sign-in, refresh and a
// password change are sent: a 401 is returned as it is.
func WithoutRefresh() CallOption { return func(c *call) { c.noRefresh = true } }
