package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// spareRetryBound is how long a route waits out the one refusal the journal
// documents as clearing on its own: a NoSpaceError naming LimitSpare, while
// the spare pool refills asynchronously. Every other answer — success, a
// real limit's refusal, any error — passes through at once.
//
// decision: the pool's refill is asynchronous by contract ("the retry finds
// it served"), and a demo presents one command at a time, so the daemon waits
// the refill out rather than making the presenter retype. Withdraw the wait
// if a demo beat ever wants the refusal story shown.
const spareRetryBound = 10 * time.Second

// awaitSpare runs op, retrying while it answers a LimitSpare refusal and the
// bound has not passed.
func awaitSpare(ctx context.Context, op func() error) error {
	deadline := time.Now().Add(spareRetryBound)
	for {
		err := op()
		if err == nil || !isLimitSpare(err) {
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// isLimitSpare answers whether err is a NoSpaceError naming the spare limit.
func isLimitSpare(err error) bool {
	var ns *journal.NoSpaceError
	if errors.As(err, &ns) {
		return ns.Limit == journal.LimitSpare
	}
	return false
}

// respond writes the one envelope every route answers with; err's text goes
// to the terminal verbatim, whatever it is.
func respond(w http.ResponseWriter, result any, err error) {
	var env envelope
	if err != nil {
		env = envelope{OK: false, Error: err.Error()}
	} else {
		b, merr := json.Marshal(result)
		if merr != nil {
			env = envelope{OK: false, Error: merr.Error()}
		} else {
			env = envelope{OK: true, Result: b}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(env)
}

// extentJSON is a byte extent on the wire.
type extentJSON struct {
	Off int64 `json:"off"`
	Len int64 `json:"len"`
}

func extentToJSON(e journal.Extent) extentJSON {
	return extentJSON{Off: e.Off, Len: e.Len}
}

// heldJSON is a served extent with the content version it was held at.
type heldJSON struct {
	Off     int64       `json:"off"`
	Len     int64       `json:"len"`
	Version versionJSON `json:"version"`
}

// mountRoutes registers this task's op routes on mux; later tasks add theirs.
func (s *Server) mountRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /share", s.share)
	mux.HandleFunc("POST /forget", s.forget)
	mux.HandleFunc("POST /strand", s.strand)
	mux.HandleFunc("POST /draw", s.draw)
	mux.HandleFunc("POST /set-version-limit", s.setVersionLimit)
	mux.HandleFunc("POST /write-at", s.writeAt)
	mux.HandleFunc("POST /read-at", s.readAt)
	mux.HandleFunc("POST /sync", s.sync)
}

// journal returns the running journal; the daemon holds none between Start
// and Shutdown.
func (s *Server) journal() (*journal.Journal, error) {
	s.mu.Lock()
	j := s.j
	s.mu.Unlock()
	if j == nil {
		return nil, fmt.Errorf("daemon not started")
	}
	return j, nil
}

// handle returns the handle share attached, the share-tag addressing every
// data-path op carries.
func (s *Server) handle(tag journal.ShareTag) (*journal.Handle, error) {
	s.mu.Lock()
	h := s.handles[tag]
	s.mu.Unlock()
	if h == nil {
		return nil, fmt.Errorf("share %d is not attached", tag)
	}
	return h, nil
}

func (s *Server) share(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag   uint32      `json:"tag"`
		Limit int64       `json:"limit"`
		Floor versionJSON `json:"floor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("share: decoding request: %w", err))
		return
	}
	j, err := s.journal()
	if err != nil {
		respond(w, nil, err)
		return
	}
	h, err := j.Share(journal.ShareTag(req.Tag), req.Limit, versionFromJSON(req.Floor))
	if err != nil {
		respond(w, nil, err)
		return
	}
	s.mu.Lock()
	s.handles[journal.ShareTag(req.Tag)] = h
	s.mu.Unlock()
	respond(w, map[string]any{"tag": req.Tag}, nil)
}

func (s *Server) forget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag uint32 `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("forget: decoding request: %w", err))
		return
	}
	j, err := s.journal()
	if err != nil {
		respond(w, nil, err)
		return
	}
	if err := j.Forget(r.Context(), journal.ShareTag(req.Tag)); err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"forgot": req.Tag}, nil)
}

func (s *Server) strand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag uint32 `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("strand: decoding request: %w", err))
		return
	}
	j, err := s.journal()
	if err != nil {
		respond(w, nil, err)
		return
	}
	if err := j.Strand(journal.ShareTag(req.Tag)); err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"stranded": req.Tag}, nil)
}

func (s *Server) draw(w http.ResponseWriter, r *http.Request) {
	j, err := s.journal()
	if err != nil {
		respond(w, nil, err)
		return
	}
	v, err := j.Draw()
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"version": versionToJSON(v)}, nil)
}

func (s *Server) setVersionLimit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version versionJSON `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("set-version-limit: decoding request: %w", err))
		return
	}
	j, err := s.journal()
	if err != nil {
		respond(w, nil, err)
		return
	}
	if err := j.SetVersionLimit(versionFromJSON(req.Version)); err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"set": versionToJSON(versionFromJSON(req.Version))}, nil)
}

