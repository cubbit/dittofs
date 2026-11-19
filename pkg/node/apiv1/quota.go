package apiv1

// Quota is one share's quota on one user or group, or on the share itself,
// as a resource: its ID the principal's, or the share's for its own, and its
// generation the quota's version.
type Quota = Resource[QuotaSpec, struct{}]

// QuotaSpec is a quota's limits: refused beyond Hard; allowed beyond Soft
// for GraceSeconds, then refused; never refused beyond Advisory, which only
// raises an event. A quota whose limits are all zero limits nothing and is
// deleted.
type QuotaSpec struct {
	Hard         Limit `json:"hard"`
	Soft         Limit `json:"soft"`
	GraceSeconds int64 `json:"grace_seconds"`
	Advisory     Limit `json:"advisory"`
}

// Limit is a byte and a file count, zero in either limiting nothing on it.
type Limit struct {
	Bytes int64 `json:"bytes"`
	Files int64 `json:"files"`
}
