package share

import (
	"fmt"
	"strings"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/spf13/cobra"
)

// policyFlags gives cmd a flag for each field of an export policy.
func policyFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringSlice("flavor", nil, "The flavours SECINFO answers, in preference order: none, sys or kerberos")
	f.String("min-proof", "", "The proof every caller gives: none, claimed, secret or ticket")
	f.String("min-protection", "", "The protection messages need: none, integrity or privacy")
	f.Bool("read-only", false, "Hold every caller to read-only")
	f.StringArray("client", nil, "A client rule, CIDR=access or @netgroup=access, access none, read or read-write; the first match decides, in order")
	f.String("squash", "", "Whose identity is squashed: none, root or all")
	f.Bool("smb-encrypt", false, "Seal every SMB message")
	f.Bool("smb-hidden", false, "Leave the share out of SMB share enumeration")
	f.Bool("smb-continuously-available", false, "Make the share's SMB handles continuously available")
}

// applyPolicy sets each field of p whose flag cmd was given, leaving the
// others as they are; --client replaces every client rule.
func applyPolicy(cmd *cobra.Command, p *apiv1.ExportPolicySpec) error {
	f := cmd.Flags()
	for flag, into := range map[string]*string{"min-proof": &p.MinProof, "min-protection": &p.MinProtection, "squash": &p.Squash} {
		if f.Changed(flag) {
			*into, _ = f.GetString(flag)
		}
	}
	for flag, into := range map[string]*bool{"read-only": &p.ReadOnly, "smb-encrypt": &p.SMB.Encrypt,
		"smb-hidden": &p.SMB.Hidden, "smb-continuously-available": &p.SMB.ContinuouslyAvailable} {
		if f.Changed(flag) {
			*into, _ = f.GetBool(flag)
		}
	}
	if f.Changed("flavor") {
		p.Flavors, _ = f.GetStringSlice("flavor")
	}
	if f.Changed("client") {
		rules, _ := f.GetStringArray("client")
		p.Clients = make([]apiv1.ClientRule, 0, len(rules))
		for _, r := range rules {
			match, access, ok := strings.Cut(r, "=")
			if !ok {
				return fmt.Errorf("%s: --client %q: want CIDR=access or @netgroup=access", cmd.CommandPath(), r)
			}
			rule := apiv1.ClientRule{CIDR: match, Access: access}
			if ng, ok := strings.CutPrefix(match, "@"); ok {
				rule = apiv1.ClientRule{Netgroup: ng, Access: access}
			}
			p.Clients = append(p.Clients, rule)
		}
	}
	return nil
}

// policies is export policies as a table.
type policies []apiv1.ExportPolicy

// Headers implements output.TableRenderer.
func (policies) Headers() []string {
	return []string{"SHARE", "FLAVORS", "MIN-PROOF", "MIN-PROTECTION", "READ-ONLY", "SQUASH", "CLIENTS"}
}

// Rows implements output.TableRenderer.
func (ps policies) Rows() [][]string {
	rows := make([][]string, 0, len(ps))
	for _, p := range ps {
		clients := make([]string, 0, len(p.Spec.Clients))
		for _, c := range p.Spec.Clients {
			match := c.CIDR
			if c.Netgroup != "" {
				match = "@" + c.Netgroup
			}
			clients = append(clients, match+"="+c.Access)
		}
		rows = append(rows, []string{p.ID, strings.Join(p.Spec.Flavors, ","), p.Spec.MinProof, p.Spec.MinProtection,
			cmdutil.BoolToYesNo(p.Spec.ReadOnly), p.Spec.Squash, strings.Join(clients, " ")})
	}
	return rows
}

func newExportPolicyCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export-policy",
		Short: "Read and write a share's export policy",
		Long: `Read and write a share's export policy: who it admits, with which proof and
protection, how it squashes identities, and its SMB flags.

'set' writes the whole policy, --flavor and --client required, any other field
whose flag is not given taken back to its default; 'update' changes only the fields whose flags are given.

Examples:
  # Admit one network read-write and one netgroup read-only
  dfsctl share export-policy set archive --flavor sys --client 10.0.0.0/8=read-write --client @auditors=read

  # Require Kerberos with privacy, leaving the rest as it is
  dfsctl share export-policy update archive --flavor kerberos --min-proof ticket --min-protection privacy`,
	}
	get := &cobra.Command{
		Use:   "get [share]",
		Short: "Show a share's export policy",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			id, err := cmdutil.Resolve(cmd, c.Shares(), "share", args)
			if err != nil {
				return err
			}
			p, err := c.ExportPolicies().Get(cmd.Context(), id)
			if err != nil {
				return fmt.Errorf("%s: share %s: %w", cmd.CommandPath(), id, err)
			}
			return cmdutil.PrintOutput(cmd, p.Resource, false, "", policies{p.Resource})
		},
	}
	cmdutil.NameOrID(get, "share")
	cmd.AddCommand(get,
		newPolicyWriteCmd(env, "set", "Write a share's whole export policy", true),
		newPolicyWriteCmd(env, "update", "Change fields of a share's export policy", false))
	return cmd
}

// newPolicyWriteCmd is set, whole, which writes the policy its flags
// describe, or update, which changes only the fields whose flags are given.
func newPolicyWriteCmd(env cmdutil.Env, verb, short string, whole bool) *cobra.Command {
	cmd := &cobra.Command{
		Use:   verb + " [share]",
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			id, err := cmdutil.Resolve(cmd, c.Shares(), "share", args)
			if err != nil {
				return err
			}
			write := cmdutil.ByPatch(c.ExportPolicies())
			if whole {
				write = cmdutil.ByPut(c.ExportPolicies())
			}
			res, err := cmdutil.Update(cmd, env, c.ExportPolicies(), write, "export policy of share", id,
				func(p *apiv1.ExportPolicySpec) error {
					if whole {
						*p = apiv1.ExportPolicySpec{}
					}
					return applyPolicy(cmd, p)
				})
			if err != nil {
				return err
			}
			return cmdutil.PrintOutput(cmd, res.Resource.Resource, false, "", policies{res.Resource.Resource})
		},
	}
	cmdutil.NameOrID(cmd, "share")
	policyFlags(cmd)
	if whole {
		requirePolicy(cmd)
	}
	return cmd
}

// requirePolicy makes cmd, which writes a whole policy, require the two
// fields a policy has no default for: its flavours and its client rules.
func requirePolicy(cmd *cobra.Command) {
	_ = cmd.MarkFlagRequired("flavor")
	_ = cmd.MarkFlagRequired("client")
}
