package handlers

import (
	"net/http"

	"github.com/cubbit/dittofs/pkg/controlplane/runtime"
)

// ClientHandler handles unified client listing API endpoints, read from the
// ClientRegistry.
type ClientHandler struct {
	rt *runtime.Runtime
}

// NewClientHandler creates a handler for unified client endpoints.
// Returns nil if rt is nil.
func NewClientHandler(rt *runtime.Runtime) *ClientHandler {
	if rt == nil {
		return nil
	}
	return &ClientHandler{rt: rt}
}

// List handles GET /api/v1/clients.
// Supports a ?protocol=nfs|smb query filter.
func (h *ClientHandler) List(w http.ResponseWriter, r *http.Request) {
	registry := h.rt.Clients()

	if protocol := r.URL.Query().Get("protocol"); protocol != "" {
		WriteJSONOK(w, registry.ListByProtocol(protocol))
		return
	}
	WriteJSONOK(w, registry.List())
}
