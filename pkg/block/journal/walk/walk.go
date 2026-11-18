// Package walk re-exports the journal's own segment scanner, so a tool
// outside the journal's tree can read a journal directory's bytes with the
// real decoder instead of a copy of the layout.
//
// This package exists only on the throwaway demo branch and merges nowhere:
// it is an alias list over internal/format and internal/core, with no logic
// of its own, so the internal types appear here unchanged.
package walk

import (
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
)

type (
	JournalID     = core.JournalID
	SegmentID     = core.SegmentID
	SegmentHeader = format.SegmentHeader
	Scan          = format.Scan
	ScannedRecord = format.ScannedRecord
	RecordHeader  = format.RecordHeader
)

const (
	SegmentHeaderSize = format.SegmentHeaderSize
	RecordHeaderSize  = format.RecordHeaderSize
	RangeBad          = format.RangeBad
)

var (
	DecodeSegmentHeader = format.DecodeSegmentHeader
	ScanSegment         = format.ScanSegment
)
