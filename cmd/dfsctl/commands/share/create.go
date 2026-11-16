package share

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	createName              string
	createMetadata          string
	createBlockStore        string
	createReadOnly          bool
	createEncryptData       bool
	createDefaultPermission string
	createSquash            string
	createOwner             string
	createDescription       string
	createRetention         string
	createRetentionTTL      string
	createJournalSize       string
	createCommitAck         string
	createRelaxedMetaCommit bool
	createReadBufferSize    string
	createQuotaBytes        string
	createAclCanonicalize   bool
	createAccessBasedEnum   bool
	createChangeNotifyOff   bool
	createStreamsDisabled   bool
	createContinuousAvail   bool
	createAllowMFsymlink    bool
	createEnableTrash       bool
	createTrashRetention    int
	createTrashRestrictAdm  bool
	createTrashMaxSize      int64
	createTrashExclude      []string
)

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new share",
	Long: `Create a new share on the DittoFS server.

A share requires a metadata store and a block store. The block store holds the
durable copy of the data; each share also keeps a local journal on disk.

Examples:
  # Create a share
  dfsctl share create --name /data --metadata default --block-store s3-store

  # Create a read-only share
  dfsctl share create --name /readonly --metadata default --block-store s3-store --read-only

  # Create with default permission allowing all users read-write access
  dfsctl share create --name /shared --metadata default --block-store s3-store --default-permission read-write

  # Create with description
  dfsctl share create --name /docs --metadata default --block-store s3-store --description "Documentation files"

  # Create a pinned share (blocks never evicted)
  dfsctl share create --name /edge-data --metadata default --block-store s3-store --retention pin

  # Create with TTL retention (evict after 72 hours of no access)
  dfsctl share create --name /logs --metadata default --block-store s3-store --retention ttl --retention-ttl 72h

  # Create with per-share cache size overrides
  dfsctl share create --name /bigdata --metadata default --block-store s3-store --journal-size 10GiB --read-buffer-size 2GiB

  # Create with per-share quota
  dfsctl share create --name /limited --metadata default --block-store s3-store --quota-bytes 10GiB

  # Create an export that does not squash root (e.g. for root-mounted/benchmark clients)
  dfsctl share create --name /export --metadata default --block-store s3-store --squash none`, RunE: cmdutil.Unavailable,
}

func init() {
	createCmd.Flags().StringVar(&createName, "name", "", "Share name/path (required)")
	createCmd.Flags().StringVar(&createMetadata, "metadata", "", "Metadata store name (required)")
	createCmd.Flags().StringVar(&createBlockStore, "block-store", "", "Block store name (required)")
	createCmd.Flags().BoolVar(&createReadOnly, "read-only", false, "Make share read-only")
	createCmd.Flags().BoolVar(&createEncryptData, "encrypt-data", false, "Require SMB3 encryption for this share")
	createCmd.Flags().StringVar(&createDefaultPermission, "default-permission", "none", "Default permission for unmapped UIDs (none|read|read-write|admin)")
	createCmd.Flags().StringVar(&createSquash, "squash", "", "Squash mode, for NFS and SMB (none|root_to_admin|root_to_guest|all_to_admin|all_to_guest). Default root_to_guest (root_squash); use none or root_to_admin so a UID 0 caller (a root-mounted NFS client, the built-in SMB admin) is not squashed to guest.")
	createCmd.Flags().StringVar(&createOwner, "owner", "", "Username that owns the share's root directory (defaults to root). The owner can write at the share root; other principals are governed by POSIX mode plus their share permission grant.")
	createCmd.Flags().StringVar(&createDescription, "description", "", "Share description")
	createCmd.Flags().StringVar(&createRetention, "retention", "", "Retention policy (pin|ttl|lru)")
	createCmd.Flags().StringVar(&createRetentionTTL, "retention-ttl", "", "Retention TTL duration (e.g., 72h, 24h)")
	createCmd.Flags().StringVar(&createJournalSize, "journal-size", "", "Per-share journal size override (e.g., 10GiB, 500MiB)")
	createCmd.Flags().StringVar(&createCommitAck, "commit-ack", "", "What a COMMIT waits for: journal (survives host crash) or block-store (survives device loss)")
	createCmd.Flags().BoolVar(&createRelaxedMetaCommit, "relaxed-metadata-commit", false, "Let an operation that promised stable metadata return before the metadata fsync")
	createCmd.Flags().StringVar(&createReadBufferSize, "read-buffer-size", "", "Per-share read buffer size override (e.g., 2GiB, 256MiB)")
	createCmd.Flags().StringVar(&createQuotaBytes, "quota-bytes", "", "Per-share byte quota (e.g., '10GiB', '500MiB'). 0 = unlimited (default)")
	createCmd.Flags().BoolVar(&createAclCanonicalize, "acl-canonicalize-inherited", true, "When false, preserves the SE_DACL_AUTO_INHERITED control bit verbatim on SET_INFO Security instead of applying MS-DTYP §2.5.3.4.2 canonicalization (Samba \"acl flag inherited canonicalization = no\"). Default true matches Windows.")
	createCmd.Flags().BoolVar(&createAccessBasedEnum, "access-based-enumeration", false, "Enable Windows access-based enumeration (SHI1005_FLAGS_ACCESS_BASED_DIRECTORY_ENUM). When true, SMB clients only see directory entries they can read.")
	createCmd.Flags().BoolVar(&createChangeNotifyOff, "change-notify-disabled", false, "Reject SMB2 CHANGE_NOTIFY with STATUS_NOT_IMPLEMENTED on this share (mirrors Samba 'kernel change notify = no').")
	createCmd.Flags().BoolVar(&createStreamsDisabled, "streams-disabled", false, "Reject SMB2 Alternate Data Stream opens with STATUS_OBJECT_NAME_INVALID on this share (mirrors Samba 'smbd:streams = no').")
	createCmd.Flags().BoolVar(&createContinuousAvail, "continuous-availability", false, "Advertise SMB2_SHARE_CAP_CONTINUOUS_AVAILABILITY and allow SMB3 persistent durable handles on this share.")
	createCmd.Flags().BoolVar(&createAllowMFsymlink, "allow-mfsymlink", false, "Convert 1067-byte XSym (Minshall+French) symlink files written by macOS/Windows SMB clients into real symlinks on CLOSE. Off by default (XSym files are stored as regular files).")
	createCmd.Flags().BoolVar(&createEnableTrash, "enable-trash", false, "Enable the per-share recycle bin so deletes move to #recycle instead of being permanent.")
	createCmd.Flags().IntVar(&createTrashRetention, "trash-retention-days", 0, "Days to retain recycled items before the reaper purges them (0 = keep forever).")
	createCmd.Flags().BoolVar(&createTrashRestrictAdm, "trash-restrict-empty-to-admin", false, "Restrict emptying the recycle bin to admins.")
	createCmd.Flags().Int64Var(&createTrashMaxSize, "trash-max-size", 0, "Max bytes the recycle bin may hold before the reaper evicts oldest items (0 = unbounded).")
	createCmd.Flags().StringSliceVar(&createTrashExclude, "trash-exclude", nil, "Glob patterns whose deletions bypass the recycle bin (repeatable).")
	_ = createCmd.MarkFlagRequired("block-store")
}
