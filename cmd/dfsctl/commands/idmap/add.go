package idmap

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	addProvider  string
	addPrincipal string
	addUsername  string
)

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add an identity mapping",
	Long: `Add a new identity mapping that links an external authentication principal
to a local DittoFS user account. This is how Kerberos, OIDC, and Active Directory
identities are mapped to the local user that owns their files and holds their
permissions. The --provider flag selects the identity provider and defaults to
"kerberos".

Examples:
  # Map a Kerberos principal to a local user (default provider)
  dfsctl idmap add --principal alice@EXAMPLE.COM --username alice

  # Map an NTLM domain user with the AD provider
  dfsctl idmap add --provider ad --principal CORP\\alice --username alice

  # Map an OIDC subject claim to a local user
  dfsctl idmap add --provider oidc --principal sub:abc123 --username bob

  # Map a Kerberos admin principal to the local admin account
  dfsctl idmap add --principal admin@CORP.COM --username admin`, RunE: cmdutil.Unavailable,
}

func init() {
	addCmd.Flags().StringVar(&addProvider, "provider", "kerberos", "Identity provider (e.g., kerberos, oidc, ad)")
	addCmd.Flags().StringVar(&addPrincipal, "principal", "", "External identity (e.g., alice@EXAMPLE.COM)")
	addCmd.Flags().StringVar(&addUsername, "username", "", "DittoFS username")
	_ = addCmd.MarkFlagRequired("principal")
	_ = addCmd.MarkFlagRequired("username")
}
