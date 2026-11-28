package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gemalto/kmip-go/kmip14"
	"github.com/gemalto/kmip-go/ttlv"

	"github.com/cubbit/dittofs/internal/kmip"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// The KMIP profile of the key-service custody (RFC 5 Appendix B.1 asks each
// key service's profile to name these):
//
//   - the master key is configured by its KMIP Name; its primary version is
//     the one Active symmetric key a Locate of that Name finds, and its
//     version-qualified ID is <server name>/<UID>;
//   - a namespace key is wrapped by Encrypt under that UID with AES-GCM, a
//     16-byte tag and an IV the server draws, the key wrap AAD sent as the
//     Authenticated Encryption Additional Data bytes;
//   - the wrap bytes join the Encrypt response's IV, tag and data
//     (encodeKMIPWrap), and Decrypt gets each back unchanged.

// KMIPConfig names the key service that holds a store configuration's master
// key and how the provider reaches it.
type KMIPConfig struct {
	Endpoint   string        // host:port
	ServerName string        // qualifies the master-key ID; empty: Endpoint
	Key        string        // the KMIP Name of the master key
	ClientCert string        // path to the client certificate's PEM
	ClientKey  []byte        // the client key's PEM
	ServerCA   string        // path to the server CA's PEM; empty: the system roots
	Timeout    time.Duration // one call's bound; 0: internal/kmip's default
}

// validateKMIP checks cfg's shape, reading no file and dialling nothing.
func validateKMIP(cfg KMIPConfig) error {
	if host, port, err := net.SplitHostPort(cfg.Endpoint); err != nil || host == "" || port == "" {
		return fmt.Errorf("provider: KMIP.Endpoint %q is not host:port", cfg.Endpoint)
	}
	if cfg.Key == "" {
		return errors.New("provider: KMIP.Key is empty: name the master key")
	}
	if cfg.ClientCert == "" {
		return errors.New("provider: KMIP.ClientCert is empty")
	}
	key := bytes.TrimSpace(cfg.ClientKey)
	if block, rest := pem.Decode(key); block == nil || !bytes.HasPrefix(key, []byte("-----BEGIN")) || len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("provider: KMIP.ClientKey is not one PEM block")
	}
	// The qualifier of every master-key ID, so it must fit one.
	field, qualifier := "KMIP.ServerName", cfg.ServerName
	if qualifier == "" {
		field, qualifier = "KMIP.Endpoint", cfg.Endpoint
	}
	if !printable(qualifier) || len(qualifier) > 0xFFFF-2 {
		return fmt.Errorf("provider: %s %q is not printable ASCII of at most 65,533 bytes", field, qualifier)
	}
	if cfg.Timeout < 0 {
		return fmt.Errorf("provider: KMIP.Timeout %v is negative", cfg.Timeout)
	}
	return nil
}

// printable reports whether s is printable ASCII.
func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// keyService is the key-service custody: a KMIP server holds the master key,
// and wraps and unwraps each namespace key for the provider. The key's bytes
// never reach this process.
type keyService struct {
	key     string       // the master key's KMIP Name, fixed for the custody's life
	storage []StorageKey // the storage wrapping keys, which never wrap a namespace key
	conn    atomic.Pointer[serviceConn]
}

// serviceConn is how the custody reaches its server: the client, and the
// qualifier of every master-key ID it reports.
type serviceConn struct {
	client    *kmip.Client
	qualifier string
}

// newKeyService is the custody of cfg, refusing the storage keys in storage.
// It loads the client's certificates and dials nothing.
func newKeyService(cfg KMIPConfig, storage []StorageKey) (*keyService, error) {
	if err := validateKMIP(cfg); err != nil {
		return nil, err
	}
	conn, err := connOf(cfg)
	if err != nil {
		return nil, err
	}
	ks := &keyService{key: cfg.Key, storage: slices.Clone(storage)}
	ks.conn.Store(conn)
	return ks, nil
}

// connOf builds the client of cfg, reading its certificate and CA files.
func connOf(cfg KMIPConfig) (*serviceConn, error) {
	cert, err := os.ReadFile(cfg.ClientCert)
	if err != nil {
		return nil, fmt.Errorf("provider: read KMIP.ClientCert: %w", err)
	}
	var ca []byte
	if cfg.ServerCA != "" {
		if ca, err = os.ReadFile(cfg.ServerCA); err != nil {
			return nil, fmt.Errorf("provider: read KMIP.ServerCA: %w", err)
		}
	}
	tlsCfg, err := kmip.TLSConfig(cert, cfg.ClientKey, ca)
	if err != nil {
		return nil, fmt.Errorf("provider: KMIP client of %s: %w", cfg.Endpoint, err)
	}
	qualifier := cfg.ServerName
	if qualifier == "" {
		qualifier = cfg.Endpoint
	}
	return &serviceConn{client: kmip.NewClient(cfg.Endpoint, tlsCfg, cfg.Timeout), qualifier: qualifier}, nil
}

