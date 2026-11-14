package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// mountRemovalRoutes registers the removals, reservations and fill routes.
func (s *Server) mountRemovalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /truncate", s.truncate)
	mux.HandleFunc("POST /deallocate", s.deallocate)
	mux.HandleFunc("POST /delete", s.delete)
	mux.HandleFunc("POST /clone-target", s.cloneTarget)
	mux.HandleFunc("POST /reserve", s.reserve)
	mux.HandleFunc("POST /reserve-write", s.reserveWrite)
	mux.HandleFunc("POST /reserve-close", s.reserveClose)
	mux.HandleFunc("POST /fill", s.fill)
}

func (s *Server) truncate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag  uint32 `json:"tag"`
		File string `json:"file"`
		Size int64  `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("truncate: decoding request: %w", err))
		return
	}
	h, id, err := s.handleAndFile(journal.ShareTag(req.Tag), req.File)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if err := validateSize(req.Size); err != nil {
		respond(w, nil, err)
		return
	}
	v, err := h.Truncate(r.Context(), id, req.Size)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"version": versionToJSON(v)}, nil)
}

// validateSize refuses a negative truncate size before the journal meets it,
// so a typo answers as a typo.
func validateSize(n int64) error {
	if n < 0 {
		return fmt.Errorf("truncate: size %d is negative", n)
	}
	return nil
}

// validateExtent refuses the zero-length and negative extents before the
// journal meets them.
func validateExtent(e journal.Extent) error {
	if e.Len <= 0 {
		return fmt.Errorf("deallocate: length %d", e.Len)
	}
	if e.Off < 0 {
		return fmt.Errorf("deallocate: offset %d is negative", e.Off)
	}
	return nil
}

func (s *Server) deallocate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag  uint32 `json:"tag"`
		File string `json:"file"`
		Off  int64  `json:"off"`
		Len  int64  `json:"len"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("deallocate: decoding request: %w", err))
		return
	}
	h, id, err := s.handleAndFile(journal.ShareTag(req.Tag), req.File)
	if err != nil {
		respond(w, nil, err)
		return
	}
	e := journal.Extent{Off: req.Off, Len: req.Len}
	if err := validateExtent(e); err != nil {
		respond(w, nil, err)
		return
	}
	v, err := h.Deallocate(r.Context(), id, e)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"version": versionToJSON(v)}, nil)
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag  uint32 `json:"tag"`
		File string `json:"file"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("delete: decoding request: %w", err))
		return
	}
	h, id, err := s.handleAndFile(journal.ShareTag(req.Tag), req.File)
	if err != nil {
		respond(w, nil, err)
		return
	}
	v, err := h.Delete(r.Context(), id)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"version": versionToJSON(v)}, nil)
}

func (s *Server) cloneTarget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag      uint32      `json:"tag"`
		File     string      `json:"file"`
		SrcShare string      `json:"srcShare"`
		Src      string      `json:"src"`
		SrcOff   int64       `json:"srcOff"`
		DstOff   int64       `json:"dstOff"`
		Len      int64       `json:"len"`
		AsOf     versionJSON `json:"asOf"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("clone-target: decoding request: %w", err))
		return
	}
	h, id, err := s.handleAndFile(journal.ShareTag(req.Tag), req.File)
	if err != nil {
		respond(w, nil, err)
		return
	}
	srcShare, err := parseShareID(req.SrcShare)
	if err != nil {
		respond(w, nil, err)
		return
	}
	src, err := parseFileID(req.Src)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if req.Len <= 0 || req.DstOff < 0 || req.SrcOff < 0 {
		respond(w, nil, fmt.Errorf("clone-target: len %d, dst-off %d, src-off %d must be len > 0 and offsets ≥ 0",
			req.Len, req.DstOff, req.SrcOff))
		return
	}
	v, err := h.CloneTarget(r.Context(), id, journal.CloneSpec{
		SrcShare: srcShare,
		Src:      src,
		SrcOff:   req.SrcOff,
		DstOff:   req.DstOff,
		Len:      req.Len,
		AsOf:     versionFromJSON(req.AsOf),
	})
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"version": versionToJSON(v)}, nil)
}

