package handlers

import (
	"net/http"
	"time"

	"github.com/cubbit/dittofs/pkg/controlplane/runtime"
)

// MountHandler handles unified mount management API endpoints.
type MountHandler struct {
	rt *runtime.Runtime
}

// NewMountHandler creates a handler for mount endpoints.
func NewMountHandler(rt *runtime.Runtime) *MountHandler {
	return &MountHandler{rt: rt}
}

// MountInfoResponse is the JSON response for mount listing endpoints.
type MountInfoResponse struct {
	ClientAddr string `json:"client_addr"`
	Protocol   string `json:"protocol"`
	ShareName  string `json:"share_name"`
	MountedAt  string `json:"mounted_at"`
}

// List handles GET /api/v1/mounts - returns all active mounts across protocols.
func (h *MountHandler) List(w http.ResponseWriter, r *http.Request) {
	mounts := h.rt.Mounts().List()
	result := make([]MountInfoResponse, 0, len(mounts))
	for _, m := range mounts {
		result = append(result, MountInfoResponse{
			ClientAddr: m.ClientAddr,
			Protocol:   m.Protocol,
			ShareName:  m.ShareName,
			MountedAt:  m.MountedAt.Format(time.RFC3339),
		})
	}
	WriteJSONOK(w, result)
}
