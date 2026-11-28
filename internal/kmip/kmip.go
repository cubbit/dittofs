// Package kmip is the KMIP 1.4 transport the key providers share: the TLS
// setup, one request and its response over a TLS connection, the framed
// message read, and the two reads both providers make of a key the server
// holds, its material and its state.
package kmip

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/gemalto/kmip-go"
	"github.com/gemalto/kmip-go/kmip14"
	"github.com/gemalto/kmip-go/ttlv"
)

// defaultTimeout bounds one round trip when the caller names no timeout.
const defaultTimeout = 5 * time.Second

// keySize is the length of an AES-256 key, the only symmetric key the
// providers wrap under.
const keySize = 32

// ErrKeyLength reports a symmetric key whose material is not 32 bytes, the
// one failure a caller treats as a configuration error rather than an
// unreachable key.
var ErrKeyLength = errors.New("kmip: key is not an AES-256 key")

// Client reaches one KMIP server. It holds no connection: each call dials
// its own.
type Client struct {
	endpoint string
	tls      *tls.Config
	timeout  time.Duration
}

// NewClient returns a client of the server at endpoint (host:port). A
// timeout of zero or less is the default of five seconds.
func NewClient(endpoint string, tlsCfg *tls.Config, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{endpoint: endpoint, tls: tlsCfg, timeout: timeout}
}

// Endpoint returns the host:port the client dials.
func (c *Client) Endpoint() string { return c.endpoint }

// TLSConfig builds the client's TLS configuration from PEM bytes: the client
// certificate and its key, and the CA the server's certificate must chain
// to. A nil CA trusts the system roots; a CA that holds no certificate is
// refused.
func TLSConfig(clientCertPEM, clientKeyPEM, serverCAPEM []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("kmip: load client cert: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if serverCAPEM != nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(serverCAPEM) {
			return nil, errors.New("kmip: server CA has no PEM certs")
		}
		tlsCfg.RootCAs = pool
	}
	return tlsCfg, nil
}

// Do opens a TLS connection to the server, sends a single-item batch
// carrying one operation under KMIP 1.4's protocol version, and returns
// that item's response payload. The connection is closed before return.
//
// ponytail: one TLS connection per call, so a cold start pays a handshake
// per unwrap. A pooled connection with a concurrency bound replaces it once
// a cold start or a master-key rotation shows the handshakes in a profile.
func (c *Client) Do(ctx context.Context, op kmip14.Operation, payload any) (ttlv.TTLV, error) {
	// The call ends at the earlier of the context's deadline and the timeout.
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	dialer := &tls.Dialer{NetDialer: &net.Dialer{}, Config: c.tls}
	conn, err := dialer.DialContext(ctx, "tcp", c.endpoint)
	if err != nil {
		return nil, fmt.Errorf("kmip: dial %s: %w", c.endpoint, err)
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("kmip: set deadline: %w", err)
	}

	req := kmip.RequestMessage{
		RequestHeader: kmip.RequestHeader{
			ProtocolVersion: kmip.ProtocolVersion{ProtocolVersionMajor: 1, ProtocolVersionMinor: 4},
			BatchCount:      1,
		},
		BatchItem: []kmip.RequestBatchItem{{
			Operation:      op,
			RequestPayload: payload,
		}},
	}
	reqTTLV, err := ttlv.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("kmip: marshal %v request: %w", op, err)
	}
	if _, err := conn.Write(reqTTLV); err != nil {
		return nil, fmt.Errorf("kmip: write %v request: %w", op, err)
	}
	resp, err := readMessage(conn)
	if err != nil {
		return nil, fmt.Errorf("kmip: read %v response: %w", op, err)
	}
	return decodeResponse(op, resp)
}

// decodeResponse returns the payload of the one batch item a response
// message carries, or the failure the item reports, by its status and
// reason names.
func decodeResponse(op kmip14.Operation, msg ttlv.TTLV) (ttlv.TTLV, error) {
	var resp kmip.ResponseMessage
	if err := unmarshal(msg, &resp); err != nil {
		return nil, fmt.Errorf("kmip: unmarshal %v response: %w", op, err)
	}
	if len(resp.BatchItem) == 0 {
		return nil, fmt.Errorf("kmip: %v response has no batch items", op)
	}
	item := resp.BatchItem[0]
	if item.ResultStatus != kmip14.ResultStatusSuccess {
		return nil, fmt.Errorf("kmip: %v failed: status=%v reason=%v message=%q",
			op, item.ResultStatus, item.ResultReason, item.ResultMessage)
	}
	raw, ok := item.ResponsePayload.(ttlv.TTLV)
	if !ok {
		return nil, fmt.Errorf("kmip: %v payload is not TTLV (got %T)", op, item.ResponsePayload)
	}
	return raw, nil
}

// GetSymmetricKey asks the server for the object stored under uid and
// returns its key material, refusing any key that is not 32 bytes with
// ErrKeyLength.
func (c *Client) GetSymmetricKey(ctx context.Context, uid string) ([]byte, error) {
	raw, err := c.Do(ctx, kmip14.OperationGet, kmip.GetRequestPayload{UniqueIdentifier: uid})
	if err != nil {
		return nil, fmt.Errorf("get key %s: %w", uid, err)
	}
	return decodeSymmetricKey(uid, raw)
}

