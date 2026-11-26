// Package entity imports a store and a library, which its importers then reach.
package entity

import (
	"example.com/lib"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// Hash is what the carver uses.
var Hash = kvstore.Open

var _ = lib.Sum
