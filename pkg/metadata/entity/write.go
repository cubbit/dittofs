package entity

import "errors"

// ErrGenerationMismatch is a write whose expect is not the generation, or
// the version, of the record as stored: the caller read an older one, or
// expected none where one exists. Nothing was written.
var ErrGenerationMismatch = errors.New("entity: record changed since it was read")

// Alloc names the IDs a user or group write draws from the store's uid, gid
// and RID counters in its own transaction. An ID it does not name is written
// as the record holds it.
type Alloc struct{ UID, GID, RID bool }
