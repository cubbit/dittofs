package share

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	editBlockStore        string
	editReadOnly          string
	editEncryptData       string
	editDefaultPermission string
	editDescription       string
	editRetention         string
	editRetentionTTL      string
	editJournalSize       string
	editCommitAck         string
	editRelaxedMetaCommit bool
	editReadBufferSize    string
	editQuotaBytes        string
	editAclCanonicalize   string
	editAccessBasedEnum   string
	editEnableTrash       string
	editTrashRetention    int
	editTrashRestrictAdm  string
	editTrashMaxSize      int64
	editTrashExclude      []string
)

var editCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Edit a share",
	Long: `Edit an existing share on the DittoFS server.

When run without flags, opens an interactive editor to modify share properties.
When flags are provided, only the specified fields are updated.

Examples:
  # Edit share interactively
  dfsctl share edit /archive

  # Update the block store reference
  dfsctl share edit /archive --block-store new-s3-store

  # Make share read-only
  dfsctl share edit /archive --read-only true

  # Make share writable
  dfsctl share edit /archive --read-only false

  # Set default permission to allow all users read-write access
  dfsctl share edit /archive --default-permission read-write

  # Update description
  dfsctl share edit /archive --description "New description"

  # Change retention policy to pin (blocks never evicted)
  dfsctl share edit /archive --retention pin

  # Change retention policy to TTL with 72-hour window
  dfsctl share edit /archive --retention ttl --retention-ttl 72h

  # Override the per-share journal size
  dfsctl share edit /archive --journal-size 10GiB

  # Override per-share read buffer size
  dfsctl share edit /archive --read-buffer-size 2GiB

  # Set per-share quota
  dfsctl share edit /archive --quota-bytes 10GiB

  # Remove quota (set to unlimited)
  dfsctl share edit /archive --quota-bytes 0`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}

func init() {
	editCmd.Flags().StringVar(&editBlockStore, "block-store", "", "Block store name")
	editCmd.Flags().StringVar(&editReadOnly, "read-only", "", "Set read-only (true|false)")
	editCmd.Flags().StringVar(&editEncryptData, "encrypt-data", "", "Require SMB3 encryption (true|false)")
	editCmd.Flags().StringVar(&editDefaultPermission, "default-permission", "", "Default permission (none|read|read-write|admin)")
	editCmd.Flags().StringVar(&editDescription, "description", "", "Share description")
	editCmd.Flags().StringVar(&editRetention, "retention", "", "Retention policy (pin|ttl|lru)")
	editCmd.Flags().StringVar(&editRetentionTTL, "retention-ttl", "", "Retention TTL duration (e.g., 72h)")
	editCmd.Flags().StringVar(&editJournalSize, "journal-size", "", "Per-share journal size override (e.g., 10GiB, 500MiB)")
	editCmd.Flags().StringVar(&editCommitAck, "commit-ack", "", "What a COMMIT waits for: journal (survives host crash) or block-store (survives device loss)")
	editCmd.Flags().BoolVar(&editRelaxedMetaCommit, "relaxed-metadata-commit", false, "Let an operation that promised stable metadata return before the metadata fsync")
	editCmd.Flags().StringVar(&editReadBufferSize, "read-buffer-size", "", "Per-share read buffer size override (e.g., 2GiB, 256MiB)")
	editCmd.Flags().StringVar(&editQuotaBytes, "quota-bytes", "", "Per-share byte quota (e.g., '10GiB'). 0 = remove quota")
	editCmd.Flags().StringVar(&editAclCanonicalize, "acl-canonicalize-inherited", "", "When false, preserves the SE_DACL_AUTO_INHERITED control bit verbatim on SET_INFO Security instead of applying MS-DTYP §2.5.3.4.2 canonicalization (Samba \"acl flag inherited canonicalization = no\"). Default true matches Windows. Takes effect on adapter restart.")
	editCmd.Flags().StringVar(&editAccessBasedEnum, "access-based-enumeration", "", "Enable/disable Windows access-based enumeration (true|false). Takes effect on adapter restart.")
	editCmd.Flags().StringVar(&editEnableTrash, "enable-trash", "", "Enable/disable the per-share recycle bin (true|false). Applied live; disabling auto-empties the bin.")
	editCmd.Flags().IntVar(&editTrashRetention, "trash-retention-days", -1, "Days to retain recycled items before the reaper purges them (0 = keep forever). -1 leaves unchanged.")
	editCmd.Flags().StringVar(&editTrashRestrictAdm, "trash-restrict-empty-to-admin", "", "Restrict emptying the recycle bin to admins (true|false).")
	editCmd.Flags().Int64Var(&editTrashMaxSize, "trash-max-size", -1, "Max bytes the recycle bin may hold before the reaper evicts oldest items (0 = unbounded). -1 leaves unchanged.")
	editCmd.Flags().StringSliceVar(&editTrashExclude, "trash-exclude", nil, "Glob patterns whose deletions bypass the recycle bin (repeatable).")
}
