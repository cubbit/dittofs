package content

import (
	"fmt"

	"github.com/cubbit/dittofs/internal/bytesize"
	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/remote/codec"
	"github.com/cubbit/dittofs/pkg/remote/s3"
	"github.com/cubbit/dittofs/pkg/settings"
)

// remoteSettings is a remote store's section (RFC 13 Appendix B): the S3
// profile's own settings beside the two fields s3 may not type, the
// credential's reference and the block target, whose range is the carver's.
type remoteSettings struct {
	s3.Settings
	// Credential names the sealed secret holding the access key and the
	// secret; a plaintext in its place is refused before Validate runs.
	Credential entity.SecretRef `json:"credential" class:"live" location:"store"`
	// BlockTarget is the carried bytes per block, read with RFC 13's size
	// grammar ("8MiB").
	BlockTarget bytesize.ByteSize `json:"block_target" class:"next write"`
}

// Validate refuses what s3.Settings.Validate refuses and a block target
// outside the carver's range, never clamping it (RFC 2 §5.2). It never prints
// the credential's reference.
func (r remoteSettings) Validate() error {
	if err := r.Settings.Validate(); err != nil {
		return err
	}
	if r.BlockTarget.Uint64() < carver.MinBlockTarget || r.BlockTarget.Uint64() > carver.MaxBlockTarget {
		return fmt.Errorf("block_target: %s is outside %s to %s",
			r.BlockTarget, bytesize.ByteSize(carver.MinBlockTarget), bytesize.ByteSize(carver.MaxBlockTarget))
	}
	return nil
}

// formatVersion is a namespace's block format version to write (RFC 4 §3.5,
// RFC 13 §5.4).
type formatVersion struct {
	Version uint8 `json:"version" class:"next write"`
}

// Validate refuses a version the codec does not know.
func (f formatVersion) Validate() error {
	if _, ok := codec.MaxHeaderLen(f.Version); !ok {
		return fmt.Errorf("block format version %d is not one this binary's codec knows", f.Version)
	}
	return nil
}

// Sections is the remote tier's settings sections, for the node's table
// (RFC 13 Appendix B): the remote store's, and the namespace's prefix and
// block format version, each a section of one key of its own, since a key
// belongs to the section with the longest prefix.
func Sections() []settings.Section {
	return []settings.Section{
		settings.Of("remote", settings.At(entity.ScopeRemote),
			remoteSettings{
				Settings:    s3.Settings{Region: "us-east-1", StorageClass: "STANDARD"},
				BlockTarget: 4 * bytesize.MiB,
			},
			settings.Required("Endpoint", "Credential", "Bucket"),
			settings.SecretRef("Credential", entity.SecretRemoteCredential)),
		settings.Of("remote.prefix", settings.At(entity.ScopeNamespace), s3.NamespaceSettings{},
			settings.SelfKey(), settings.Required("Prefix")),
		// decision: the default is version 1, a constant, not the newest
		// version this binary reads: a default that followed the binary
		// would make one that learns version 2 write it at once on every
		// namespace that never set the key, and a new binary keeps writing
		// the version already written until the control plane raises it
		// (RFC 13 §5.5). With one version the two read the same. When a
		// second version lands, the default becomes the installation's
		// active block format version, read from its installation record.
		settings.Of("remote.block_format_version", settings.At(entity.ScopeNamespace),
			formatVersion{Version: 1}, settings.SelfKey(), settings.FormatVersion("Version")),
	}
}
