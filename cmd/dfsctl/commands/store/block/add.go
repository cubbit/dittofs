package block

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	addName   string
	addType   string
	addConfig string
	// S3 specific
	addBucket          string
	addRegion          string
	addEndpoint        string
	addPrefix          string
	addAccessKey       string
	addSecretKey       string
	addCompression     string
	addParallelUploads int
	// Encryption (client-side, optional)
	addEncryptionAEAD       string
	addEncryptionKeyKind    string
	addEncryptionKeyFile    string
	addEncryptionKMIPHost   string
	addEncryptionKMIPCA     string
	addEncryptionKMIPCert   string
	addEncryptionKMIPKey    string
	addEncryptionKMIPKeyUID string
)

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a block store",
	Long: `Add a new block store to the DittoFS server.

Supported types:
  - s3: AWS S3 or S3-compatible store (durable, production)
  - memory: In-memory store (fast, ephemeral, for testing)

Type-specific options:
  s3:
    --bucket: S3 bucket name (or prompted interactively)
    --region: AWS region (default: us-east-1)
    --endpoint: Custom endpoint for S3-compatible stores
    --prefix: Key prefix within the bucket
    --access-key: AWS access key ID
    --secret-key: AWS secret access key

  Every s3 option left off the command line is asked for individually when
  stdin is a terminal. An empty endpoint means AWS S3; the resolved target is
  echoed before the store is created.

--config gives the store config as JSON. Flags passed with it are merged into
it; a flag that contradicts a key the JSON sets is refused.

Examples:
  # Add an S3 store with flags
  dfsctl store block add --name s3-store --type s3 --bucket my-bucket --region us-west-2

  # Add an S3 store interactively
  dfsctl store block add --name s3-store --type s3

  # Add a MinIO store (S3-compatible)
  dfsctl store block add --name minio-store --type s3 --bucket data --endpoint http://localhost:9000

  # Add an S3 store with zstd block compression
  dfsctl store block add --name prod-s3 --type s3 --bucket my-bucket --compression zstd

  # Add a memory store (for testing)
  dfsctl store block add --name test-remote --type memory`, RunE: cmdutil.Unavailable,
}

func init() {
	addCmd.Flags().StringVar(&addName, "name", "", "Store name (required)")
	addCmd.Flags().StringVar(&addType, "type", "s3", "Store type: s3, memory")
	addCmd.Flags().StringVar(&addConfig, "config", "", "Store configuration as JSON (flags passed with it are merged in)")
	// S3 flags
	addCmd.Flags().StringVar(&addBucket, "bucket", "", "S3 bucket name (required for s3)")
	addCmd.Flags().StringVar(&addRegion, "region", defaultS3Region, "AWS region (for s3)")
	addCmd.Flags().StringVar(&addEndpoint, "endpoint", "", "Custom S3 endpoint (for S3-compatible stores)")
	addCmd.Flags().StringVar(&addPrefix, "prefix", "", "Key prefix within the bucket (for s3)")
	addCmd.Flags().StringVar(&addAccessKey, "access-key", "", "AWS access key ID (for s3)")
	addCmd.Flags().StringVar(&addSecretKey, "secret-key", "", "AWS secret access key (for s3)")
	addCmd.Flags().StringVar(&addCompression, "compression", "", "Enable per-block compression: zstd, lz4 (default: off)")
	addCmd.Flags().IntVar(&addParallelUploads, "parallel-uploads", 0, "Max parallel chunk uploads to this remote (0 = adaptive: auto-tune to saturate the uplink)")
	// Encryption flags
	addCmd.Flags().StringVar(&addEncryptionAEAD, "encryption-aead", "", "Enable client-side encryption with the given AEAD: aes-256-gcm, chacha20-poly1305, xchacha20-poly1305")
	addCmd.Flags().StringVar(&addEncryptionKeyKind, "encryption-key-kind", "", "Key provider: local | kmip (required when --encryption-aead is set)")
	addCmd.Flags().StringVar(&addEncryptionKeyFile, "encryption-key-file", "", "Path to local key file (kind=local)")
	addCmd.Flags().StringVar(&addEncryptionKMIPHost, "encryption-kmip-endpoint", "", "KMIP server endpoint host:port (kind=kmip)")
	addCmd.Flags().StringVar(&addEncryptionKMIPCA, "encryption-kmip-ca", "", "KMIP server CA bundle (kind=kmip, optional)")
	addCmd.Flags().StringVar(&addEncryptionKMIPCert, "encryption-kmip-cert", "", "KMIP client certificate (kind=kmip)")
	addCmd.Flags().StringVar(&addEncryptionKMIPKey, "encryption-kmip-key", "", "KMIP client private key (kind=kmip)")
	addCmd.Flags().StringVar(&addEncryptionKMIPKeyUID, "encryption-kmip-key-uid", "", "KMIP managed symmetric key UID (kind=kmip)")
	_ = addCmd.MarkFlagRequired("name")
}

// defaultS3Region is the region an s3 store falls back to when the operator
// names none: the --region default, and the answer its prompt offers.
const defaultS3Region = "us-east-1"