// reconfigure takes a new way to reach the server: endpoint, server name,
// certificates and timeout. It refuses a changed Key, and a configuration
// that does not validate or whose files do not load, leaving the running
// client in force.
func (ks *keyService) reconfigure(cfg KMIPConfig) error {
	if err := validateKMIP(cfg); err != nil {
		return err
	}
	if cfg.Key != ks.key {
		return fmt.Errorf("provider: KMIP.Key %q is not %q, the master key the key records are wrapped under; a rotation changes master key", cfg.Key, ks.key)
	}
	conn, err := connOf(cfg)
	if err != nil {
		return err
	}
	// decision: the client is swapped whole once built, so a call in flight
	// finishes on the old one. A changed qualifier is a rotation the caller
	// re-wraps, never a loss, since an unwrap reads only the UID. Withdraw it
	// if a swap must ever drain the calls in flight.
	ks.conn.Store(conn)
	return nil
}

func (*keyService) kind() byte { return kindKeyService }

// unavailable is a failed call of the custody to the server conn reaches, about
// the master key what names: ErrMaterialUnavailable with the endpoint and the
// cause, whose text holds the KMIP result codes.
func unavailable(conn *serviceConn, what string, err error) error {
	return fmt.Errorf("provider: %s at %s: %w: %w", what, conn.client.Endpoint(), keys.ErrMaterialUnavailable, err)
}

// primary reads the master key's primary version as its master-key ID.
func (ks *keyService) primary(ctx context.Context) ([]byte, error) {
	_, id, err := ks.primaryVersion(ctx, ks.conn.Load())
	return id, err
}

// primaryVersion is the UID of the master key's primary version on conn's
// server, which is not a storage key's, and its master-key ID.
func (ks *keyService) primaryVersion(ctx context.Context, conn *serviceConn) (string, []byte, error) {
	uid, err := ks.primaryUID(ctx, conn)
	if err != nil {
		return "", nil, err
	}
	id, err := masterKeyIDOf(conn.qualifier, uid)
	if err != nil {
		return "", nil, unavailable(conn, fmt.Sprintf("KMIP key %q", ks.key), err)
	}
	return uid, id, nil
}

// primaryUID is the UID of the master key's primary version, which is not a
// storage key's.
func (ks *keyService) primaryUID(ctx context.Context, conn *serviceConn) (string, error) {
	what := fmt.Sprintf("KMIP key %q", ks.key)
	raw, err := conn.client.Do(ctx, kmip14.OperationLocate, locateRequest{Attribute: []attribute{
		{AttributeName: kmip14.TagObjectType.CanonicalName(), AttributeValue: kmip14.ObjectTypeSymmetricKey},
		{AttributeName: kmip14.TagState.CanonicalName(), AttributeValue: kmip14.StateActive},
		{AttributeName: kmip14.TagName.CanonicalName(), AttributeValue: kmipName{NameValue: ks.key, NameType: kmip14.NameTypeUninterpretedTextString}},
	}})
	if err != nil {
		return "", unavailable(conn, what, err)
	}
	var resp locateResponse
	if err := decodeTTLV(raw, &resp); err != nil {
		return "", unavailable(conn, what, fmt.Errorf("decode the Locate response: %w", err))
	}
	uid, err := locatedOne(ks.key, resp.UniqueIdentifier)
	if err != nil {
		return "", unavailable(conn, what, err)
	}
	if err := notStorage(uid, ks.storage); err != nil {
		return "", unavailable(conn, what, err)
	}
	return uid, nil
}

// locatedOne is the primary version among the UIDs a Locate of the Active
// symmetric keys named name returned.
//
// decision: exactly one UID is the primary version. None means the key is
// missing or not Active, and two or more mean the Name is ambiguous: each
// fails the read, naming the key and the count, and a wrap that needs it
// wraps nothing, so no record is written under a version picked by chance.
// Withdraw it if a profile ever states which of several is the primary.
func locatedOne(name string, uids []string) (string, error) {
	if len(uids) != 1 {
		return "", fmt.Errorf("a Locate of the Active symmetric key named %q found %d keys, want 1", name, len(uids))
	}
	return uids[0], nil
}

