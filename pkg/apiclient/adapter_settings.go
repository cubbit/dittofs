package apiclient

import (
	"fmt"
	"strings"
)

// NFSAdapterSettingsResponse is the response for NFS adapter settings.
type NFSAdapterSettingsResponse struct {
	MinVersion                   string   `json:"min_version"`
	MaxVersion                   string   `json:"max_version"`
	LeaseTime                    int      `json:"lease_time"`
	GracePeriod                  int      `json:"grace_period"`
	DelegationRecallTimeout      int      `json:"delegation_recall_timeout"`
	CallbackTimeout              int      `json:"callback_timeout"`
	LeaseBreakTimeout            int      `json:"lease_break_timeout"`
	MaxConnections               int      `json:"max_connections"`
	MaxClients                   int      `json:"max_clients"`
	MaxCompoundOps               int      `json:"max_compound_ops"`
	MaxReadSize                  int      `json:"max_read_size"`
	MaxWriteSize                 int      `json:"max_write_size"`
	PreferredTransferSize        int      `json:"preferred_transfer_size"`
	DelegationsEnabled           bool     `json:"delegations_enabled"`
	MaxDelegations               int      `json:"max_delegations"`
	DirDelegBatchWindowMs        int      `json:"dir_deleg_batch_window_ms"`
	V4MinMinorVersion            int      `json:"v4_min_minor_version"`
	V4MaxMinorVersion            int      `json:"v4_max_minor_version"`
	V4MaxConnectionsPerSession   int      `json:"v4_max_connections_per_session"`
	BlockedOperations            []string `json:"blocked_operations"`
	PortmapperEnabled            bool     `json:"portmapper_enabled"`
	PortmapperPort               int      `json:"portmapper_port"`
	PortmapperRegisterWithSystem bool     `json:"portmapper_register_with_system"`
	UDPEnabled                   bool     `json:"udp_enabled"`
	MDNSEnabled                  bool     `json:"mdns_enabled"`
	Version                      int      `json:"version"`
}

// SMBAdapterSettingsResponse is the response for SMB adapter settings.
type SMBAdapterSettingsResponse struct {
	MinDialect              string   `json:"min_dialect"`
	MaxDialect              string   `json:"max_dialect"`
	SessionTimeout          int      `json:"session_timeout"`
	OplockBreakTimeout      int      `json:"oplock_break_timeout"`
	MaxConnections          int      `json:"max_connections"`
	MaxSessions             int      `json:"max_sessions"`
	EnableEncryption        bool     `json:"enable_encryption"`
	Signing                 string   `json:"signing"`
	DirectoryLeasingEnabled bool     `json:"directory_leasing_enabled"`
	BlockedOperations       []string `json:"blocked_operations"`
	MDNSEnabled             bool     `json:"mdns_enabled"`
	WSDiscoveryEnabled      bool     `json:"wsdiscovery_enabled"`
	Version                 int      `json:"version"`
}

// PatchNFSSettingsRequest uses pointer fields for partial updates.
type PatchNFSSettingsRequest struct {
	MinVersion                   *string   `json:"min_version,omitempty"`
	MaxVersion                   *string   `json:"max_version,omitempty"`
	LeaseTime                    *int      `json:"lease_time,omitempty"`
	GracePeriod                  *int      `json:"grace_period,omitempty"`
	DelegationRecallTimeout      *int      `json:"delegation_recall_timeout,omitempty"`
	CallbackTimeout              *int      `json:"callback_timeout,omitempty"`
	LeaseBreakTimeout            *int      `json:"lease_break_timeout,omitempty"`
	MaxConnections               *int      `json:"max_connections,omitempty"`
	MaxClients                   *int      `json:"max_clients,omitempty"`
	MaxCompoundOps               *int      `json:"max_compound_ops,omitempty"`
	MaxReadSize                  *int      `json:"max_read_size,omitempty"`
	MaxWriteSize                 *int      `json:"max_write_size,omitempty"`
	PreferredTransferSize        *int      `json:"preferred_transfer_size,omitempty"`
	DelegationsEnabled           *bool     `json:"delegations_enabled,omitempty"`
	MaxDelegations               *int      `json:"max_delegations,omitempty"`
	DirDelegBatchWindowMs        *int      `json:"dir_deleg_batch_window_ms,omitempty"`
	V4MinMinorVersion            *int      `json:"v4_min_minor_version,omitempty"`
	V4MaxMinorVersion            *int      `json:"v4_max_minor_version,omitempty"`
	V4MaxConnectionsPerSession   *int      `json:"v4_max_connections_per_session,omitempty"`
	BlockedOperations            *[]string `json:"blocked_operations,omitempty"`
	PortmapperEnabled            *bool     `json:"portmapper_enabled,omitempty"`
	PortmapperPort               *int      `json:"portmapper_port,omitempty"`
	PortmapperRegisterWithSystem *bool     `json:"portmapper_register_with_system,omitempty"`
	UDPEnabled                   *bool     `json:"udp_enabled,omitempty"`
	MDNSEnabled                  *bool     `json:"mdns_enabled,omitempty"`
}

