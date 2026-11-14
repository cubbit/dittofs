package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// mountOffloadRoutes registers the offload family and repack.
func (s *Server) mountOffloadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /offload", s.offload)
	mux.HandleFunc("POST /mark-offloaded", s.markOffloaded)
	mux.HandleFunc("POST /unmark", s.unmark)
	mux.HandleFunc("POST /release", s.release)
	mux.HandleFunc("POST /repack", s.repack)
}

func (s *Server) offload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag     uint32      `json:"tag"`
		File    string      `json:"file"`
		Through versionJSON `json:"through"`
		Limit   int64       `json:"limit"`
		Widen   int64       `json:"widen"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("offload: decoding request: %w", err))
		return
	}
	h, id, err := s.handleAndFile(journal.ShareTag(req.Tag), req.File)
	if err != nil {
		respond(w, nil, err)
		return
	}
	var written int
	err = h.Offload(r.Context(), id, versionFromJSON(req.Through), req.Limit, req.Widen,
		func(ctx context.Context, o journal.Offer, report func([]journal.Extent) error) error {
			exts, ferr := writeSink(s.OffloadDir, o)
			if ferr != nil {
				return ferr
			}
			written = len(exts)
			return report(exts)
		})
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"offloaded": written, "dir": s.OffloadDir}, nil)
}

func (s *Server) markOffloaded(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag     uint32       `json:"tag"`
		File    string       `json:"file"`
		Extents []extentJSON `json:"extents"`
		Oldest  versionJSON  `json:"oldest"`
		Newest  versionJSON  `json:"newest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("mark-offloaded: decoding request: %w", err))
		return
	}
	h, id, err := s.handleAndFile(journal.ShareTag(req.Tag), req.File)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if len(req.Extents) == 0 {
		respond(w, nil, fmt.Errorf("mark-offloaded: no extents named"))
		return
	}
	exts := make([]journal.Extent, 0, len(req.Extents))
	for _, e := range req.Extents {
		exts = append(exts, journal.Extent{Off: e.Off, Len: e.Len})
	}
	if err := h.MarkOffloaded(r.Context(), id, exts,
		versionFromJSON(req.Oldest), versionFromJSON(req.Newest)); err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"marked": len(exts)}, nil)
}

func (s *Server) unmark(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag     uint32       `json:"tag"`
		File    string       `json:"file"`
		Extents []extentJSON `json:"extents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("unmark: decoding request: %w", err))
		return
	}
	h, id, err := s.handleAndFile(journal.ShareTag(req.Tag), req.File)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if len(req.Extents) == 0 {
		respond(w, nil, fmt.Errorf("unmark: no extents named"))
		return
	}
	exts := make([]journal.Extent, 0, len(req.Extents))
	for _, e := range req.Extents {
		exts = append(exts, journal.Extent{Off: e.Off, Len: e.Len})
	}
	if err := h.Unmark(r.Context(), id, exts); err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"unmarked": len(exts)}, nil)
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag     uint32       `json:"tag"`
		File    string       `json:"file"`
		Extents []extentJSON `json:"extents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("release: decoding request: %w", err))
		return
	}
	h, id, err := s.handleAndFile(journal.ShareTag(req.Tag), req.File)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if len(req.Extents) == 0 {
		respond(w, nil, fmt.Errorf("release: no extents named"))
		return
	}
	exts := make([]journal.Extent, 0, len(req.Extents))
	for _, e := range req.Extents {
		exts = append(exts, journal.Extent{Off: e.Off, Len: e.Len})
	}
	if err := h.Release(r.Context(), id, exts); err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"released": len(exts)}, nil)
}

func (s *Server) repack(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Budget   int64    `json:"budget"`
		Tag      uint32   `json:"tag"`
		Segments []uint64 `json:"segments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("repack: decoding request: %w", err))
		return
	}
	if req.Budget <= 0 {
		respond(w, nil, fmt.Errorf("repack: budget %d, want a positive byte count", req.Budget))
		return
	}
	j, err := s.journal()
	if err != nil {
		respond(w, nil, err)
		return
	}
	scope := journal.RepackScope{Share: journal.ShareTag(req.Tag)}
	for _, seg := range req.Segments {
		scope.Segments = append(scope.Segments, journal.SegmentID(seg))
	}
	freed, err := j.Repack(r.Context(), req.Budget, scope)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"freed": freed}, nil)
}

// parseExtents parses the CLI's OFF:LEN,OFF:LEN form into extents, naming
// the offending pair in every refusal. An empty extent and a zero length are
// refused, as the journal refuses them.
func parseExtents(s string) ([]journal.Extent, error) {
	if s == "" {
		return nil, fmt.Errorf("extents: none named (--extents OFF:LEN,...)")
	}
	parts := strings.Split(s, ",")
	out := make([]journal.Extent, 0, len(parts))
	for _, p := range parts {
		offStr, lenStr, ok := strings.Cut(p, ":")
		if !ok {
			return nil, fmt.Errorf("extent %q: want OFF:LEN", p)
		}
		off, err1 := parseInt64(offStr)
		ln, err2 := parseInt64(lenStr)
		if err1 != nil || err2 != nil || off < 0 || ln <= 0 {
			return nil, fmt.Errorf("extent %q: want a non-negative offset and a positive length", p)
		}
		out = append(out, journal.Extent{Off: off, Len: ln})
	}
	return out, nil
}
