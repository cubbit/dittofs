// Package keyfile is the key file's format: a 32-byte master key and its ID,
// wrapped under AES-256-GCM with a key Argon2id derives from a passphrase,
// as JSON inside a PEM block. It encodes and decodes the file's bytes only;
// writing the file to disk is its callers'.
package keyfile

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"
)

var (
	// ErrCorrupt is a key file that does not decode, records an unsupported
	// version, KDF or wrap, or records KDF parameters outside the bounds.
	ErrCorrupt = errors.New("keyfile: key file corrupt")
	// ErrUnwrap is a key file whose master key does not unwrap under the
	// passphrase given: the wrong passphrase, or a tampered wrap.
	ErrUnwrap = errors.New("keyfile: unwrap failed")
	// ErrPassphrase is an empty passphrase, refused before any work.
	ErrPassphrase = errors.New("keyfile: empty passphrase")
)

const (
	pemType = "DITTOFS ENCRYPTED MASTER KEY"
	version = 1
	kdfAlgo = "argon2id"
	wrapAlg = "aes-256-gcm"
	keyLen  = 32
	saltLen = 16
	// nonceLen is AES-GCM's standard nonce size; its Open panics on any other.
	nonceLen = 12
	threads  = 4
)

// Argon2 parameter bounds a key file must record. The lower bounds keep
// argon2.IDKey from panicking on a corrupt or tampered file (it panics when
// time or threads is 0) and refuse a file weaker than was ever generated. The
// upper bounds cap the unwrap's cost, so whoever can write the key file cannot
// turn a start into an OOM kill or a minutes-long hang: about 4 GiB, and a
// time and a thread count far above any sane configuration. Generate's
// default, t=3, m=64 MiB, p=4, sits well inside them.
const (
	minTime      = 1
	maxTime      = 64
	minMemoryKiB = 8 * 1024
	maxMemoryKiB = 4 * 1024 * 1024
	minThreads   = 1
	maxThreads   = 64
)

// kdf holds the KDF parameters recorded beside the wrapped master key. The
// default follows the OWASP 2024 password-storage cheat sheet for Argon2id.
type kdf struct {
	Algo      string `json:"algo"`
	Salt      string `json:"salt"`
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memory_kib"`
	Threads   uint8  `json:"threads"`
}

// file is the JSON body inside the PEM block.
type file struct {
	Version          int    `json:"version"`
	MasterKeyID      string `json:"master_key_id"`
	KDF              kdf    `json:"kdf"`
	Wrap             string `json:"wrap"`
	Nonce            string `json:"nonce"`
	WrappedMasterKey string `json:"wrapped_master_key"`
}

// Generate returns the bytes of a new key file: a fresh master key under a
// fresh random ID, wrapped under passphrase. weak records the lowest cost a
// key file may, about 24 times faster to open and as much weaker, for tests
// that open many files; never write a weak file where a key protects data.
func Generate(passphrase string, weak bool) ([]byte, error) {
	if passphrase == "" {
		return nil, ErrPassphrase
	}
	t, m := uint32(3), uint32(64*1024)
	if weak {
		t, m = minTime, minMemoryKiB
	}
	masterKey := make([]byte, keyLen)
	if _, err := rand.Read(masterKey); err != nil {
		return nil, fmt.Errorf("keyfile: read master key: %w", err)
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("keyfile: read salt: %w", err)
	}
	aead, err := newGCM(argon2.IDKey([]byte(passphrase), salt, t, m, threads, keyLen))
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("keyfile: read wrap nonce: %w", err)
	}
	kf := file{
		Version:     version,
		MasterKeyID: uuid.NewString(),
		KDF: kdf{
			Algo:      kdfAlgo,
			Salt:      base64.StdEncoding.EncodeToString(salt),
			Time:      t,
			MemoryKiB: m,
			Threads:   threads,
		},
		Wrap:             wrapAlg,
		Nonce:            base64.StdEncoding.EncodeToString(nonce),
		WrappedMasterKey: base64.StdEncoding.EncodeToString(aead.Seal(nil, nonce, masterKey, nil)),
	}
	body, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("keyfile: marshal key file: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: body}), nil
}

