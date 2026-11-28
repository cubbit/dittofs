package keyprovider

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/cubbit/dittofs/internal/keyfile"
)

// localPassphraseEnv names the environment variable holding the
// passphrase that unlocks the on-disk key file. Kept out of every
// configuration file by design — it must travel through the process
// environment only.
const localPassphraseEnv = "DITTOFS_ENCRYPTION_PASSPHRASE"

// localProvider unlocks a single master key from a passphrase-protected
// file and uses AES-256-GCM with a random nonce to wrap per-block keys.
//
// Nonce-reuse safety: with a fresh 96-bit nonce per Wrap call, the
// birthday bound for collision is ~2^48 calls per master key — many
// orders of magnitude beyond any realistic block volume.
type localProvider struct {
	aesGCMKEK
}

func newLocalProvider(cfg Config, passphrase string) (*localProvider, error) {
	if cfg.File == "" {
		return nil, fmt.Errorf("%w: local provider requires a file path", ErrInvalidConfig)
	}
	// ponytail: one passphrase for every role's key file; upgrade: a per-role variable in Config.
	if passphrase == "" {
		return nil, ErrPassphraseMissing
	}
	masterKeyID, masterKey, err := loadLocalKeyFile(cfg.File, passphrase)
	if err != nil {
		return nil, err
	}
	retired, err := loadRetiredKeys(masterKeyID, cfg.RetiredFiles, func(path string) (string, []byte, error) {
		return loadLocalKeyFile(path, passphrase)
	})
	if err != nil {
		zeroKey(masterKey)
		return nil, err
	}
	return &localProvider{aesGCMKEK: aesGCMKEK{
		masterKey:   masterKey,
		masterKeyID: masterKeyID,
		retired:     retired,
	}}, nil
}

// loadLocalKeyFile reads one passphrase-protected key file and returns
// the master key id it records alongside the unwrapped key material.
// Every key file carries its own id, so a retired file identifies itself
// without the operator restating it in config.
func loadLocalKeyFile(path, passphrase string) (string, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("keyprovider: read key file: %w", err)
	}
	id, key, err := keyfile.Open(raw, passphrase)
	return id, key, keyFileError(err)
}

// GenerateKeyFile produces the bytes of a fresh passphrase-protected key
// file. The caller is responsible for writing the bytes to disk with
// suitable permissions (mode 0600 is recommended). Exposed so the dfsctl
// CLI can offer an "auto-create on first use" affordance without
// reinventing the encoding.
func GenerateKeyFile(passphrase string) ([]byte, error) {
	raw, err := keyfile.Generate(passphrase, false)
	return raw, keyFileError(err)
}

// GenerateWeakKeyFile is GenerateKeyFile at the lowest Argon2 cost a key file
// may record, for tests that open many key files: it derives about 24 times
// faster and protects the master key as much less. Never write one where a
// key protects data.
func GenerateWeakKeyFile(passphrase string) ([]byte, error) {
	raw, err := keyfile.Generate(passphrase, true)
	return raw, keyFileError(err)
}

// keyFileError translates the key-file format's errors into this package's,
// rather than aliasing them:
// ErrUnwrapFailed is also the block-key unwrap's error, and
// ErrPassphraseMissing's message names the passphrase's variable.
func keyFileError(err error) error {
	for _, t := range [...]struct{ from, to error }{
		{keyfile.ErrCorrupt, ErrKeyFileCorrupt},
		{keyfile.ErrUnwrap, ErrUnwrapFailed},
		{keyfile.ErrPassphrase, ErrPassphraseMissing},
	} {
		if errors.Is(err, t.from) {
			// Only this package's sentinel matches; the detail after keyfile's
			// own keeps the message this package always wrote.
			if detail, ok := strings.CutPrefix(err.Error(), t.from.Error()); ok {
				return fmt.Errorf("%w%s", t.to, detail)
			}
			return fmt.Errorf("%w: %v", t.to, err)
		}
	}
	return err
}

// aesGCMKEK is the shared Wrap / Unwrap / Close implementation used by
// any provider that holds an in-memory 32-byte symmetric KEK. The
// wrapped layout is `nonce || ciphertext-with-tag` under AES-256-GCM.
//
// Wrap always uses masterKey and records masterKeyID. Unwrap resolves the
// id recorded in the frame against the current key plus retired, so a
// block wrapped under a previous master key stays readable after the
// current key changes. Retired keys are decrypt-only: nothing is ever
// wrapped under them again.
type aesGCMKEK struct {
	masterKey   []byte
	masterKeyID string

	// retired maps a master key id to decrypt-only key material. Written
	// once during construction and read-only thereafter, so it needs no
	// lock alongside the concurrent Wrap / Unwrap calls.
	retired map[string][]byte
}

func (k *aesGCMKEK) CurrentMasterKeyID() string { return k.masterKeyID }

// keyForID returns the master key that wrapped a frame recording
// masterKeyID. An empty id means the frame predates id recording and can
// only have come from the current key.
func (k *aesGCMKEK) keyForID(masterKeyID string) ([]byte, error) {
	if masterKeyID == "" || masterKeyID == k.masterKeyID {
		return k.masterKey, nil
	}
	if key, ok := k.retired[masterKeyID]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("%w: frame wrapped under %q, have current %q and %d retired",
		ErrWrongMasterKey, masterKeyID, k.masterKeyID, len(k.retired))
}

func (k *aesGCMKEK) Wrap(_ context.Context, blockKey []byte) ([]byte, string, error) {
	if len(blockKey) == 0 {
		return nil, "", fmt.Errorf("keyprovider: empty block key")
	}
	aead, err := newGCM(k.masterKey)
	if err != nil {
		return nil, "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", fmt.Errorf("keyprovider: read nonce: %w", err)
	}
	out := make([]byte, 0, len(nonce)+len(blockKey)+aead.Overhead())
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, blockKey, nil)
	return out, k.masterKeyID, nil
}

func (k *aesGCMKEK) Unwrap(_ context.Context, wrapped []byte, masterKeyID string) ([]byte, error) {
	masterKey, err := k.keyForID(masterKeyID)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(masterKey)
	if err != nil {
		return nil, err
	}
	ns := aead.NonceSize()
	if len(wrapped) < ns+aead.Overhead() {
		return nil, fmt.Errorf("%w: wrapped payload too short (%d bytes)", ErrUnwrapFailed, len(wrapped))
	}
	nonce, ciphertext := wrapped[:ns], wrapped[ns:]
	plain, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnwrapFailed, err)
	}
	return plain, nil
}

// Close zeros the master key bytes in memory, retired keys included.
// Best-effort — the Go runtime makes no guarantee the bytes are not
// retained on a GC heap.
func (k *aesGCMKEK) Close() error {
	zeroKey(k.masterKey)
	k.masterKey = nil
	zeroKeys(k.retired)
	return nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("keyprovider: GCM key must be 32 bytes (AES-256)")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("keyprovider: aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("keyprovider: cipher.NewGCM: %w", err)
	}
	return aead, nil
}