// notStorage refuses uid when it is the UID of one of the storage keys.
//
// decision: the comparison is by UID alone, not by endpoint and UID, since two
// spellings of one endpoint would otherwise let the storage key through. Two
// servers whose keys share a UID are refused visibly, and the operator names
// another key. Narrow it to endpoint and UID if KMIP servers ever report a
// stable server identity.
func notStorage(uid string, storage []StorageKey) error {
	for _, sk := range storage {
		if sk.ID == uid {
			return fmt.Errorf("UID %s is a storage wrapping key, which never wraps a namespace key", uid)
		}
	}
	return nil
}

// masterKeyIDOf is the version-qualified ID of the key qualifier's server
// holds under uid: qualifier/uid, as ASCII bytes.
//
// decision: the qualifier is the configured server name, or the endpoint when
// none is set, since a KMIP UID is unique only within one server. A name is
// the operator's claim that the endpoints it is given reach one server's key
// set: two servers given one name collide as bare UIDs would. With no name,
// an endpoint respelled or failed over changes every ID, which a rotation
// re-wraps. Withdraw the default if KMIP servers ever report a stable server
// identity.
func masterKeyIDOf(qualifier, uid string) ([]byte, error) {
	if !validUID(uid) {
		return nil, fmt.Errorf("UID %q is empty or holds a byte outside ! to ~, or a /, = or \"", uid)
	}
	id := qualifier + "/" + uid
	for i := 0; i < len(id); i++ {
		if id[i] >= 0x80 {
			return nil, fmt.Errorf("master-key ID %q is not ASCII", id)
		}
	}
	if len(id) > 0xFFFF {
		return nil, fmt.Errorf("master-key ID of %d bytes is longer than its 2-byte length holds", len(id))
	}
	return []byte(id), nil
}

// validUID reports whether uid is a UID a master-key ID may carry: printable
// ASCII without a space, /, = or ", so it never forges the result codes an
// error naming it holds.
func validUID(uid string) bool {
	return uid != "" && printable(uid) && !strings.ContainsAny(uid, ` /="`)
}

// splitID is a master-key ID's qualifier and UID, either side of its last /.
func splitID(masterKeyID []byte) (qualifier, uid string, err error) {
	i := bytes.LastIndexByte(masterKeyID, '/')
	if i < 0 || !validUID(string(masterKeyID[i+1:])) {
		return "", "", fmt.Errorf("%w: master-key ID %q names no valid UID", errStoredWrap, masterKeyID)
	}
	return string(masterKeyID[:i]), string(masterKeyID[i+1:]), nil
}

// wrap wraps each key of in under the master key's primary version, read
// first, so a wrap after the Name is retargeted records the new version.
//
// decision: a key-service wrap raises no wrap count: the service draws its own
// nonces and keeps its own bound on them (RFC 5 Appendix B.1). Withdraw it if
// a profile ever has the provider send the IV.
func (ks *keyService) wrap(ctx context.Context, in ...sealIn) ([][]byte, error) {
	conn := ks.conn.Load()
	uid, id, err := ks.primaryVersion(ctx, conn)
	if err != nil {
		return nil, err
	}
	what := fmt.Sprintf("Encrypt under KMIP key %q, UID %s", ks.key, uid)

	out := make([][]byte, 0, len(in))
	for _, w := range in {
		raw, err := conn.client.Do(ctx, kmip14.OperationEncrypt, encryptRequest{
			UniqueIdentifier:                      uid,
			CryptographicParameters:               gcmParameters(true),
			Data:                                  w.key,
			AuthenticatedEncryptionAdditionalData: w.aad,
		})
		if err != nil {
			return nil, unavailable(conn, what, err)
		}
		var resp encryptResponse
		if err := decodeTTLV(raw, &resp); err != nil {
			return nil, unavailable(conn, what, fmt.Errorf("decode the Encrypt response: %w", err))
		}
		if resp.UniqueIdentifier != uid {
			return nil, unavailable(conn, what, fmt.Errorf("the Encrypt response names UID %q", resp.UniqueIdentifier))
		}
		// GCM's data is as long as the key: a server that appends its tag
		// would store a record that never unwraps.
		if len(resp.Data) != len(w.key) {
			return nil, unavailable(conn, what, fmt.Errorf("the Encrypt response holds %d bytes of data for a %d-byte key", len(resp.Data), len(w.key)))
		}
		join, err := encodeKMIPWrap(resp.IVCounterNonce, resp.AuthenticatedEncryptionTag, resp.Data)
		if err != nil {
			return nil, unavailable(conn, what, err)
		}
		out = append(out, storedWrap{kind: kindKeyService, masterKeyID: id, wrap: join}.encode())
	}
	return out, nil
}