func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag    uint32 `json:"tag"`
		Bytes  int64  `json:"bytes"`
		Writes int    `json:"writes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("reserve: decoding request: %w", err))
		return
	}
	h, err := s.handle(journal.ShareTag(req.Tag))
	if err != nil {
		respond(w, nil, err)
		return
	}
	if req.Writes <= 0 || req.Bytes < 0 {
		respond(w, nil, fmt.Errorf("reserve: bytes %d, writes %d: writes must be positive, bytes not negative",
			req.Bytes, req.Writes))
		return
	}
	var res *journal.Reservation
	err = awaitSpare(r.Context(), func() error {
		var rerr error
		res, rerr = h.Reserve(req.Bytes, req.Writes)
		return rerr
	})
	if err != nil {
		respond(w, nil, err)
		return
	}
	s.mu.Lock()
	s.nextRes++
	rid := s.nextRes
	s.reservations[rid] = res
	s.mu.Unlock()
	respond(w, map[string]any{"rid": rid}, nil)
}

func (s *Server) reservation(rid uint32) (*journal.Reservation, error) {
	s.mu.Lock()
	res := s.reservations[rid]
	s.mu.Unlock()
	if res == nil {
		return nil, fmt.Errorf("reservation %d is not open", rid)
	}
	return res, nil
}

func (s *Server) reserveWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag   uint32  `json:"tag"`
		Rid   uint32  `json:"rid"`
		File  string  `json:"file"`
		Off   int64   `json:"off"`
		Mtime int64   `json:"mtime"`
		Str   *string `json:"str"`
		Hex   *string `json:"hex"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("reserve-write: decoding request: %w", err))
		return
	}
	res, err := s.reservation(req.Rid)
	if err != nil {
		respond(w, nil, err)
		return
	}
	id, err := parseFileID(req.File)
	if err != nil {
		respond(w, nil, err)
		return
	}
	data, err := writePayload(req.Str, req.Hex, nil)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if len(data) == 0 {
		respond(w, nil, fmt.Errorf("reserve-write: the payload is empty"))
		return
	}
	var v journal.Version
	err = awaitSpare(r.Context(), func() error {
		var werr error
		v, werr = res.WriteAt(r.Context(), id, req.Off, data, req.Mtime)
		return werr
	})
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"version": versionToJSON(v)}, nil)
}

func (s *Server) reserveClose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rid uint32 `json:"rid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("reserve-close: decoding request: %w", err))
		return
	}
	s.mu.Lock()
	res := s.reservations[req.Rid]
	delete(s.reservations, req.Rid)
	s.mu.Unlock()
	if res == nil {
		respond(w, nil, fmt.Errorf("reservation %d is not open", req.Rid))
		return
	}
	if err := res.Close(); err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"closed": req.Rid}, nil)
}

func (s *Server) fill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag  uint32      `json:"tag"`
		File string      `json:"file"`
		Off  int64       `json:"off"`
		AsOf versionJSON `json:"asOf"`
		Ver  versionJSON `json:"ver"`
		Str  *string     `json:"str"`
		Hex  *string     `json:"hex"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("fill: decoding request: %w", err))
		return
	}
	h, id, err := s.handleAndFile(journal.ShareTag(req.Tag), req.File)
	if err != nil {
		respond(w, nil, err)
		return
	}
	data, err := writePayload(req.Str, req.Hex, nil)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if err := awaitSpare(r.Context(), func() error {
		return h.Fill(r.Context(), id, req.Off, data,
			versionFromJSON(req.AsOf), versionFromJSON(req.Ver))
	}); err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"filled": len(data)}, nil)
}

// handleAndFile is handle plus the file the operation names, both the CLI
// already validated once and the daemon validates again at trust's edge.
func (s *Server) handleAndFile(tag journal.ShareTag, file string) (*journal.Handle, journal.FileID, error) {
	h, err := s.handle(tag)
	if err != nil {
		return nil, journal.FileID{}, err
	}
	id, err := parseFileID(file)
	if err != nil {
		return nil, journal.FileID{}, err
	}
	return h, id, nil
}

// parseLen parses a positive decimal length.
func parseLen(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("length %q, want a positive byte count", s)
	}
	return n, nil
}