// Open decodes raw, a key file, and unwraps its master key under passphrase.
// It returns the ID the file records and the 32-byte key. A file that does not
// decode, or whose parameters are out of bounds, is ErrCorrupt, checked before
// Argon2 runs; a wrap that does not open is ErrUnwrap.
func Open(raw []byte, passphrase string) (id string, key []byte, err error) {
	if passphrase == "" {
		return "", nil, ErrPassphrase
	}
	kf, err := decode(raw)
	if err != nil {
		return "", nil, err
	}
	salt, err := base64.StdEncoding.DecodeString(kf.KDF.Salt)
	if err != nil {
		return "", nil, fmt.Errorf("%w: salt: %v", ErrCorrupt, err)
	}
	nonce, err := base64.StdEncoding.DecodeString(kf.Nonce)
	if err != nil {
		return "", nil, fmt.Errorf("%w: nonce: %v", ErrCorrupt, err)
	}
	wrapped, err := base64.StdEncoding.DecodeString(kf.WrappedMasterKey)
	if err != nil {
		return "", nil, fmt.Errorf("%w: wrapped_master_key: %v", ErrCorrupt, err)
	}
	if len(nonce) != nonceLen {
		return "", nil, fmt.Errorf("%w: nonce has %d bytes, want %d", ErrCorrupt, len(nonce), nonceLen)
	}
	aead, err := newGCM(argon2.IDKey([]byte(passphrase), salt, kf.KDF.Time, kf.KDF.MemoryKiB, kf.KDF.Threads, keyLen))
	if err != nil {
		return "", nil, err
	}
	plain, err := aead.Open(nil, nonce, wrapped, nil)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrUnwrap, err)
	}
	if len(plain) != keyLen {
		return "", nil, fmt.Errorf("%w: unwrapped master key has %d bytes, want %d", ErrCorrupt, len(plain), keyLen)
	}
	return kf.MasterKeyID, plain, nil
}

func decode(raw []byte) (*file, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != pemType {
		return nil, fmt.Errorf("%w: missing %q PEM block", ErrCorrupt, pemType)
	}
	var kf file
	if err := json.Unmarshal(block.Bytes, &kf); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if kf.Version != version {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrCorrupt, kf.Version)
	}
	if kf.KDF.Algo != kdfAlgo {
		return nil, fmt.Errorf("%w: unsupported kdf %q", ErrCorrupt, kf.KDF.Algo)
	}
	if kf.Wrap != wrapAlg {
		return nil, fmt.Errorf("%w: unsupported wrap %q", ErrCorrupt, kf.Wrap)
	}
	if kf.MasterKeyID == "" {
		return nil, fmt.Errorf("%w: empty master_key_id", ErrCorrupt)
	}
	p := kf.KDF
	if p.Time < minTime || p.Time > maxTime {
		return nil, fmt.Errorf("%w: kdf time %d out of range [%d,%d]", ErrCorrupt, p.Time, minTime, maxTime)
	}
	if p.MemoryKiB < minMemoryKiB || p.MemoryKiB > maxMemoryKiB {
		return nil, fmt.Errorf("%w: kdf memory_kib %d out of range [%d,%d]", ErrCorrupt, p.MemoryKiB, minMemoryKiB, maxMemoryKiB)
	}
	if p.Threads < minThreads || p.Threads > maxThreads {
		return nil, fmt.Errorf("%w: kdf threads %d out of range [%d,%d]", ErrCorrupt, p.Threads, minThreads, maxThreads)
	}
	return &kf, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("keyfile: aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("keyfile: cipher.NewGCM: %w", err)
	}
	return aead, nil
}
