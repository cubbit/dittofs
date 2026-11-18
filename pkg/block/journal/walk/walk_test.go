package walk_test

import (
	"bytes"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
	"github.com/cubbit/dittofs/pkg/block/journal/walk"
)

// The seam scans a segment planted through the independent byte-level
// builder: one verified write, then a seal marker.
func TestScanSegmentReadsAPlantedWriteAndItsSeal(t *testing.T) {
	id := [16]byte{1, 2, 3}
	seg := raw.SegmentHeader{Identity: id, ID: 7}.Bytes()
	rec := raw.Record{
		Identity: id, Kind: 1, Segment: 7, FileID: [16]byte{0xAA},
		Length: 5, Seq: 1, Version: raw.Version{Counter: 3}, Tag: 1,
		RecordOffset: uint32(len(seg)),
		Write:        &raw.WriteBody{MTime: 5, Payload: []byte("hello")},
	}.Bytes()
	seg = append(seg, rec...)
	seg = append(seg, raw.SealMarker{Identity: id, Segment: 7, Offset: uint64(len(seg))}.Bytes()...)
	seg = append(seg, make([]byte, 4096-len(seg))...)

	h, err := walk.DecodeSegmentHeader(seg[:walk.SegmentHeaderSize], id)
	if err != nil {
		t.Fatalf("DecodeSegmentHeader: %v", err)
	}
	sc, err := walk.ScanSegment(bytes.NewReader(seg), int64(len(seg)), id, h.ID)
	if err != nil {
		t.Fatalf("ScanSegment: %v", err)
	}
	if len(sc.Records) != 1 || sc.Records[0].Header.Extent.Len != 5 || sc.Records[0].Header.Version.Counter != 3 {
		t.Errorf("records = %+v, want the one planted write of 5 bytes at v3", sc.Records)
	}
	if !sc.Sealed {
		t.Errorf("Sealed = false, want the planted seal marker to end the scan")
	}
}
