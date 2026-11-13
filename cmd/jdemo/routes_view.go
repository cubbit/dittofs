package main

import (
	"net/http"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// indexEntryJSON is one index entry on the wire.
type indexEntryJSON struct {
	Kind      string      `json:"kind"`
	Off       int64       `json:"off"`
	Len       int64       `json:"len"`
	Version   versionJSON `json:"version"`
	Tag       uint32      `json:"tag"`
	Segment   uint64      `json:"segment"`
	SegOff    int64       `json:"segOff"`
	Offloaded bool        `json:"offloaded"`
}

func indexEntryToJSON(e journal.IndexEntry) indexEntryJSON {
	return indexEntryJSON{
		Kind:      entryKindName(e.Kind),
		Off:       e.Extent.Off,
		Len:       e.Extent.Len,
		Version:   versionToJSON(e.Version),
		Tag:       uint32(e.Tag),
		Segment:   uint64(e.Location.Segment),
		SegOff:    e.Location.Offset,
		Offloaded: e.Offloaded,
	}
}

// segmentInfoJSON is one segment's accounting on the wire.
type segmentInfoJSON struct {
	ID        uint64 `json:"id"`
	Stream    uint32 `json:"stream"`
	Sealed    bool   `json:"sealed"`
	Failed    bool   `json:"failed"`
	Damaged   bool   `json:"damaged"`
	Allocated int64  `json:"allocated"`
	HeldBytes int64  `json:"held"`
	Retained  int64  `json:"retained"`
}

// mountViewRoutes registers the read-only viewers.
func (s *Server) mountViewRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /index", s.viewIndex)
	mux.HandleFunc("GET /segments", s.viewSegments)
	mux.HandleFunc("GET /stats", s.viewStats)
	mux.HandleFunc("GET /verify", s.viewVerify)
}

func (s *Server) viewIndex(w http.ResponseWriter, r *http.Request) {
	j, err := s.journal()
	if err != nil {
		respond(w, nil, err)
		return
	}
	in := j.Inspect()
	if f := r.URL.Query().Get("file"); f != "" {
		id, err := parseFileID(f)
		if err != nil {
			respond(w, nil, err)
			return
		}
		es, err := in.File(id)
		if err != nil {
			respond(w, nil, err)
			return
		}
		out := make([]indexEntryJSON, 0, len(es))
		for _, e := range es {
			out = append(out, indexEntryToJSON(e))
		}
		respond(w, map[string]any{"entries": out}, nil)
		return
	}
	out := []indexEntryJSON{}
	for _, es := range in.All() {
		for _, e := range es {
			out = append(out, indexEntryToJSON(e))
		}
	}
	respond(w, map[string]any{"entries": out}, nil)
}

func (s *Server) viewSegments(w http.ResponseWriter, r *http.Request) {
	j, err := s.journal()
	if err != nil {
		respond(w, nil, err)
		return
	}
	segs := j.Inspect().Segments()
	out := make([]segmentInfoJSON, 0, len(segs))
	for _, seg := range segs {
		out = append(out, segmentInfoJSON{
			ID:        uint64(seg.ID),
			Stream:    seg.Stream,
			Sealed:    seg.Sealed,
			Failed:    seg.Failed,
			Damaged:   seg.Damaged,
			Allocated: seg.Allocated,
			HeldBytes: seg.HeldBytes,
			Retained:  seg.RetainedBytes,
		})
	}
	respond(w, map[string]any{"segments": out}, nil)
}

func (s *Server) viewStats(w http.ResponseWriter, r *http.Request) {
	j, err := s.journal()
	if err != nil {
		respond(w, nil, err)
		return
	}
	// Stats is plain exported data, so it crosses whole: the pane renders
	// every figure the journal reports, not a chosen few.
	respond(w, map[string]any{"stats": j.Stats()}, nil)
}

func (s *Server) viewVerify(w http.ResponseWriter, r *http.Request) {
	j, err := s.journal()
	if err != nil {
		respond(w, nil, err)
		return
	}
	rep, err := j.Inspect().Verify(r.Context())
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"report": verifyToJSON(rep)}, nil)
}

// verifyJSON carries a verify report's findings.
type verifyJSON struct {
	Unsupported int   `json:"unsupported"`
	Unindexed   int   `json:"unindexed"`
	BadFooters  int   `json:"badFooters"`
	Mismatch    int64 `json:"mismatch"`
}

func verifyToJSON(rep journal.VerifyReport) verifyJSON {
	n := 0
	for _, es := range rep.Unsupported {
		n += len(es)
	}
	return verifyJSON{
		Unsupported: n,
		Unindexed:   len(rep.Unindexed),
		BadFooters:  len(rep.BadFooters),
		Mismatch:    rep.FootprintMismatch,
	}
}
