// Package netlogon implements the `dfsctl netlogon` command group for inspecting
// and controlling the NETLOGON machine-account (SMB NTLM pass-through) subsystem
// on a running DittoFS server.
package netlogon

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

// Cmd is the parent `netlogon` command.
var Cmd = &cobra.Command{
	Use:   "netlogon",
	Short: "Inspect and control the NETLOGON machine account (SMB NTLM pass-through)",
	Long: `Inspect and control the NETLOGON machine account used for SMB NTLM pass-through
of Active Directory domain users on the running DittoFS server.

When a client connects by a name with no Kerberos SPN (an IP, or the Explorer →
Network discovery name), Windows falls back to NTLM; DittoFS validates that NTLM
response against a domain controller over a NETLOGON secure channel using a
machine account. These commands report the live machine-account / secure-channel
state and force a machine-password rotation.

Unlike 'dfs netlogon test' (a self-contained probe run from the config file),
these talk to the RUNNING server over the API and require admin credentials.

Examples:
  # Show live machine-account and secure-channel state
  dfsctl netlogon status

  # Force a machine-password rotation now (online-join only)
  dfsctl netlogon rotate`,
}

func init() {
	Cmd.AddCommand(statusCmd)
	Cmd.AddCommand(rotateCmd)
}

// --- status ---

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show live NETLOGON machine-account and secure-channel state",
	Long: `Report the live state of the NETLOGON machine account on the running server:
the active provider (offline or online-join), the machine-account name, the
realm / NetBIOS domain / DC binding, whether the account is joined, whether the
secure channel is currently established, and the automatic-rotation schedule.

Examples:
  # Show status as a table
  dfsctl netlogon status

  # Show status as JSON
  dfsctl netlogon status -o json`,
	Args: cobra.NoArgs, RunE: cmdutil.Unavailable,
}

// --- rotate ---

var rotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "Force a machine-account password rotation now",
	Long: `Force an immediate rotation of the machine-account password on the running server.

Rotation applies only to the online-join provider, which owns the machine-password
lifecycle: the new password is set on the domain controller (NetrServerPasswordSet2),
switched in memory, and persisted — keeping the stored secret in sync with the DC.
The offline/static provider owns no password lifecycle, so this command returns an
error there; rotate that secret by updating the machine-account configuration.

Examples:
  # Rotate the machine password now
  dfsctl netlogon rotate`,
	Args: cobra.NoArgs, RunE: cmdutil.Unavailable,
}
