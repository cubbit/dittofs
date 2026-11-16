// Package identityprovider implements the `dfsctl identity-provider` command
// group for managing LDAP/AD and Kerberos identity providers over the REST API.
package identityprovider

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

// Cmd is the parent `identity-provider` command.
var Cmd = &cobra.Command{
	Use:     "identity-provider",
	Aliases: []string{"idp"},
	Short:   "Identity provider (LDAP/AD, Kerberos) management",
	Long: `Manage DittoFS identity providers (LDAP/AD and Kerberos) over the API.

LDAP changes are hot-reloaded by the live identity resolver without a server restart. Kerberos machine-account changes are saved immediately but take effect only on the next server restart. Secret material (bind password, machine secret) is write-only and is never returned by the API.

Examples:
  # List all identity providers and their status
  dfsctl identity-provider list

  # Show the current LDAP configuration (secrets redacted)
  dfsctl identity-provider get ldap

  # Test an LDAP configuration without saving it
  dfsctl identity-provider test ldap --config '{"enabled":true,"url":"ldap://dc.corp.example:389","base_dn":"DC=corp,DC=example","bind_dn":"CN=svc,DC=corp,DC=example","bind_password":"s3cret","idmap":"rfc2307"}'`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(getCmd)
	Cmd.AddCommand(setCmd)
	Cmd.AddCommand(testCmd)
	Cmd.AddCommand(configureCmd)
}

// --- list ---

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List identity providers and their state",
	Long: `List all identity providers configured on the DittoFS server and their current state.

Each row shows the provider type (ldap or kerberos), whether it has been configured, and whether it is enabled. Use 'identity-provider get <type>' to inspect the full configuration for a specific provider.

Examples:
  # Show all identity providers as a table
  dfsctl identity-provider list

  # Get identity provider status as JSON
  dfsctl identity-provider list -o json`, RunE: cmdutil.Unavailable,
}

// --- get ---

var getCmd = &cobra.Command{
	Use:   "get <ldap|kerberos>",
	Short: "Show an identity provider's configuration (secrets redacted)",
	Long: `Show the current configuration of an identity provider with all secret fields redacted.

Use this to verify LDAP or Kerberos settings without exposing sensitive credentials. The output uses the same JSON schema accepted by 'identity-provider set', making it easy to copy-edit and resubmit.

Examples:
  # Show the current LDAP configuration
  dfsctl identity-provider get ldap

  # Show the current Kerberos configuration as JSON
  dfsctl identity-provider get kerberos -o json`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}

// --- set ---

var setConfigJSON string

var setCmd = &cobra.Command{
	Use:   "set <ldap|kerberos> --config '<json>'",
	Short: "Create or replace an identity provider's configuration",
	Long: `Create or replace an identity provider's configuration from a JSON body.

The JSON shape matches the API config schema. For LDAP, set "bind_password" to
the real password (or "********" / omit to keep the stored one). LDAP changes
apply live; Kerberos changes apply on the next server restart.

Examples:
  dfsctl identity-provider set ldap --config '{"enabled":true,"url":"ldaps://dc:636","base_dn":"DC=x,DC=y","bind_dn":"CN=svc,DC=x,DC=y","bind_password":"s3cret","idmap":"rfc2307"}'
  dfsctl identity-provider set kerberos --config @/path/to/krb.json`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}

func init() {
	setCmd.Flags().StringVar(&setConfigJSON, "config", "", "configuration as a JSON string, or @file to read from a file (required)")
	_ = setCmd.MarkFlagRequired("config")
	testCmd.Flags().StringVar(&testConfigJSON, "config", "", "configuration to test as a JSON string, or @file (required)")
	_ = testCmd.MarkFlagRequired("config")
}

// --- test ---

var testConfigJSON string

var testCmd = &cobra.Command{
	Use:   "test <ldap|kerberos> --config '<json>'",
	Short: "Test an identity provider's configuration without persisting it",
	Long: `Validate and test an identity provider configuration against the live server without saving it.

The server attempts to connect to the LDAP or Kerberos endpoint and runs basic reachability checks. On success the command exits 0; on failure it exits non-zero with a description of which stage failed. Use this before 'identity-provider set' to avoid applying a broken configuration.

Examples:
  # Test an LDAP configuration inline
  dfsctl identity-provider test ldap --config '{"enabled":true,"url":"ldap://dc.corp.example:389","base_dn":"DC=corp,DC=example","bind_dn":"CN=svc,DC=corp,DC=example","bind_password":"s3cret","idmap":"rfc2307"}'

  # Test a Kerberos configuration loaded from a file
  dfsctl identity-provider test kerberos --config @/etc/dittofs/krb5-config.json`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}

var configureCmd = newConfigureCmd()

// newConfigureCmd constructs the configure *cobra.Command.
func newConfigureCmd() *cobra.Command {
	var (
		machineAccountEnabled bool
		machineAccountName    string
		machineSecret         string
		machineKeytab         string
		dcAddresses           []string
	)

	cmd := &cobra.Command{
		Use:   "configure kerberos",
		Short: "Configure Kerberos machine-account settings",
		Long: `Set individual Kerberos machine-account flags without replacing the full configuration.

The current configuration is read from the API, the specified flags are applied,
and the result is written back. Fields not specified on the command line are
preserved unchanged.

--machine-secret is write-only: omit it to keep the currently stored credential;
provide a new value to rotate it. Submitting the redacted placeholder ("********")
also preserves the stored secret.

Changes take effect on the next server restart.

Examples:
  dfsctl identity-provider configure kerberos --machine-account-enabled --machine-account-name MYHOST$ --machine-secret 'p@ss' --machine-keytab /etc/krb5.keytab --dc-address 192.0.2.10 --dc-address 192.0.2.11`,
		Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
	}

	cmd.Flags().BoolVar(&machineAccountEnabled, "machine-account-enabled", false,
		"Enable machine-account authentication for NETLOGON")
	cmd.Flags().StringVar(&machineAccountName, "machine-account-name", "",
		"Machine account name (e.g. MYHOST$)")
	cmd.Flags().StringVar(&machineSecret, "machine-secret", "",
		"Machine account password (write-only; omit to keep the stored value)")
	cmd.Flags().StringVar(&machineKeytab, "machine-keytab", "",
		"Path to the machine-account keytab file")
	cmd.Flags().StringArrayVar(&dcAddresses, "dc-address", nil,
		"Domain controller address (repeatable; pass once per address)")

	return cmd
}
