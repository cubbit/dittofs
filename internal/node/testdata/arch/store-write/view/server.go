package api

import "github.com/cubbit/dittofs/pkg/metadata/kvstore"

// Deps holds the store's view itself, every write of it reachable.
type Deps struct{ Store kvstore.ControlPlane }
