package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/cubbit/dittofs/pkg/transform/keys"
)

const (
	// kindKeyFile and kindKeyService are a stored wrap's provider kinds
	// (RFC 5 Appendix B.1).
	kindKeyFile    byte = 0x01
	kindKeyService byte = 0x02

	// wrapLimit is the most wraps one master key makes (RFC 5 Appendix B.1).
	wrapLimit = 1 << 32

	nonceLen = 12
	tagLen   = 16
)

// errStoredWrap is bytes that do not have the stored wrap's layout.
var errStoredWrap = errors.New("provider: stored wrap malformed")

// lenPrefixed appends field to b after its length, 4 bytes big-endian.
func lenPrefixed(b, field []byte) []byte {
	return append(binary.BigEndian.AppendUint32(b, uint32(len(field))), field...)
}

// wrapAAD is a key wrap's associated data: namespace ID ‖ kind ‖ key ID, each
// with a 4-byte length (RFC 5 Appendix B.1). ns is empty for a wrap that
// belongs to no namespace.
func wrapAAD(ns []byte, kind, id string) []byte {
	b := lenPrefixed(make([]byte, 0, 12+len(ns)+len(kind)+len(id)), ns)
	b = lenPrefixed(b, []byte(kind))
	return lenPrefixed(b, []byte(id))
}

// storedWrap is a wrapped key in the one layout every provider kind shares
// (RFC 5 Appendix B.1): provider kind (1 byte) ‖ master-key ID length (2
// bytes, big-endian) ‖ master-key ID ‖ wrap length (4 bytes, big-endian) ‖
// wrap bytes.
type storedWrap struct {
	kind        byte
	masterKeyID []byte
	wrap        []byte
}

func (s storedWrap) encode() []byte {
	b := make([]byte, 0, 7+len(s.masterKeyID)+len(s.wrap))
	b = append(b, s.kind)
	b = binary.BigEndian.AppendUint16(b, uint16(len(s.masterKeyID)))
	b = append(b, s.masterKeyID...)
	return lenPrefixed(b, s.wrap)
}

// parseStoredWrap reads b as a stored wrap, refusing an unknown provider kind,
// a length that runs past b, and bytes after the wrap. The fields returned
// share b's bytes.
func parseStoredWrap(b []byte) (storedWrap, error) {
	if len(b) < 3 {
		return storedWrap{}, fmt.Errorf("%w: %d bytes, too short for its header", errStoredWrap, len(b))
	}

	s := storedWrap{kind: b[0]}
	if s.kind != kindKeyFile && s.kind != kindKeyService {
		return storedWrap{}, fmt.Errorf("%w: provider kind %#x", errStoredWrap, s.kind)
	}

	idLen := int(binary.BigEndian.Uint16(b[1:3]))
	rest := b[3:]
	if len(rest) < idLen+4 {
		return storedWrap{}, fmt.Errorf("%w: master-key ID of %d bytes runs past its %d", errStoredWrap, idLen, len(rest))
	}
	s.masterKeyID = rest[:idLen:idLen]
	rest = rest[idLen:]

	wrapLen := uint64(binary.BigEndian.Uint32(rest[:4]))
	rest = rest[4:]
	if uint64(len(rest)) != wrapLen {
		return storedWrap{}, fmt.Errorf("%w: wrap of %d bytes, %d follow", errStoredWrap, wrapLen, len(rest))
	}
	s.wrap = rest[:wrapLen:wrapLen]

	return s, nil
}

// sealIn is one key to wrap and the associated data that binds it.
type sealIn struct{ aad, key []byte }

// wrap wraps each key of in under the key-file master key, returning each
// one's stored wrap in order. It first raises the master key's wrap count by
// len(in), so a wrap the count refuses draws no nonce and seals nothing.
func (p *Provider) wrap(ctx context.Context, in ...sealIn) ([][]byte, error) {
	// decision: the count is raised before any nonce is drawn and committed
	// whether or not the wraps that follow are used, so a failure or a crash
	// between the raise and the wrap overcounts by design: RFC 5 Appendix B.1
	// allows a crash to overcount, never to undercount. Withdraw it only if
	// the count and the record a wrap goes into ever commit together.
	ok, err := p.records.RaiseWrapCount(ctx, p.masterKeyID[:], uint64(len(in)), wrapLimit)
	if err != nil {
		return nil, fmt.Errorf("provider: raise the wrap count of master key %x: %w", p.masterKeyID, err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: master key %x has no room for %d more of its 2³² wraps; rotate it", keys.ErrWrapLimit, p.masterKeyID, len(in))
	}

	out := make([][]byte, 0, len(in))
	for _, w := range in {
		nonce := make([]byte, nonceLen, nonceLen+len(w.key)+tagLen)
		if _, err := io.ReadFull(p.rand, nonce); err != nil {
			return nil, fmt.Errorf("provider: draw a wrap nonce: %w", err)
		}
		sealed := p.aead.Seal(nonce, nonce, w.key, w.aad)
		out = append(out, storedWrap{kind: kindKeyFile, masterKeyID: p.masterKeyID[:], wrap: sealed}.encode())
	}

	return out, nil
}

// unwrap opens stored, a key-file stored wrap under the provider's master
// key, with associated data aad: a wrap of another provider kind, under
// another master key, or bound to other associated data is refused.
func (p *Provider) unwrap(stored, aad []byte) ([]byte, error) {
	s, err := parseStoredWrap(stored)
	if err != nil {
		return nil, err
	}
	if s.kind != kindKeyFile {
		return nil, fmt.Errorf("provider: stored wrap of provider kind %#x, not the key file's", s.kind)
	}
	if !bytes.Equal(s.masterKeyID, p.masterKeyID[:]) {
		return nil, fmt.Errorf("provider: stored wrap under master key %x, not %x", s.masterKeyID, p.masterKeyID)
	}
	if len(s.wrap) < nonceLen+tagLen {
		return nil, fmt.Errorf("%w: key-file wrap of %d bytes, shorter than its nonce and tag", errStoredWrap, len(s.wrap))
	}

	key, err := p.aead.Open(nil, s.wrap[:nonceLen], s.wrap[nonceLen:], aad)
	if err != nil {
		return nil, fmt.Errorf("provider: unwrap under master key %x: %w", p.masterKeyID, err)
	}

	return key, nil
}
