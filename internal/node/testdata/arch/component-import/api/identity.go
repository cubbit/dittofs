package api

import "github.com/cubbit/dittofs/pkg/identity"

// The API may name identity's join credential and keytab check, and not its
// other calls.
var (
	cred  identity.JoinCredential
	names = identity.KeytabNames
	check = identity.CheckKeytab
)
