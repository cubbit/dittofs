package provider

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/keyfile"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Row is one key record as the metadata store holds it: its kind, ID and
// state as RFC 5 Appendix B.2 names them ("data-key", "current", …), and the
// rest of the record in the provider's codec. It is an alias of an unnamed
// struct, so the metadata store and keystest satisfy KeyRecords without
// importing this package.
type Row = struct {
	Kind, ID, State string
	Record          []byte
}

// KeyRecords is what the provider needs of the metadata store: one
// namespace's key records and the master keys' wrap counts.
type KeyRecords interface {
	// Load returns every key record of ns.
	Load(ctx context.Context, ns entity.NamespaceID) ([]Row, error)
	// Update runs fn on ns's key records in one transaction that conflicts
	// with any other write of them, and writes the rows fn returns, each
	// replacing the record of its kind and ID. It deletes nothing. fn may run
	// more than once and must have no other effect.
	Update(ctx context.Context, ns entity.NamespaceID, fn func(rows []Row) ([]Row, error)) error
	// RaiseWrapCount adds n to masterKeyID's wrap count if the sum stays at
	// most limit, and reports whether it did.
	RaiseWrapCount(ctx context.Context, masterKeyID []byte, n, limit uint64) (bool, error)
}

// Config is a store configuration's provider settings.
type Config struct {
	KeyFile    string   // the master key's file
	Passphrase string   // the key file's passphrase (RFC 13 Appendix C)
	Escrowed   []Escrow // the passed escrow checks the store configuration records
}

// Escrow is a passed escrow check: what VerifyEscrow returns and the store
// configuration records.
type Escrow struct {
	MasterKeyID string // as the key file records it
	Fingerprint [32]byte
}

// StorageKey is one storage wrapping key, by its ID and fingerprint.
type StorageKey struct {
	ID          string
	Fingerprint [32]byte // zero when its bytes cannot be read: checked by ID alone
}

// Deps is what the provider is built over.
type Deps struct {
	Records     KeyRecords
	StorageKeys []StorageKey // every storage key the node holds, current and retired
	Logger      *slog.Logger
	Now         func() time.Time
	Rand        io.Reader // keys, nonces and IDs; crypto/rand.Reader in production
}

// Provider is the material provider of one store configuration. It is safe
// for concurrent use.
type Provider struct {
	records KeyRecords
	rand    io.Reader

	// masterKeyID names the key-file master key; aead holds the key itself,
	// which never leaves this process.
	masterKeyID [16]byte
	aead        cipher.AEAD
}

// fingerprintLabel is what a fingerprint is computed over (RFC 5 §2.5).
const fingerprintLabel = "dittofs key check v1"

// fingerprint is material's fingerprint, HMAC-SHA256(material, "dittofs key
// check v1") (RFC 5 §2.5).
func fingerprint(material []byte) (f [32]byte) {
	m := hmac.New(sha256.New, material)
	m.Write([]byte(fingerprintLabel))
	m.Sum(f[:0])
	return f
}

// parseMasterKeyID reads a key file's master key ID, a UUID's canonical text,
// as its 16 bytes.
func parseMasterKeyID(s string) ([16]byte, error) {
	u, err := uuid.Parse(s)
	if err != nil || u.String() != s {
		return [16]byte{}, fmt.Errorf("master key ID %q is not a UUID's text", s)
	}
	return u, nil
}

// ValidateConfig checks cfg's shape and reads nothing, so a setting is checked
// where its key file is absent.
func ValidateConfig(cfg Config) error {
	if cfg.KeyFile == "" {
		return errors.New("provider: no key file")
	}
	if cfg.Passphrase == "" {
		return errors.New("provider: no key-file passphrase")
	}
	for _, e := range cfg.Escrowed {
		if _, err := parseMasterKeyID(e.MasterKeyID); err != nil {
			return fmt.Errorf("provider: escrow pass: %w", err)
		}
	}
	return nil
}

// New builds the provider of cfg. It reads and unwraps the master key from
// cfg's key file, and fails if it cannot, so a store whose master key is
// unreadable does not open; it refuses a master key that is one of the
// storage keys in deps.
func New(ctx context.Context, cfg Config, deps Deps) (*Provider, error) {
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if deps.Records == nil || deps.Rand == nil {
		return nil, errors.New("provider: Deps needs Records and Rand")
	}

	raw, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("provider: read the key file: %w", err)
	}
	id, key, err := keyfile.Open(raw, cfg.Passphrase)
	if err != nil {
		return nil, fmt.Errorf("provider: open key file %s: %w", cfg.KeyFile, err)
	}
	defer clear(key)

	masterKeyID, err := parseMasterKeyID(id)
	if err != nil {
		return nil, fmt.Errorf("provider: key file %s: %w: %w", cfg.KeyFile, keyfile.ErrCorrupt, err)
	}
	fp := fingerprint(key)

	for _, sk := range deps.StorageKeys {
		// decision: a storage key whose fingerprint is zero, because its
		// bytes cannot be read here, is matched by its ID alone, so its bytes
		// in a key file under another ID are not caught. Withdraw it once
		// every storage key's fingerprint reaches the provider.
		if sk.ID == id || (sk.Fingerprint != [32]byte{} && sk.Fingerprint == fp) {
			return nil, fmt.Errorf("provider: master key %x is a storage wrapping key, which never wraps a namespace key", masterKeyID)
		}
	}

	return build(masterKeyID, key, deps)
}

// build is the provider of the master key key, named masterKeyID, over deps.
func build(masterKeyID [16]byte, key []byte, deps Deps) (*Provider, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("provider: master key %x: %w", masterKeyID, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("provider: master key %x: %w", masterKeyID, err)
	}

	return &Provider{records: deps.Records, rand: deps.Rand, masterKeyID: masterKeyID, aead: aead}, nil
}
