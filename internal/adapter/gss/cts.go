package gss

import (
	"crypto/hmac"
	"crypto/sha1"
	"fmt"
	"hash"
	"sync"

	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/crypto/common"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
)

// RFC 4121 §2's key usages for the per-message tokens.
const (
	usageAcceptorSign  uint32 = 23
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

// checksummer is HMAC-SHA1-96 under one key usage's Kc, derived once. Its
// HMACs are pooled, keyed once each, so callers in parallel share no state
// and the steady state keys nothing.
type checksummer struct{ pool sync.Pool }

func newChecksummer(key []byte, usage uint32) (*checksummer, error) {
	kc, err := deriveKey(key, common.GetUsageKc(usage))
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