// unwrap asks the server to open stored, a key-service stored wrap, under the
// UID its master-key ID names and the associated data aad.
//
// decision: every failure of an unwrap is unavailable, never destroyed,
// whatever the server answers: a failed Decrypt cannot tell a destroyed key
// from a misconfigured endpoint, and only a state read of Destroyed reports
// loss. Withdraw it if a KMIP profile ever reports destruction on Decrypt
// unambiguously.
func (ks *keyService) unwrap(ctx context.Context, stored, aad []byte) ([]byte, error) {
	conn := ks.conn.Load()
	s, err := parseStoredWrap(stored)
	if err != nil {
		return nil, unavailable(conn, "unwrap", err)
	}
	if s.kind != kindKeyService {
		return nil, unavailable(conn, "unwrap", fmt.Errorf("stored wrap of provider kind %#x, not the key service's", s.kind))
	}
	what := fmt.Sprintf("Decrypt under master key %q", s.masterKeyID)
	_, uid, err := splitID(s.masterKeyID)
	if err != nil {
		return nil, unavailable(conn, what, err)
	}
	iv, tag, data, err := parseKMIPWrap(s.wrap)
	if err != nil {
		return nil, unavailable(conn, what, err)
	}

	raw, err := conn.client.Do(ctx, kmip14.OperationDecrypt, decryptRequest{
		UniqueIdentifier:                      uid,
		CryptographicParameters:               gcmParameters(false),
		Data:                                  data,
		IVCounterNonce:                        iv,
		AuthenticatedEncryptionAdditionalData: aad,
		AuthenticatedEncryptionTag:            tag,
	})
	if err != nil {
		return nil, unavailable(conn, what, err)
	}
	var resp decryptResponse
	if err := decodeTTLV(raw, &resp); err != nil {
		return nil, unavailable(conn, what, fmt.Errorf("decode the Decrypt response: %w", err))
	}
	return resp.Data, nil
}

// state reads the state of the master key masterKeyID names.
//
// decision: an ID qualified by another server than the one the custody
// reaches now is not read: the same UID there may name another key, whose
// Destroyed would mark this server's records lost. Its read gets no answer,
// so the caller keeps the state it last read. Withdraw it if KMIP servers
// ever report a stable server identity to check.
func (ks *keyService) state(ctx context.Context, masterKeyID []byte) error {
	conn := ks.conn.Load()
	qualifier, uid, err := splitID(masterKeyID)
	if err != nil {
		return unavailable(conn, "state read", err)
	}
	if qualifier != conn.qualifier {
		return fmt.Errorf("provider: state of master key %q: no answer: the custody reaches %s, not %s", masterKeyID, conn.qualifier, qualifier)
	}
	st, err := conn.client.State(ctx, uid)
	return stateAnswer(masterKeyID, conn.client.Endpoint(), st, err)
}

// stateAnswer is what a state read of the master key id at endpoint means,
// from the state it returned and its error: nil for Active,
// ErrMaterialDestroyed for Destroyed and Destroyed Compromised,
// ErrMaterialUnavailable for any other state and for Item Not Found, and an
// error wrapping neither for a read the server did not answer.
func stateAnswer(id []byte, endpoint string, st kmip14.State, err error) error {
	if err != nil && !itemNotFound(err) {
		return fmt.Errorf("provider: state of master key %q at %s: no answer: %w", id, endpoint, err)
	}
	switch kmip.Classify(st, err) {
	case kmip.Active:
		return nil
	case kmip.Destroyed:
		return fmt.Errorf("provider: master key %q at %s is %v: %w", id, endpoint, st, keys.ErrMaterialDestroyed)
	}
	if err != nil {
		return fmt.Errorf("provider: master key %q at %s: %w: %w", id, endpoint, keys.ErrMaterialUnavailable, err)
	}
	return fmt.Errorf("provider: master key %q at %s is %v: %w", id, endpoint, st, keys.ErrMaterialUnavailable)
}

// itemNotFound reports whether err is a server's answer that it holds no
// object under the UID.
//
// decision: it reads the reason's name in the error's text, as internal/kmip
// writes a failed status ("reason=ItemNotFound"), since the transport exports
// no error to tell an Item Not Found answer from no answer; the transport's
// tests pin that text, and validUID keeps a UID the error names from forging
// it. Replace it with an error the transport exports once it has one.
func itemNotFound(err error) bool {
	return strings.Contains(err.Error(), "reason=ItemNotFound")
}

// kmipWrapVersion1 is the key-service wrap bytes' one version.
const kmipWrapVersion1 byte = 1

