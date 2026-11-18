package settings

import (
	"maps"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// versions is the range of versions this binary reads and writes of each
// format nodes share, the one a node registers at every start (RFC 13 §5.5).
var versions = map[entity.Format]entity.VersionRange{
	// kvstore.MinFormat to kvstore.MaxFormat.
	entity.FormatStore: {Min: 1, Max: 1},
	// The journal's format file version, FormatVersion in
	// pkg/block/journal/internal/format/formatfile.go.
	entity.FormatJournal: {Min: 1, Max: 1},
	// The block format, which has one version so far (RFC 4 §3.5).
	entity.FormatBlock: {Min: 1, Max: 1},
	// The settings record schema (RFC 13 §2.3).
	entity.FormatSettings: {Min: 1, Max: 1},
}

// Versions returns the range of versions this binary reads and writes of
// each format nodes share, a fresh copy, so no caller can widen it.
func Versions() map[entity.Format]entity.VersionRange {
	return maps.Clone(versions)
}
