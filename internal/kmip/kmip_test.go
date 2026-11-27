package kmip

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gemalto/kmip-go"
	"github.com/gemalto/kmip-go/kmip14"
	"github.com/gemalto/kmip-go/ttlv"

	"github.com/cubbit/dittofs/internal/prof"
)

// selfSigned returns a self-signed ECDSA certificate and its key, as PEM.
func selfSigned(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kmip-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func TestTLSConfig(t *testing.T) {
	prof.Watch(t, prof.Options{})
	certPEM, keyPEM := selfSigned(t)

	cfg, err := TLSConfig(certPEM, keyPEM, nil)
	if err != nil {
		t.Fatalf("TLSConfig without a CA: %v", err)
	}
	if len(cfg.Certificates) != 1 || cfg.RootCAs != nil {
		t.Fatalf("TLSConfig without a CA: %d certificates, RootCAs set %v; want 1 and the system roots", len(cfg.Certificates), cfg.RootCAs != nil)
	}
	if cfg.MinVersion != 0x0303 {
		t.Fatalf("MinVersion = %#x, want TLS 1.2", cfg.MinVersion)
	}

	cfg, err = TLSConfig(certPEM, keyPEM, certPEM)
	if err != nil {
		t.Fatalf("TLSConfig with a CA: %v", err)
	}
	if cfg.RootCAs == nil {
		t.Fatal("TLSConfig with a CA left RootCAs unset")
	}

	for _, tc := range []struct {
		name          string
		cert, key, ca []byte
		want          string
	}{
		{"client cert not PEM", []byte("garbage"), keyPEM, nil, "load client cert"},
		{"key not the cert's", certPEM, []byte("garbage"), nil, "load client cert"},
		{"empty CA", certPEM, keyPEM, []byte{}, "server CA has no PEM certs"},
		{"CA not PEM", certPEM, keyPEM, []byte("not a pem certificate"), "server CA has no PEM certs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := TLSConfig(tc.cert, tc.key, tc.ca); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("TLSConfig: error %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestExtractSymmetricKeyMaterial(t *testing.T) {
	prof.Watch(t, prof.Options{})
	material := []byte{1, 2, 3, 4}
	out, err := extractSymmetricKeyMaterial(&kmip.KeyValue{KeyMaterial: material})
	if err != nil || !bytes.Equal(out, material) {
		t.Fatalf("extract []byte: got %x, %v; want %x", out, err, material)
	}
	out[0] = 0xFF
	if material[0] == 0xFF {
		t.Fatal("extractSymmetricKeyMaterial aliased the input slice")
	}

	var buf bytes.Buffer
	enc := ttlv.NewEncoder(&buf)
	enc.EncodeByteString(kmip14.TagKeyMaterial, material)
	if err := enc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	out, err = extractSymmetricKeyMaterial(&kmip.KeyValue{KeyMaterial: ttlv.TTLV(buf.Bytes())})
	if err != nil || !bytes.Equal(out, material) {
		t.Fatalf("extract TTLV byte string: got %x, %v; want %x", out, err, material)
	}

	buf.Reset()
	enc.EncodeTextString(kmip14.TagKeyMaterial, "text")
	if err := enc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	for _, tc := range []struct {
		name string
		kv   *kmip.KeyValue
		want string
	}{
		{"nil KeyValue", nil, "missing KeyValue"},
		{"TTLV of another type", &kmip.KeyValue{KeyMaterial: ttlv.TTLV(buf.Bytes())}, "unexpected type"},
		{"unexpected go type", &kmip.KeyValue{KeyMaterial: 12345}, "unexpected go type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := extractSymmetricKeyMaterial(tc.kv); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("extract: error %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// response marshals a one-item response message for op.
func response(t *testing.T, op kmip14.Operation, status kmip14.ResultStatus, reason kmip14.ResultReason, payload any) ttlv.TTLV {
	t.Helper()
	item := kmip.ResponseBatchItem{Operation: op, ResultStatus: status, ResponsePayload: payload}
	if status != kmip14.ResultStatusSuccess {
		item.ResultReason = reason
		item.ResultMessage = "no such object"
	}
	raw, err := ttlv.Marshal(kmip.ResponseMessage{
		ResponseHeader: kmip.ResponseHeader{
			ProtocolVersion: kmip.ProtocolVersion{ProtocolVersionMajor: 1, ProtocolVersionMinor: 4},
			TimeStamp:       time.Now(),
			BatchCount:      1,
		},
		BatchItem: []kmip.ResponseBatchItem{item},
	})
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return raw
}

// getPayload is the decoded payload of a successful Get carrying material.
func getPayload(t *testing.T, objectType kmip14.ObjectType, material []byte) ttlv.TTLV {
	t.Helper()
	p := kmip.GetResponsePayload{ObjectType: objectType, UniqueIdentifier: "uid-1"}
	if material != nil {
		p.SymmetricKey = &kmip.SymmetricKey{KeyBlock: kmip.KeyBlock{
			KeyFormatType:          kmip14.KeyFormatTypeRaw,
			KeyValue:               &kmip.KeyValue{KeyMaterial: material},
			CryptographicAlgorithm: kmip14.CryptographicAlgorithmAES,
			CryptographicLength:    len(material) * 8,
		}}
	}
	raw, err := decodeResponse(kmip14.OperationGet, response(t, kmip14.OperationGet, kmip14.ResultStatusSuccess, 0, p))
	if err != nil {
		t.Fatalf("decodeResponse of a successful Get: %v", err)
	}
	return raw
}

// The response decode: each malformed or failed response is an error naming
// what was wrong, and none panics.
func TestDecodeResponse(t *testing.T) {
	prof.Watch(t, prof.Options{})
	noItems, err := ttlv.Marshal(kmip.ResponseMessage{ResponseHeader: kmip.ResponseHeader{
		ProtocolVersion: kmip.ProtocolVersion{ProtocolVersionMajor: 1, ProtocolVersionMinor: 4},
		TimeStamp:       time.Now(),
	}})
	if err != nil {
		t.Fatalf("marshal response with no items: %v", err)
	}
	for _, tc := range []struct {
		name string
		msg  ttlv.TTLV
		want []string
	}{
		{"no batch item", noItems, []string{"Get response has no batch items"}},
		{"failed status", response(t, kmip14.OperationGet, kmip14.ResultStatusOperationFailed, kmip14.ResultReasonItemNotFound, nil),
			[]string{"Get failed", "status=OperationFailed", "reason=ItemNotFound", "no such object"}},
		{"success with no payload", response(t, kmip14.OperationGet, kmip14.ResultStatusSuccess, 0, nil), []string{"Get payload is not TTLV", "<nil>"}},
		{"not TTLV", ttlv.TTLV{1, 2, 3}, []string{"unmarshal Get response"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeResponse(kmip14.OperationGet, tc.msg)
			if err == nil {
				t.Fatal("decodeResponse: nil error, want one")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("decodeResponse: error %q does not name %q", err, w)
				}
			}
		})
	}
}

func TestDecodeSymmetricKey(t *testing.T) {
	prof.Watch(t, prof.Options{})
	material := bytes.Repeat([]byte{0xA7}, 32)
	got, err := decodeSymmetricKey("uid-1", getPayload(t, kmip14.ObjectTypeSymmetricKey, material))
	if err != nil || !bytes.Equal(got, material) {
		t.Fatalf("decodeSymmetricKey: got %x, %v; want %x", got, err, material)
	}

	if _, err := decodeSymmetricKey("uid-1", getPayload(t, kmip14.ObjectTypeSymmetricKey, make([]byte, 16))); !errors.Is(err, ErrKeyLength) {
		t.Fatalf("decodeSymmetricKey of a 16-byte key: error %v, want ErrKeyLength", err)
	}
	for _, tc := range []struct {
		name string
		raw  ttlv.TTLV
		want string
	}{
		{"not a symmetric key", getPayload(t, kmip14.ObjectTypeSecretData, material), "is not a symmetric key"},
		{"no SymmetricKey", getPayload(t, kmip14.ObjectTypeSymmetricKey, nil), "missing SymmetricKey"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeSymmetricKey("uid-1", tc.raw)
			if err == nil || errors.Is(err, ErrKeyLength) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "uid-1") {
				t.Fatalf("decodeSymmetricKey: error %v, want one naming %q and the uid", err, tc.want)
			}
		})
	}
}

// stateAttrPayload is the decoded payload of a GetAttributes answering attrs.
func stateAttrPayload(t *testing.T, attrs ...kmip.Attribute) ttlv.TTLV {
	t.Helper()
	raw, err := decodeResponse(kmip14.OperationGetAttributes, response(t, kmip14.OperationGetAttributes, kmip14.ResultStatusSuccess, 0,
		getAttributesResponsePayload{UniqueIdentifier: "uid-1", Attribute: attrs}))
	if err != nil {
		t.Fatalf("decodeResponse of a GetAttributes: %v", err)
	}
	return raw
}

func TestDecodeState(t *testing.T) {
	prof.Watch(t, prof.Options{})
	got, err := decodeState("uid-1", stateAttrPayload(t,
		kmip.Attribute{AttributeName: "Name", AttributeValue: kmip.Name{NameValue: "k", NameType: kmip14.NameTypeUninterpretedTextString}},
		kmip.NewAttributeFromTag(kmip14.TagState, 0, kmip14.StateDeactivated)))
	if err != nil || got != kmip14.StateDeactivated {
		t.Fatalf("decodeState: got %v, %v; want Deactivated", got, err)
	}
	for _, tc := range []struct {
		name string
		raw  ttlv.TTLV
		want string
	}{
		{"no State", stateAttrPayload(t), "returned no State attribute"},
		{"State of another type", stateAttrPayload(t, kmip.Attribute{AttributeName: "State", AttributeValue: "Active"}), "State attribute has unexpected go type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeState("uid-1", tc.raw); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("decodeState: error %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// frame is a TTLV header declaring n value bytes, the value and its padding.
func frame(n uint32, pad int) []byte {
	f := make([]byte, headerLen+int(n)+pad)
	binary.BigEndian.PutUint32(f[4:8], n)
	for i := headerLen; i < headerLen+int(n); i++ {
		f[i] = 0xAA
	}
	return f
}

func TestReadMessage(t *testing.T) {
	prof.Watch(t, prof.Options{})
	for _, tc := range []struct {
		name string
		f    []byte
	}{
		{"padded", frame(3, 5)},
		{"aligned", frame(16, 0)},
		{"empty value", frame(0, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A second frame follows on the same reader: the read stops at
			// the first one's end.
			next := frame(8, 0)
			r := bytes.NewReader(append(append([]byte{}, tc.f...), next...))
			got, err := readMessage(r)
			if err != nil || !bytes.Equal(got, tc.f) {
				t.Fatalf("readMessage: got %x, %v; want %x", []byte(got), err, tc.f)
			}
			if r.Len() != len(next) {
				t.Fatalf("readMessage left %d bytes, want the next frame's %d", r.Len(), len(next))
			}
		})
	}
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"short header", []byte{0x01, 0x02}},
		{"short value", frame(16, 0)[:headerLen+4]},
		{"padding missing", frame(3, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := readMessage(bytes.NewReader(tc.in)); err == nil {
				t.Fatal("readMessage: nil error on a short read")
			}
		})
	}
}

// A length above the cap is refused before its body is allocated: the
// allocation such a claim would ask for never happens.
func TestReadMessageCap(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hdr := make([]byte, headerLen)
	binary.BigEndian.PutUint32(hdr[4:8], maxMessageSize+1)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := readMessage(bytes.NewReader(hdr))
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "exceeds cap") {
		t.Fatalf("readMessage of a length above the cap: error %v, want the cap refusal", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Fatalf("readMessage allocated %d bytes before refusing the length, want under 1 MiB", grew)
	}

	binary.BigEndian.PutUint32(hdr[4:8], maxMessageSize)
	if _, err := readMessage(bytes.NewReader(hdr)); err == nil || strings.Contains(err.Error(), "exceeds cap") {
		t.Fatalf("readMessage of a length at the cap: error %v, want a short read, not the cap refusal", err)
	}
}

// closedEndpoint returns the address of a listener on 127.0.0.1 closed
// before return, so a dial to it is refused.
func closedEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

// A dial to a closed listener fails within the timeout and names the
// endpoint, and the reads over it name the key.
func TestDialClosedListener(t *testing.T) {
	prof.Watch(t, prof.Options{})
	certPEM, keyPEM := selfSigned(t)
	tlsCfg, err := TLSConfig(certPEM, keyPEM, certPEM)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	endpoint := closedEndpoint(t)
	const timeout = 2 * time.Second
	c := NewClient(endpoint, tlsCfg, timeout)
	if c.Endpoint() != endpoint {
		t.Fatalf("Endpoint() = %q, want %q", c.Endpoint(), endpoint)
	}
	start := time.Now()
	_, getErr := c.GetSymmetricKey(context.Background(), "uid-1")
	_, stateErr := c.State(context.Background(), "uid-1")
	// A bounded wait is the assertion here: no clock reaches the dialer.
	if elapsed := time.Since(start); elapsed > 2*timeout {
		t.Fatalf("two dials took %s, want each within the %s timeout", elapsed, timeout)
	}
	for name, err := range map[string]error{"GetSymmetricKey": getErr, "State": stateErr} {
		if err == nil || !strings.Contains(err.Error(), "dial "+endpoint) || !strings.Contains(err.Error(), "uid-1") {
			t.Fatalf("%s: error %v, want a dial error naming %s and the uid", name, err, endpoint)
		}
	}
}

func TestNewClientDefaultTimeout(t *testing.T) {
	prof.Watch(t, prof.Options{})
	for _, in := range []time.Duration{0, -time.Second} {
		if got := NewClient("h:1", nil, in).timeout; got != defaultTimeout {
			t.Fatalf("NewClient(timeout %s): timeout %s, want the default %s", in, got, defaultTimeout)
		}
	}
	if got := NewClient("h:1", nil, time.Second).timeout; got != time.Second {
		t.Fatalf("NewClient(timeout 1s): timeout %s, want 1s", got)
	}
}
