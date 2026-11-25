// Package entity imports a store, which its importers then reach.
package entity

import "github.com/cubbit/dittofs/pkg/metadata/kvstore"

// Hash is what the carver uses.
var Hash = kvstore.Open
