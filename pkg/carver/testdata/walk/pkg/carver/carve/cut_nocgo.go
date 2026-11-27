//go:build !cgo

package carve

import "github.com/cubbit/dittofs/pkg/metadata/kvstore"

var _ = kvstore.Open