// PatchSMBSettingsRequest uses pointer fields for partial updates.
type PatchSMBSettingsRequest struct {
	MinDialect              *string   `json:"min_dialect,omitempty"`
	MaxDialect              *string   `json:"max_dialect,omitempty"`
	SessionTimeout          *int      `json:"session_timeout,omitempty"`
	OplockBreakTimeout      *int      `json:"oplock_break_timeout,omitempty"`
	MaxConnections          *int      `json:"max_connections,omitempty"`
	MaxSessions             *int      `json:"max_sessions,omitempty"`
	EnableEncryption        *bool     `json:"enable_encryption,omitempty"`
	Signing                 *string   `json:"signing,omitempty"`
	DirectoryLeasingEnabled *bool     `json:"directory_leasing_enabled,omitempty"`
	BlockedOperations       *[]string `json:"blocked_operations,omitempty"`
	MDNSEnabled             *bool     `json:"mdns_enabled,omitempty"`
	WSDiscoveryEnabled      *bool     `json:"wsdiscovery_enabled,omitempty"`
}

// SettingsOption is a functional option for settings API calls.
type SettingsOption func(url *string)

// withQueryParam returns a SettingsOption that appends a query parameter to the URL.
func withQueryParam(param string) SettingsOption {
	return func(url *string) {
		if strings.Contains(*url, "?") {
			*url += "&" + param
		} else {
			*url += "?" + param
		}
	}
}

// WithForce adds the force=true query parameter to bypass range validation.
func WithForce() SettingsOption {
	return withQueryParam("force=true")
}

// WithDryRun adds the dry_run=true query parameter to validate without applying.
func WithDryRun() SettingsOption {
	return withQueryParam("dry_run=true")
}

func applySettingsOptions(url string, opts []SettingsOption) string {
	for _, opt := range opts {
		opt(&url)
	}
	return url
}

// GetNFSSettings returns the NFS adapter settings.
func (c *Client) GetNFSSettings() (*NFSAdapterSettingsResponse, error) {
	var resp NFSAdapterSettingsResponse
	if err := c.get("/api/v1/adapters/nfs/settings", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// UpdateNFSSettings performs a full replacement (PUT) of NFS adapter settings.
func (c *Client) UpdateNFSSettings(req any, opts ...SettingsOption) (*NFSAdapterSettingsResponse, error) {
	url := applySettingsOptions("/api/v1/adapters/nfs/settings", opts)
	var resp NFSAdapterSettingsResponse
	if err := c.put(url, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PatchNFSSettings performs a partial update (PATCH) of NFS adapter settings.
func (c *Client) PatchNFSSettings(req *PatchNFSSettingsRequest, opts ...SettingsOption) (*NFSAdapterSettingsResponse, error) {
	url := applySettingsOptions("/api/v1/adapters/nfs/settings", opts)
	var resp NFSAdapterSettingsResponse
	if err := c.patch(url, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PatchSMBSettings performs a partial update (PATCH) of SMB adapter settings.
func (c *Client) PatchSMBSettings(req *PatchSMBSettingsRequest, opts ...SettingsOption) (*SMBAdapterSettingsResponse, error) {
	url := applySettingsOptions("/api/v1/adapters/smb/settings", opts)
	var resp SMBAdapterSettingsResponse
	if err := c.patch(url, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ResetNFSSettings resets NFS adapter settings to defaults.
// If setting is non-empty, only that specific setting is reset.
func (c *Client) ResetNFSSettings(setting string) (*NFSAdapterSettingsResponse, error) {
	url := "/api/v1/adapters/nfs/settings/reset"
	if setting != "" {
		url += fmt.Sprintf("?setting=%s", setting)
	}
	var resp NFSAdapterSettingsResponse
	if err := c.post(url, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
