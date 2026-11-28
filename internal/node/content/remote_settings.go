package content

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/cubbit/dittofs/internal/bytesize"
	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/remote"
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
	if n := r.BlockTarget.Uint64(); n < carver.MinBlockTarget || n > carver.MaxBlockTarget {
		return fmt.Errorf("block_target: %d bytes is outside %d to %d bytes", n, carver.MinBlockTarget, carver.MaxBlockTarget)
	}
	return nil
}

// formatVersion is a namespace's block format version to write (RFC 4 §3.5,
// RFC 13 §5.4).
type formatVersion struct {
	Version uint8 `json:"version" class:"next write"`
}

// Validate refuses a version the codec does not know. It runs before the
// format version gate, so a version past the node's range is refused here
// first, as ErrInvalid.
func (f formatVersion) Validate() error {
	if _, ok := codec.MaxHeaderLen(f.Version); !ok {
		return fmt.Errorf("block format version %d is not one this binary's codec knows", f.Version)
	}
	return nil
}

// remoteDefault is the remote store section's defaults (RFC 13 Appendix B).
var remoteDefault = remoteSettings{
	Settings:    s3.Settings{Region: "us-east-1", StorageClass: "STANDARD"},
	BlockTarget: 4 * bytesize.MiB,
}

// Sections is the remote tier's settings sections, for the node's table
// (RFC 13 Appendix B): the remote store's, and the namespace's prefix and
// block format version, each a section of one key of its own, since a key
// belongs to the section with the longest prefix.
func Sections() []settings.Section {
	return []settings.Section{
		settings.Of("remote", settings.At(entity.ScopeRemote), remoteDefault,
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

// credentialFields are a remote credential's two names, in the order
// ParseCredential returns their values.
var credentialFields = [2]string{"access_key_id", "secret_access_key"}

// ParseCredential reads a remote store credential's value (RFC 13 §7): one
// JSON object with exactly the two names access_key_id and
// secret_access_key, matched case-sensitively, each once and a non-empty
// string, and nothing but whitespace after it. Its error names the missing,
// empty or repeated field, or says the value holds another field or is not
// such an object, and never holds the value, a field name outside the two or
// the decoder's text, any of which could carry the key or the secret.
func ParseCredential(value []byte) (accessKeyID, secretAccessKey string, err error) {
	notObject := errors.New("remote credential: is not a JSON object of access_key_id and secret_access_key")
	dec := json.NewDecoder(bytes.NewReader(value))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return "", "", notObject
	}

	var got [2]string
	var seen [2]bool
	for dec.More() {
		t, err := dec.Token()
		name, ok := t.(string)
		if err != nil || !ok {
			return "", "", notObject
		}

		i := 0
		for i < len(credentialFields) && credentialFields[i] != name {
			i++
		}
		if i == len(credentialFields) {
			return "", "", errors.New("remote credential: holds a field other than access_key_id and secret_access_key")
		}
		if seen[i] {
			return "", "", fmt.Errorf("remote credential: %s is given twice", name)
		}

		t, err = dec.Token()
		v, ok := t.(string)
		switch {
		case err != nil:
			return "", "", notObject
		case !ok:
			return "", "", fmt.Errorf("remote credential: %s is not a string", name)
		case v == "":
			return "", "", fmt.Errorf("remote credential: %s is empty", name)
		}
		got[i], seen[i] = v, true
	}

	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return "", "", notObject
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", "", errors.New("remote credential: holds something after the object")
	}
	for i, ok := range seen {
		if !ok {
			return "", "", fmt.Errorf("remote credential: %s is missing", credentialFields[i])
		}
	}

	return got[0], got[1], nil
}

// clientConfig is a store configuration's client configuration (RFC 4 C.3)
// from its settings and its credential's unsealed value: the settings judged
// again, as at start, and the credential parsed again, as at its seal, each
// refusal wrapping remote.ErrInvalid. limit is the client's connection limit
// and maxBlock the largest encoded block the store is put, both judged by
// s3.NewClient.
func clientConfig(s remoteSettings, credential []byte, limit int, maxBlock int64) (s3.ClientConfig, error) {
	if err := s.Validate(); err != nil {
		return s3.ClientConfig{}, fmt.Errorf("remote: %w: %w", err, remote.ErrInvalid)
	}

	key, secret, err := ParseCredential(credential)
	if err != nil {
		return s3.ClientConfig{}, fmt.Errorf("%w: %w", err, remote.ErrInvalid)
	}

	// Validate has refused any block of the bundle that is not a certificate,
	// which AppendCertsFromPEM would drop silently.
	var roots *x509.CertPool
	if s.TrustRoots != "" {
		roots = x509.NewCertPool()
		roots.AppendCertsFromPEM([]byte(s.TrustRoots))
	}

	return s3.ClientConfig{
		Endpoint:        s.Endpoint,
		Region:          s.Region,
		Bucket:          s.Bucket,
		StorageClass:    s.StorageClass,
		AccessKeyID:     key,
		SecretAccessKey: secret,
		TrustRoots:      roots,
		Limit:           limit,
		MaxBlockSize:    maxBlock,
	}, nil
}
