package commands

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	loginServer      string
	loginUsername    string
	loginPassword    string
	loginContextName string

	loginCACert        string
	loginClientCert    string
	loginClientKey     string
	loginTLSSkipVerify bool
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate with DittoFS server",
	Long: `Authenticate with a DittoFS server and store credentials locally.

Contacts the control-plane API at the given server URL, exchanges credentials
for an access/refresh token pair, and saves them to ~/.config/dfsctl/config.json
under a named context (default: auto-derived from the server URL). Subsequent
commands reuse the stored token and refresh it automatically when it expires.
TLS client certificates and a custom CA bundle can be pinned at login time and
are persisted into the context for later commands.

Examples:
  # First login to a local server (prompts for password)
  dfsctl login --server http://localhost:8080 --username admin

  # Login to a remote server with mutual TLS
  dfsctl login --server https://dfs.example.com --username admin --cacert ca.pem --client-cert client.pem --client-key client.key

  # Login passing password on the command line (less secure; avoid in shared environments)
  dfsctl login --server http://localhost:8080 -u admin -p secret

  # Re-login to the already-stored server (password prompt only)
  dfsctl login`, RunE: cmdutil.Unavailable,
}

func init() {
	loginCmd.Flags().StringVar(&loginServer, "server", "http://localhost:8080", "Server URL")
	loginCmd.Flags().StringVarP(&loginUsername, "username", "u", "", "Username")
	loginCmd.Flags().StringVarP(&loginPassword, "password", "p", "", "Password")
	loginCmd.Flags().StringVarP(&loginContextName, "context", "c", "", "Context name (defaults to current or auto-generated)")
	loginCmd.Flags().StringVar(&loginCACert, "cacert", "", "Path to a PEM CA bundle trusted for the server certificate")
	loginCmd.Flags().StringVar(&loginClientCert, "client-cert", "", "Path to a PEM client certificate for mutual TLS")
	loginCmd.Flags().StringVar(&loginClientKey, "client-key", "", "Path to the PEM client private key for mutual TLS")
	loginCmd.Flags().BoolVar(&loginTLSSkipVerify, "tls-skip-verify", false, "Disable TLS certificate verification (insecure)")
}