// writeAt appends one write's payload, generated here for --size so a
// multi-MiB write never crosses the socket whole.
func (s *Server) writeAt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag   uint32  `json:"tag"`
		File  string  `json:"file"`
		Off   int64   `json:"off"`
		Mtime int64   `json:"mtime"`
		Str   *string `json:"str"`
		Hex   *string `json:"hex"`
		Size  *int64  `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("write-at: decoding request: %w", err))
		return
	}
	h, err := s.handle(journal.ShareTag(req.Tag))
	if err != nil {
		respond(w, nil, err)
		return
	}
	id, err := parseFileID(req.File)
	if err != nil {
		respond(w, nil, err)
		return
	}
	data, err := writePayload(req.Str, req.Hex, req.Size)
	if err != nil {
		respond(w, nil, err)
		return
	}
	var v journal.Version
	err = awaitSpare(r.Context(), func() error {
		var werr error
		v, werr = h.WriteAt(r.Context(), id, req.Off, data, req.Mtime)
		return werr
	})
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"version": versionToJSON(v), "bytes": len(data)}, nil)
}

// writePayload builds exactly one of the three payload sources, refusing the
// absent and the doubled alike: a demo command's data must never be a
// surprise.
func writePayload(str, hexp *string, size *int64) ([]byte, error) {
	chosen := 0
	if str != nil {
		chosen++
	}
	if hexp != nil {
		chosen++
	}
	if size != nil {
		chosen++
	}
	if chosen != 1 {
		return nil, fmt.Errorf("write-at: exactly one of str, hex, size names the payload")
	}
	switch {
	case str != nil:
		return []byte(*str), nil
	case hexp != nil:
		b, err := hex.DecodeString(*hexp)
		if err != nil {
			return nil, fmt.Errorf("write-at: hex %q: %w", *hexp, err)
		}
		return b, nil
	default:
		if *size < 0 {
			return nil, fmt.Errorf("write-at: size %d is negative", *size)
		}
		// A cycling pattern so every read of a filled range is visibly
		// alive without carrying real content over the socket.
		pattern := []byte("0123456789abcdef")
		out := make([]byte, *size)
		for i := range out {
			out[i] = pattern[i%len(pattern)]
		}
		return out, nil
	}
}

func (s *Server) readAt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag  uint32 `json:"tag"`
		File string `json:"file"`
		Off  int64  `json:"off"`
		Len  int64  `json:"len"`
		Full bool   `json:"full"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("read-at: decoding request: %w", err))
		return
	}
	if req.Len <= 0 {
		respond(w, nil, fmt.Errorf("read-at: length %d", req.Len))
		return
	}
	h, err := s.handle(journal.ShareTag(req.Tag))
	if err != nil {
		respond(w, nil, err)
		return
	}
	id, err := parseFileID(req.File)
	if err != nil {
		respond(w, nil, err)
		return
	}
	p := make([]byte, req.Len)
	n, missing, held, asOf, err := h.ReadAt(r.Context(), id, req.Off, p)
	if err != nil {
		respond(w, nil, err)
		return
	}
	resp := struct {
		N       int64        `json:"n"`
		Data    string       `json:"data"`
		Missing []extentJSON `json:"missing"`
		Held    []heldJSON   `json:"held"`
		AsOf    versionJSON  `json:"asOf"`
	}{N: int64(n), Data: hex.EncodeToString(p[:n]), Missing: []extentJSON{}, Held: []heldJSON{}, AsOf: versionToJSON(asOf)}
	for _, m := range missing {
		resp.Missing = append(resp.Missing, extentToJSON(m))
	}
	for _, hv := range held {
		resp.Held = append(resp.Held, heldJSON{Off: hv.Extent.Off, Len: hv.Extent.Len, Version: versionToJSON(hv.Version)})
	}
	respond(w, resp, nil)
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag   uint32   `json:"tag"`
		Files []string `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("sync: decoding request: %w", err))
		return
	}
	h, err := s.handle(journal.ShareTag(req.Tag))
	if err != nil {
		respond(w, nil, err)
		return
	}
	ids := make([]journal.FileID, 0, len(req.Files))
	for _, f := range req.Files {
		id, err := parseFileID(f)
		if err != nil {
			respond(w, nil, err)
			return
		}
		ids = append(ids, id)
	}
	if err := h.Sync(r.Context(), ids...); err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"synced": len(ids)}, nil)
}

// parseOffset parses a decimal file offset, refusing the negative as a name
// the presenter typed rather than letting the journal meet it.
func parseOffset(s string) (int64, error) {
	off, err := strconv.ParseInt(s, 10, 64)
	if err != nil || off < 0 {
		return 0, fmt.Errorf("offset %q", s)
	}
	return off, nil
}