// decodeSymmetricKey reads the key material out of a Get response payload.
func decodeSymmetricKey(uid string, raw ttlv.TTLV) ([]byte, error) {
	var payload kmip.GetResponsePayload
	if err := unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("kmip: decode Get payload of key %s: %w", uid, err)
	}
	if payload.ObjectType != kmip14.ObjectTypeSymmetricKey {
		return nil, fmt.Errorf("kmip: key %s is not a symmetric key (got object type %v)", uid, payload.ObjectType)
	}
	if payload.SymmetricKey == nil {
		return nil, fmt.Errorf("kmip: Get response of key %s missing SymmetricKey", uid)
	}
	key, err := extractSymmetricKeyMaterial(payload.SymmetricKey.KeyBlock.KeyValue)
	if err != nil {
		return nil, fmt.Errorf("kmip: key %s: %w", uid, err)
	}
	if len(key) != keySize {
		clear(key)
		return nil, fmt.Errorf("%w: key %q has unexpected length %d (want %d for AES-256)", ErrKeyLength, uid, len(key), keySize)
	}
	return key, nil
}

// extractSymmetricKeyMaterial copies the raw byte material out of a KMIP
// KeyValue. The library decodes ByteString KeyMaterial as []byte.
func extractSymmetricKeyMaterial(kv *kmip.KeyValue) ([]byte, error) {
	if kv == nil {
		return nil, errors.New("missing KeyValue in Get response")
	}
	switch m := kv.KeyMaterial.(type) {
	case []byte:
		out := make([]byte, len(m))
		copy(out, m)
		return out, nil
	case ttlv.TTLV:
		if m.Type() != ttlv.TypeByteString {
			return nil, fmt.Errorf("key material has unexpected type %s", m.Type())
		}
		out := make([]byte, len(m.ValueByteString()))
		copy(out, m.ValueByteString())
		return out, nil
	default:
		return nil, fmt.Errorf("key material has unexpected go type %T", m)
	}
}

// getAttributesRequestPayload and getAttributesResponsePayload carry the
// GetAttributes operation, which the KMIP library models no types for.
// GetAttributes is read-only: it reports on an object and can neither
// create nor modify one, so the client credential needs no write
// capability at the server.
type getAttributesRequestPayload struct {
	UniqueIdentifier string
	AttributeName    []string
}

type getAttributesResponsePayload struct {
	UniqueIdentifier string
	Attribute        []kmip.Attribute
}

// State reads the KMIP State attribute of the object stored under uid. The
// State is how a server says "stop using this", so a state that cannot be
// read is an error rather than an assumed Active.
func (c *Client) State(ctx context.Context, uid string) (kmip14.State, error) {
	raw, err := c.Do(ctx, kmip14.OperationGetAttributes,
		getAttributesRequestPayload{UniqueIdentifier: uid, AttributeName: []string{kmip14.TagState.CanonicalName()}})
	if err != nil {
		return 0, fmt.Errorf("state of key %s: %w", uid, err)
	}
	return decodeState(uid, raw)
}

// decodeState reads the State attribute out of a GetAttributes response
// payload.
func decodeState(uid string, raw ttlv.TTLV) (kmip14.State, error) {
	stateName := kmip14.TagState.CanonicalName()
	var payload getAttributesResponsePayload
	if err := unmarshal(raw, &payload); err != nil {
		return 0, fmt.Errorf("kmip: decode GetAttributes payload of key %s: %w", uid, err)
	}
	for _, attr := range payload.Attribute {
		if attr.AttributeName != stateName {
			continue
		}
		value, ok := attr.AttributeValue.(ttlv.EnumValue)
		if !ok {
			return 0, fmt.Errorf("kmip: key %s State attribute has unexpected go type %T", uid, attr.AttributeValue)
		}
		return kmip14.State(value), nil
	}
	return 0, fmt.Errorf("kmip: key %s returned no State attribute", uid)
}

// maxMessageSize bounds one inbound KMIP message. Responses are kilobytes
// (a Get of a 32-byte symmetric key fits in about 512 bytes); 16 MiB is far
// above any legitimate one yet keeps a misbehaving peer from forcing an
// unbounded allocation.
const maxMessageSize = 16 * 1024 * 1024

// headerLen is a TTLV header: a 3-byte tag, a 1-byte type and a 4-byte
// length.
const headerLen = 8

// readMessage reads exactly one TTLV-framed message off r. The value after
// the header is zero-padded to an 8-byte boundary (KMIP 1.4 §9.1.1.4), so
// the message is the header, the length it declares and the padding. A
// length above maxMessageSize is refused before the body is allocated.
func readMessage(r io.Reader) (ttlv.TTLV, error) {
	var header [headerLen]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	bodyLen := binary.BigEndian.Uint32(header[4:8])
	if int64(bodyLen) > maxMessageSize {
		return nil, fmt.Errorf("kmip: message length %d exceeds cap %d", bodyLen, maxMessageSize)
	}
	pad := (8 - int(bodyLen%8)) % 8
	msg := make([]byte, headerLen+int(bodyLen)+pad)
	copy(msg, header[:])
	if _, err := io.ReadFull(r, msg[headerLen:]); err != nil {
		return nil, err
	}
	return ttlv.TTLV(msg), nil
}

// unmarshal is ttlv.Unmarshal with its panics returned as errors: the
// library panics on some malformed input, such as a byte string where a
// structure belongs, and the bytes come from the server.
func unmarshal(raw ttlv.TTLV, v any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("malformed TTLV: %v", r)
		}
	}()
	return ttlv.Unmarshal(raw, v)
}
