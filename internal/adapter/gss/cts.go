package gss

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"fmt"
	"hash"
	"sync"

	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/crypto/common"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
)

// RFC 4121 §2's key usages for the per-message tokens.
const (
	usageAcceptorSeal  uint32 = 22
	usageAcceptorSign  uint32 = 23
	usageInitiatorSeal uint32 = 24
	usageInitiatorSign uint32 = 25
)

// hmacSHA1Size is the checksum's length in an aes*-cts-hmac-sha1-96 token,
// the HMAC-SHA1 truncated to 96 bits (RFC 3962 §6).
const hmacSHA1Size = 12

// etypeOf is the enctype of a context key, named by its length: the two
// types the per-message tokens are built for, as the mechanism admits them.
func etypeOf(key []byte) (int32, error) {
	switch len(key) {
	case 16:
		return etypeID.AES128_CTS_HMAC_SHA1_96, nil
	case 32:
		return etypeID.AES256_CTS_HMAC_SHA1_96, nil
	}
	return 0, fmt.Errorf("gss: a context key of %d bytes", len(key))
}

// deriveKey is RFC 3961 §5.1's DK(key, constant) for the context key's type.
func deriveKey(key, constant []byte) ([]byte, error) {
	id, err := etypeOf(key)
	if err != nil {
		return nil, err
	}
	et, err := crypto.GetEtype(id)
	if err != nil {
		return nil, err
	}
	return et.DeriveKey(key, constant)
}

// checksummer is HMAC-SHA1-96 under a key derived once from the context key
// with constant: a key usage's Kc for a MIC token, its Ki for a Wrap token.
// Its HMACs are pooled, keyed once each, so callers in parallel share no
// state and the steady state keys nothing.
type checksummer struct{ pool sync.Pool }

func newChecksummer(key, constant []byte) (*checksummer, error) {
	kc, err := deriveKey(key, constant)
	if err != nil {
		return nil, err
	}
	c := &checksummer{}
	c.pool.New = func() any { return hmac.New(sha1.New, kc) }
	return c, nil
}

// appendSum appends the checksum of msg followed by hdr to dst, reading both
// where they lie. It allocates no copy of msg.
func (c *checksummer) appendSum(dst, msg, hdr []byte) []byte {
	h := c.pool.Get().(hash.Hash)
	h.Reset()
	h.Write(msg)
	h.Write(hdr)
	n := len(dst)
	dst = h.Sum(dst)[:n+hmacSHA1Size]
	c.pool.Put(h)
	return dst
}

// sealer is RFC 3962's aes*-cts-hmac-sha1-96 encryption under one key usage:
// AES-CTS under its Ke and HMAC-SHA1-96 under its Ki, both derived once.
type sealer struct {
	*cts
	sum *checksummer
}

func newSealer(key []byte, usage uint32) (*sealer, error) {
	ke, err := deriveKey(key, common.GetUsageKe(usage))
	if err != nil {
		return nil, err
	}
	c, err := newCTS(ke)
	if err != nil {
		return nil, err
	}
	sum, err := newChecksummer(key, common.GetUsageKi(usage))
	if err != nil {
		return nil, err
	}
	return &sealer{cts: c, sum: sum}, nil
}

// cts is AES-CTS under one key. Its CBC modes are pooled as the HMACs are,
// since each holds a copy of the expanded key, too large to make per call.
type cts struct {
	block    cipher.Block
	enc, dec sync.Pool
}

// cbcMode is the CBC mode the standard library returns, whose IV is reset
// between uses.
type cbcMode interface {
	cipher.BlockMode
	SetIV([]byte)
}

func newCTS(key []byte) (*cts, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	var zero [aes.BlockSize]byte
	c := &cts{block: block}
	c.enc.New = func() any { return cipher.NewCBCEncrypter(block, zero[:]).(cbcMode) }
	c.dec.New = func() any { return cipher.NewCBCDecrypter(block, zero[:]).(cbcMode) }
	return c, nil
}

// cbc runs one of pool's CBC modes from a zero IV over b in place.
func cbc(pool *sync.Pool, b []byte) {
	var zero [aes.BlockSize]byte
	m := pool.Get().(cbcMode)
	m.SetIV(zero[:])
	m.CryptBlocks(b, b)
	pool.Put(m)
}

// ponytail: AES-CTS is written here, CBC over the message where it lies and
// the stolen block in a 32-byte scratch, because neither gokrb5 nor the
// standard library encrypts in place: gokrb5's aescts copies the plaintext.
// Replace it when either gains an in-place AES-CTS.

// encrypt encrypts p in place with RFC 3962's AES-CTS under a zero IV:
// CBC, then the last two blocks swapped and the final one cut to the
// plaintext's length. p is at least one block.
func (c *cts) encrypt(p []byte) {
	b := c.block
	if len(p) == aes.BlockSize {
		b.Encrypt(p, p)
		return
	}
	tail, r := ctsTail(len(p))
	var iv [aes.BlockSize]byte
	if tail > 0 {
		cbc(&c.enc, p[:tail])
		copy(iv[:], p[tail-aes.BlockSize:tail])
	}
	var s [2 * aes.BlockSize]byte // the last two blocks, the final one zero-padded
	copy(s[:], p[tail:])
	subtle.XORBytes(s[:aes.BlockSize], s[:aes.BlockSize], iv[:])
	b.Encrypt(s[:aes.BlockSize], s[:aes.BlockSize])
	subtle.XORBytes(s[aes.BlockSize:], s[aes.BlockSize:], s[:aes.BlockSize])
	b.Encrypt(s[aes.BlockSize:], s[aes.BlockSize:])
	copy(p[tail:], s[aes.BlockSize:])
	copy(p[tail+aes.BlockSize:], s[:r])
}

// decrypt is encrypt's inverse, in place over p.
func (c *cts) decrypt(p []byte) {
	b := c.block
	if len(p) == aes.BlockSize {
		b.Decrypt(p, p)
		return
	}
	tail, r := ctsTail(len(p))
	var iv [aes.BlockSize]byte
	if tail > 0 {
		copy(iv[:], p[tail-aes.BlockSize:tail])
		cbc(&c.dec, p[:tail])
	}
	// s[16:] is the final block decrypted, the padded last plaintext XOR the
	// next-to-last ciphertext block, whose last 16-r bytes it therefore holds
	// whole; s[:16] rebuilds that block from them and its r stolen bytes.
	var s [2 * aes.BlockSize]byte
	b.Decrypt(s[aes.BlockSize:], p[tail:tail+aes.BlockSize])
	copy(s[:r], p[tail+aes.BlockSize:])
	copy(s[r:aes.BlockSize], s[aes.BlockSize+r:])
	subtle.XORBytes(s[aes.BlockSize:aes.BlockSize+r], s[aes.BlockSize:aes.BlockSize+r], s[:r])
	b.Decrypt(s[:aes.BlockSize], s[:aes.BlockSize])
	subtle.XORBytes(s[:aes.BlockSize], s[:aes.BlockSize], iv[:])
	copy(p[tail:], s[:aes.BlockSize])
	copy(p[tail+aes.BlockSize:], s[aes.BlockSize:aes.BlockSize+r])
}

// ctsTail splits a message of n bytes, more than one block, into the CBC
// prefix's length and the final block's length r, 1 to 16.
func ctsTail(n int) (tail, r int) {
	r = (n-1)%aes.BlockSize + 1
	return n - r - aes.BlockSize, r
}
