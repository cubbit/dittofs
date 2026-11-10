package datapath

import (
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// recordBytes, emptyRoomOf and sealOver are the stream's figures the
// capacity tests size their limits by.
func recordBytes(n int64) int64 { return segment.RecordBytes(n) }

func emptyRoomOf(cap int64) int64 { return segment.EmptyRoom(cap) }

func sealOver(n int64) int64 {
	return format.SealMarkerSize + n*format.CatalogEntrySize + format.TrailerSize
}