// encodeKMIPWrap joins the fields an Encrypt response returned into a
// stored wrap's wrap bytes.
//
// decision: the wrap bytes are version (1 byte, 1) ‖ IV length (2 bytes,
// big-endian) ‖ IV ‖ tag (16 bytes) ‖ data, the Encrypt response's three
// fields as the server returned them; no field is interpreted, and the
// cipher's parameters are fixed, so they are not stored. The IV's length is,
// since servers draw 12 or 16 bytes. A change is version 2, which every reader
// keeps. Withdraw it if a profile ever returns its wrap as one opaque blob.
func encodeKMIPWrap(iv, tag, data []byte) ([]byte, error) {
	if len(iv) == 0 || len(iv) > 0xFFFF {
		return nil, fmt.Errorf("an IV of %d bytes, want 1 to 65,535", len(iv))
	}
	if len(tag) != tagLen {
		return nil, fmt.Errorf("a tag of %d bytes, want %d", len(tag), tagLen)
	}
	b := make([]byte, 0, 3+len(iv)+tagLen+len(data))
	b = append(b, kmipWrapVersion1)
	b = binary.BigEndian.AppendUint16(b, uint16(len(iv)))
	b = append(b, iv...)
	b = append(b, tag...)
	return append(b, data...), nil
}

// parseKMIPWrap splits key-service wrap bytes back into the IV, tag and data
// encodeKMIPWrap joined, refusing an unknown version, an empty IV, and a
// length that runs past b. The fields returned share b's bytes.
func parseKMIPWrap(b []byte) (iv, tag, data []byte, err error) {
	if len(b) < 3 {
		return nil, nil, nil, fmt.Errorf("%w: key-service wrap of %d bytes, too short for its header", errStoredWrap, len(b))
	}
	if b[0] != kmipWrapVersion1 {
		return nil, nil, nil, fmt.Errorf("%w: key-service wrap version %d", errStoredWrap, b[0])
	}
	ivLen := int(binary.BigEndian.Uint16(b[1:3]))
	rest := b[3:]
	if ivLen == 0 || len(rest) < ivLen+tagLen {
		return nil, nil, nil, fmt.Errorf("%w: key-service wrap: an IV of %d bytes and a tag of %d in %d", errStoredWrap, ivLen, tagLen, len(rest))
	}
	return rest[:ivLen:ivLen], rest[ivLen : ivLen+tagLen : ivLen+tagLen], rest[ivLen+tagLen:], nil
}

// gcmParameters are the Cryptographic Parameters of every Encrypt and
// Decrypt: AES-GCM with a 16-byte tag, the IV drawn by the server on Encrypt.
func gcmParameters(randomIV bool) cryptographicParameters {
	return cryptographicParameters{
		BlockCipherMode:        kmip14.BlockCipherModeGCM,
		CryptographicAlgorithm: kmip14.CryptographicAlgorithmAES,
		RandomIV:               randomIV,
		TagLength:              tagLen,
	}
}

// decodeTTLV is ttlv.Unmarshal with its panics returned as errors, since the
// bytes are the server's.
//
// ponytail: a copy of internal/kmip's unexported unmarshal. Delete it once
// the transport exports its own.
func decodeTTLV(raw ttlv.TTLV, v any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("malformed TTLV: %v", r)
		}
	}()
	return ttlv.Unmarshal(raw, v)
}

// The payloads of the operations only this custody sends, which the KMIP
// library models no types for (KMIP 1.4 §4.9, §4.29, §4.30).

type cryptographicParameters struct {
	BlockCipherMode        kmip14.BlockCipherMode
	CryptographicAlgorithm kmip14.CryptographicAlgorithm
	RandomIV               bool `ttlv:",omitempty"`
	TagLength              int
}

type attribute struct {
	AttributeName  string
	AttributeValue any
}

type kmipName struct {
	NameValue string
	NameType  kmip14.NameType
}

type locateRequest struct {
	Attribute []attribute
}

type locateResponse struct {
	UniqueIdentifier []string
}

type encryptRequest struct {
	UniqueIdentifier                      string
	CryptographicParameters               cryptographicParameters
	Data                                  []byte
	AuthenticatedEncryptionAdditionalData []byte
}

type encryptResponse struct {
	UniqueIdentifier           string
	Data                       []byte
	IVCounterNonce             []byte
	AuthenticatedEncryptionTag []byte
}

type decryptRequest struct {
	UniqueIdentifier                      string
	CryptographicParameters               cryptographicParameters
	Data                                  []byte
	IVCounterNonce                        []byte
	AuthenticatedEncryptionAdditionalData []byte
	AuthenticatedEncryptionTag            []byte
}

type decryptResponse struct {
	Data []byte
}
