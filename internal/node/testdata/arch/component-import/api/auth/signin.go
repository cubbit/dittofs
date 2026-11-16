package auth

// Identity and the secrets package, which only the auth package may import.
import (
	_ "github.com/cubbit/dittofs/pkg/identity"
	_ "github.com/cubbit/dittofs/pkg/secrets"
)
