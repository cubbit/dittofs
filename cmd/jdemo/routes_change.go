package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// sinceBound is the most changes one /since answers with; a demo file's
// history is short, and a pane's scroll is shorter.
const sinceBound = 1000

// mountChangeRoutes registers the hold family and the change/query ops.
func (s *Server) mountChangeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /hold", s.hold)
	mux.HandleFunc("POST /unhold", s.unhold)
	mux.HandleFunc("POST /held", s.held)
	mux.HandleFunc("POST /stamp", s.stamp)
	mux.HandleFunc("POST /since", s.since)
	mux.HandleFunc("POST /settle", s.settle)
	mux.HandleFunc("POST /extents", s.extents)
	mux.HandleFunc("POST /files", s.files)
	mux.HandleFunc("POST /dirty-files", s.dirtyFiles)
}

func (s *Server) hold(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag   uint32            `json:"tag"`
		Share string            `json:"share"`
		Cut   uint64            `json:"cut"`
		Marks map[string]string `json:"marks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("hold: decoding request: %w", err))
		return
	}
	h, err := s.handle(journal.ShareTag(req.Tag))
	if err != nil {
		respond(w, nil, err)
		return
	}
	share, err := parseShareID(req.Share)
	if err != nil {
		respond(w, nil, err)
		return
	}
	marks := make(map[journal.FileID]journal.Version)
	for f, ver := range req.Marks {
		id, err := parseFileID(f)
		if err != nil {
			respond(w, nil, err)
			return
		}
		v, err := parseVersion(ver)
		if err != nil {
			respond(w, nil, err)
			return
		}
		marks[id] = v
	}
	if err := h.Hold(r.Context(), share, journal.SnapshotCut(req.Cut), marks); err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"held cut": req.Cut}, nil)
}

func (s *Server) unhold(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag   uint32 `json:"tag"`
		Share string `json:"share"`
		Cut   uint64 `json:"cut"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("unhold: decoding request: %w", err))
		return
	}
	h, err := s.handle(journal.ShareTag(req.Tag))
	if err != nil {
		respond(w, nil, err)
		return
	}
	share, err := parseShareID(req.Share)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if err := h.Unhold(r.Context(), share, journal.SnapshotCut(req.Cut)); err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"released cut": req.Cut}, nil)
}

func (s *Server) held(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag   uint32 `json:"tag"`
		Share string `json:"share"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("held: decoding request: %w", err))
		return
	}
	h, err := s.handle(journal.ShareTag(req.Tag))
	if err != nil {
		respond(w, nil, err)
		return
	}
	share, err := parseShareID(req.Share)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"held": h.Held(share)}, nil)
}

func (s *Server) stamp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag     uint32      `json:"tag"`
		File    string      `json:"file"`
		Through versionJSON `json:"through"`
		Cut     uint64      `json:"cut"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("stamp: decoding request: %w", err))
		return
	}
	h, id, err := s.handleAndFile(journal.ShareTag(req.Tag), req.File)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if err := h.Stamp(r.Context(), id, versionFromJSON(req.Through), journal.SnapshotCut(req.Cut)); err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"stamped": req.Through}, nil)
}

func (s *Server) since(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag  uint32      `json:"tag"`
		File string      `json:"file"`
		From versionJSON `json:"from"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("since: decoding request: %w", err))
		return
	}
	h, id, err := s.handleAndFile(journal.ShareTag(req.Tag), req.File)
	if err != nil {
		respond(w, nil, err)
		return
	}
	type changeJSON struct {
		Kind    string      `json:"kind"`
		Off     int64       `json:"off"`
		Len     int64       `json:"len"`
		Version versionJSON `json:"version"`
		Mtime   int64       `json:"mtime"`
	}
	out := []changeJSON{}
	for ch, serr := range h.Since(r.Context(), id, versionFromJSON(req.From)) {
		if serr != nil {
			respond(w, nil, serr)
			return
		}
		out = append(out, changeJSON{
			Kind: kindName(ch.Kind), Off: ch.Extent.Off, Len: ch.Extent.Len,
			Version: versionToJSON(ch.Version), Mtime: ch.Mtime,
		})
		if len(out) >= sinceBound {
			break
		}
	}
	respond(w, map[string]any{"changes": out}, nil)
}

func (s *Server) settle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag     uint32      `json:"tag"`
		File    string      `json:"file"`
		Version versionJSON `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("settle: decoding request: %w", err))
		return
	}
	h, id, err := s.handleAndFile(journal.ShareTag(req.Tag), req.File)
	if err != nil {
		respond(w, nil, err)
		return
	}
	h.Settle(id, versionFromJSON(req.Version))
	respond(w, map[string]any{"settled": req.Version}, nil)
}

func (s *Server) extents(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag  uint32 `json:"tag"`
		File string `json:"file"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("extents: decoding request: %w", err))
		return
	}
	h, id, err := s.handleAndFile(journal.ShareTag(req.Tag), req.File)
	if err != nil {
		respond(w, nil, err)
		return
	}
	exts, err := h.Extents(id)
	if err != nil {
		respond(w, nil, err)
		return
	}
	out := make([]extentJSON, 0, len(exts))
	for _, e := range exts {
		out = append(out, extentJSON{Off: e.Off, Len: e.Len})
	}
	respond(w, map[string]any{"extents": out}, nil)
}

func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag uint32 `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("files: decoding request: %w", err))
		return
	}
	h, err := s.handle(journal.ShareTag(req.Tag))
	if err != nil {
		respond(w, nil, err)
		return
	}
	out := []string{}
	for id := range h.Files() {
		out = append(out, hexOf(id))
	}
	respond(w, map[string]any{"files": out}, nil)
}

func (s *Server) dirtyFiles(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag uint32 `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, nil, fmt.Errorf("dirty-files: decoding request: %w", err))
		return
	}
	h, err := s.handle(journal.ShareTag(req.Tag))
	if err != nil {
		respond(w, nil, err)
		return
	}
	out := []map[string]any{}
	for id, oldest := range h.DirtyFiles() {
		out = append(out, map[string]any{
			"file":   hexOf(id),
			"oldest": oldest.UnixNano(),
		})
	}
	respond(w, map[string]any{"files": out}, nil)
}

// kindName is the one word Since's kind prints; journalKindWords is the one
// table the CLI rebuilds it from too.
func kindName(k journal.ChangeKind) string {
	if name, ok := journalKindWords[k]; ok {
		return name
	}
	return "kind " + strconv.Itoa(int(k))
}

// changeLine renders one change the way since prints it: kind, extent,
// version, and a write's mtime when it carries one.
func changeLine(ch journal.Change) string {
	line := fmt.Sprintf("%s off=%d len=%d v%s", kindName(ch.Kind), ch.Extent.Off, ch.Extent.Len,
		fmtVersion(ch.Version))
	if ch.Mtime != 0 {
		line += " mtime=" + strconv.FormatInt(ch.Mtime, 10)
	}
	return line
}
