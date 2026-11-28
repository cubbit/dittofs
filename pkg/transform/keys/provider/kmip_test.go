package provider

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gemalto/kmip-go/kmip14"
	"github.com/gemalto/kmip-go/ttlv"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// The vector testdata/kmip-stored-wrap-v1.py computes: the tag and data an
// AES-256-GCM Encrypt under its master key (bytes 0x00 to 0x1f) returns for
// the stored-wrap vector's key and AAD under IV 0xb0 to 0xbf, and the
// key-service stored wrap of UID 3 at 127.0.0.1:5696 that joins them.
//
// ponytail: the vector is this package's own, computed by a script beside it,
// until RFC 5's golden vectors have a shared loader; move it there then.
const (
	kmipVectorTag    = "3d158ceb6a53d777fb3a24d15497e743"
	kmipVectorData   = "e15cd621be0643e777faf12b9bb8be45ca7ccabf8a6b185134f85276358ed0ea"
	kmipVectorStored = "0200103132372e302e302e313a353639362f3300000043010010b0b1b2b3b4b5b6b7b8b9babbbcbdbebf3d158ceb6a53d777fb3a24d15497e743e15cd621be0643e777faf12b9bb8be45ca7ccabf8a6b185134f85276358ed0ea"
)

// The stored wrap built from the vector's endpoint, UID, IV, tag and data
// equals the vector another implementation computed, the reader splits it
// back into those fields, and the fields open under the vector's master key
// and AAD as the server's Encrypt sealed them. A field order, a length width,
// a byte order, a missing kind byte or an ID without its qualifier fails here.
func TestKMIPStoredWrapVector(t *testing.T) {
	prof.Watch(t, prof.Options{})

	iv, tag, data := span(0xb0, 0xc0), mustHex(t, kmipVectorTag), mustHex(t, kmipVectorData)
	id, err := masterKeyIDOf("127.0.0.1:5696", "3")
	if err != nil {
		t.Fatalf("master-key ID: %v", err)
	}
	join, err := encodeKMIPWrap(iv, tag, data)
	if err != nil {
		t.Fatalf("encodeKMIPWrap: %v", err)
	}
	stored := storedWrap{kind: kindKeyService, masterKeyID: id, wrap: join}.encode()
	if got := mustHex(t, kmipVectorStored); !bytes.Equal(stored, got) {
		t.Fatalf("stored wrap %x, want the vector's %x", stored, got)
	}

	s, err := parseStoredWrap(stored)
	if err != nil {
		t.Fatalf("parse the vector's stored wrap: %v", err)
	}
	gotIV, gotTag, gotData, err := parseKMIPWrap(s.wrap)
	if err != nil {
		t.Fatalf("parse the vector's wrap bytes: %v", err)
	}
	if !bytes.Equal(gotIV, iv) || !bytes.Equal(gotTag, tag) || !bytes.Equal(gotData, data) {
		t.Errorf("the vector's wrap bytes split into IV %x, tag %x, data %x; want %x, %x, %x", gotIV, gotTag, gotData, iv, tag, data)
	}
	if q, uid, err := splitID(s.masterKeyID); err != nil || q != "127.0.0.1:5696" || uid != "3" {
		t.Errorf("split of the vector's master-key ID %q = %q, %q, %v; want 127.0.0.1:5696 and 3", s.masterKeyID, q, uid, err)
	}

	block, err := aes.NewCipher(span(0x00, 0x20))
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(iv))
	if err != nil {
		t.Fatal(err)
	}
	key, err := gcm.Open(nil, gotIV, append(bytes.Clone(gotData), gotTag...), mustHex(t, vectorAAD))
	if err != nil || !bytes.Equal(key, span(0x50, 0x70)) {
		t.Errorf("the vector's fields open to %x, %v; want the vector's key %x", key, err, span(0x50, 0x70))
	}
}

func TestKMIPWrapRefusals(t *testing.T) {
	prof.Watch(t, prof.Options{})

	good := mustHex(t, kmipVectorStored)
	s, err := parseStoredWrap(good)
	if err != nil {
		t.Fatal(err)
	}
	join := s.wrap
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"a join cut inside its header", join[:2]},
		{"an unknown version", append([]byte{2}, join[1:]...)},
		{"an IV of zero bytes", append([]byte{1, 0, 0}, join[3:]...)},
		{"an IV length past the end", []byte{1, 0xff, 0xff, 1, 2, 3}},
		{"a body shorter than its tag", join[:3+16+15]},
	} {
		if iv, tag, data, err := parseKMIPWrap(tc.b); !errors.Is(err, errStoredWrap) {
			t.Errorf("parseKMIPWrap of %s = %x, %x, %x, %v; want errStoredWrap", tc.name, iv, tag, data, err)
		}
	}
	if _, _, _, err := parseKMIPWrap(join[:3+16+16]); err != nil {
		t.Errorf("parseKMIPWrap of a join with an empty body = %v, want it accepted", err)
	}

	for _, tc := range []struct {
		name         string
		iv, tag, out []byte
	}{
		{"an empty IV", nil, make([]byte, 16), nil},
		{"an IV past its 2-byte length", make([]byte, 0x10000), make([]byte, 16), nil},
		{"a 15-byte tag", make([]byte, 16), make([]byte, 15), nil},
	} {
		if b, err := encodeKMIPWrap(tc.iv, tc.tag, []byte{1}); err == nil {
			t.Errorf("encodeKMIPWrap of %s = %x, want an error", tc.name, b)
		}
	}
}

func TestMasterKeyIDRefusals(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, tc := range []struct {
		name, qualifier, uid string
	}{
		{"a non-ASCII UID", "kms-a", "clé"},
		{"a non-ASCII qualifier", "kmś", "3"},
		{"an ID of 65,536 bytes", strings.Repeat("q", 65534), "3"},
		{"an empty UID", "kms-a", ""},
		{"a UID holding a /", "kms-a", "3/4"},
		{"a UID holding a space and an =", "kms-a", "3 reason=ItemNotFound"},
	} {
		if id, err := masterKeyIDOf(tc.qualifier, tc.uid); err == nil {
			t.Errorf("masterKeyIDOf for %s = %q, want an error", tc.name, id)
		}
	}
	if id, err := masterKeyIDOf(strings.Repeat("q", 65533), "3"); err != nil || len(id) != 65535 {
		t.Errorf("masterKeyIDOf of 65,535 bytes = %d bytes, %v; want it accepted", len(id), err)
	}

	for _, id := range []string{"no-slash", "kms-a/", "kms-a/3 reason=ItemNotFound"} {
		if _, uid, err := splitID([]byte(id)); !errors.Is(err, errStoredWrap) {
			t.Errorf("splitID(%q) = %q, %v; want errStoredWrap", id, uid, err)
		}
	}
	if q, uid, err := splitID([]byte("kms/a/3")); err != nil || q != "kms/a" || uid != "3" {
		t.Errorf("splitID(kms/a/3) = %q, %q, %v; want kms/a and 3, either side of the last /", q, uid, err)
	}
}

func TestLocatedOne(t *testing.T) {
	prof.Watch(t, prof.Options{})

	if uid, err := locatedOne("mk", []string{"7"}); err != nil || uid != "7" {
		t.Errorf("locatedOne of one UID = %q, %v; want 7", uid, err)
	}
	for _, uids := range [][]string{nil, {"7", "8"}} {
		uid, err := locatedOne("mk", uids)
		if err == nil {
			t.Errorf("locatedOne of %d UIDs = %q, want an error", len(uids), uid)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, `"mk"`) || !strings.Contains(msg, fmt.Sprintf("found %d keys", len(uids))) {
			t.Errorf("locatedOne of %d UIDs: %q, want the key's name and the count", len(uids), msg)
		}
	}
}

// The comparison takes no endpoint: a storage key's UID is refused at any
// spelling of the server's address.
func TestNotStorage(t *testing.T) {
	prof.Watch(t, prof.Options{})

	storage := []StorageKey{{ID: "9"}, {ID: "3"}}
	if err := notStorage("3", storage); err == nil {
		t.Error("notStorage of a storage key's UID = nil, want an error")
	}
	if err := notStorage("4", storage); err != nil {
		t.Errorf("notStorage of another UID = %v, want nil", err)
	}
}

// clientPEMs writes a self-signed client certificate under t.TempDir() and
// returns its path and its key's PEM.
func clientPEMs(t *testing.T) (certPath string, keyPEM []byte) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "dittofs-test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(t.TempDir(), "client-cert.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// kmipConfig is a configuration whose certificate loads, at endpoint.
func kmipConfig(t *testing.T, endpoint string) KMIPConfig {
	t.Helper()

	cert, key := clientPEMs(t)
	return KMIPConfig{Endpoint: endpoint, Key: "mk", ClientCert: cert, ClientKey: key}
}

// closedEndpoint is the address of a listener on 127.0.0.1 closed before it
// is returned, so a dial to it is refused.
func closedEndpoint(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestValidateConfigKMIP(t *testing.T) {
	prof.Watch(t, prof.Options{})

	_, keyPEM := clientPEMs(t)
	// No file is present at either path and nothing listens at the endpoint:
	// a rule that read a file or dialled would fail the valid row.
	valid := KMIPConfig{Endpoint: "127.0.0.1:1", ServerName: "kms-a", Key: "mk", ClientCert: "/nonexistent/cert.pem", ClientKey: keyPEM, ServerCA: "/nonexistent/ca.pem", Timeout: time.Second}
	if err := ValidateConfig(Config{KMIP: valid}); err != nil {
		t.Fatalf("ValidateConfig of a valid KMIP configuration = %v, want nil", err)
	}

	with := func(edit func(*KMIPConfig)) Config {
		c := valid
		edit(&c)
		return Config{KMIP: c}
	}
	for _, tc := range []struct {
		name, field string
		cfg         Config
	}{
		{"a key file beside a key service", "key file", Config{KeyFile: "/nonexistent/master.key", Passphrase: passphrase, KMIP: valid}},
		{"an endpoint without a port", "KMIP.Endpoint", with(func(c *KMIPConfig) { c.Endpoint = "kms.example" })},
		{"an endpoint without a host", "KMIP.Endpoint", with(func(c *KMIPConfig) { c.Endpoint = ":5696" })},
		{"an empty key", "KMIP.Key", with(func(c *KMIPConfig) { c.Key = "" })},
		{"an empty client certificate", "KMIP.ClientCert", with(func(c *KMIPConfig) { c.ClientCert = "" })},
		{"an empty client key", "KMIP.ClientKey", with(func(c *KMIPConfig) { c.ClientKey = nil })},
		{"a client key after other text", "KMIP.ClientKey", with(func(c *KMIPConfig) { c.ClientKey = append([]byte("junk\n"), keyPEM...) })},
		{"an endpoint not ASCII, no server name", "KMIP.Endpoint", with(func(c *KMIPConfig) { c.ServerName, c.Endpoint = "", "kmś.example:5696" })},
		{"a server name of 65,534 bytes", "KMIP.ServerName", with(func(c *KMIPConfig) { c.ServerName = strings.Repeat("k", 65534) })},
		{"a client key that is no PEM", "KMIP.ClientKey", with(func(c *KMIPConfig) { c.ClientKey = []byte("not a PEM") })},
		{"a client key of two PEM blocks", "KMIP.ClientKey", with(func(c *KMIPConfig) { c.ClientKey = append(bytes.Clone(keyPEM), keyPEM...) })},
		{"a server name with a control byte", "KMIP.ServerName", with(func(c *KMIPConfig) { c.ServerName = "kms\n" })},
		{"a server name not ASCII", "KMIP.ServerName", with(func(c *KMIPConfig) { c.ServerName = "kmś" })},
		{"a negative timeout", "KMIP.Timeout", with(func(c *KMIPConfig) { c.Timeout = -time.Second })},
	} {
		err := ValidateConfig(tc.cfg)
		if err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("ValidateConfig of %s = %v, want an error naming %s", tc.name, err, tc.field)
		}
		if err != nil && bytes.Contains([]byte(err.Error()), keyPEM[40:80]) {
			t.Errorf("ValidateConfig of %s names the client key's bytes: %v", tc.name, err)
		}
	}
}

// New over a key service loads the certificate and dials nothing, so it
// succeeds with nothing listening; the escrow gate passes, since the master
// key lives off the host, and VerifyEscrow has no key file to check.
func TestNewOverAKeyService(t *testing.T) {
	prof.Watch(t, prof.Options{})

	storage := []StorageKey{{ID: "3"}}
	p, err := New(context.Background(), Config{KMIP: kmipConfig(t, closedEndpoint(t))}, Deps{Records: keystest.NewRecords(), Rand: rand.Reader, StorageKeys: storage})
	if err != nil {
		t.Fatalf("New over a key service nobody listens for = %v, want it built", err)
	}
	// The storage keys reach the custody, whose primary read refuses them;
	// that refusal through a Locate is a row against the real server.
	if got := p.custody.(*keyService).storage; len(got) != 1 || notStorage("3", got) == nil {
		t.Errorf("the key-service custody holds storage keys %v, want %v", got, storage)
	}
	if p.custody.kind() != kindKeyService || p.kf != nil {
		t.Errorf("New over a key service built a custody of kind %#x and a key file %v; want kind %#x and none", p.custody.kind(), p.kf, kindKeyService)
	}
	if err := p.checkEscrow(); err != nil {
		t.Errorf("escrow gate over a key service = %v, want nil", err)
	}
	if _, err := p.VerifyEscrow(context.Background(), []byte("a copy")); err == nil {
		t.Error("VerifyEscrow over a key service = nil, want an error")
	}

	cfg := kmipConfig(t, "127.0.0.1:1")
	cfg.ClientCert = filepath.Join(t.TempDir(), "absent.pem")
	if _, err := New(context.Background(), Config{KMIP: cfg}, Deps{Records: keystest.NewRecords(), Rand: rand.Reader}); err == nil {
		t.Error("New over a key service whose certificate file is absent = nil, want an error")
	}
}

// A call that gets no answer, a refused dial or a cancelled context, is
// unavailable for a wrap, an unwrap and a primary read, and neither
// unavailable nor destroyed for a state read, which the caller then keeps.
func TestKeyServiceErrorMapping(t *testing.T) {
	prof.Watch(t, prof.Options{})

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name     string
		ctx      context.Context
		endpoint string
	}{
		{"a refused dial", context.Background(), closedEndpoint(t)},
		// The listener's backlog accepts the dial and nothing ever answers,
		// and the timeout is an hour, so only the cancelled context ends the
		// calls: a custody that ignored it would hang the test, a bounded
		// wait being the assertion here.
		{"a cancelled context", cancelled, silentEndpoint(t)},
	} {
		cfg := kmipConfig(t, tc.endpoint)
		cfg.Timeout = time.Hour
		ks, err := newKeyService(cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, wrapErr := ks.wrap(tc.ctx, sealIn{aad: []byte("aad"), key: make([]byte, 32)})
		_, unwrapErr := ks.unwrap(tc.ctx, mustHex(t, kmipVectorStored), mustHex(t, vectorAAD))
		_, primaryErr := ks.primary(tc.ctx)
		for op, err := range map[string]error{"wrap": wrapErr, "unwrap": unwrapErr, "primary": primaryErr} {
			if !errors.Is(err, keys.ErrMaterialUnavailable) || errors.Is(err, keys.ErrMaterialDestroyed) {
				t.Errorf("%s after %s = %v, want ErrMaterialUnavailable and never ErrMaterialDestroyed", op, tc.name, err)
			}
		}
		err = ks.state(tc.ctx, []byte(tc.endpoint+"/3"))
		if err == nil || errors.Is(err, keys.ErrMaterialUnavailable) || errors.Is(err, keys.ErrMaterialDestroyed) {
			t.Errorf("state after %s = %v, want an error wrapping neither ErrMaterialUnavailable nor ErrMaterialDestroyed", tc.name, err)
		}
	}
}

// silentEndpoint is the address of a listener on 127.0.0.1 that accepts into
// its backlog and never answers, closed when the test ends.
func silentEndpoint(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().String()
}

// A state read of an ID another server qualifies is not sent: the same UID
// there may name another key. It gets no answer, neither unavailable nor
// destroyed, and names both qualifiers.
func TestStateOfAForeignQualifier(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ks, err := newKeyService(kmipConfig(t, closedEndpoint(t)), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = ks.state(context.Background(), []byte("old-kms:5696/3"))
	if err == nil || errors.Is(err, keys.ErrMaterialUnavailable) || errors.Is(err, keys.ErrMaterialDestroyed) {
		t.Fatalf("state of another server's ID = %v, want an error wrapping neither sentinel", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "old-kms:5696") || strings.Contains(msg, "dial") {
		t.Errorf("state of another server's ID = %q, want it refused before any dial, naming old-kms:5696", msg)
	}
}

// decodeTTLV returns an error, never a panic, for bytes a server sends that
// are not the payload asked for.
func TestDecodeTTLVRefusesMalformedPayloads(t *testing.T) {
	prof.Watch(t, prof.Options{})

	byteString := ttlv.TTLV{0x42, 0x00, 0x7c, byte(ttlv.TypeByteString), 0, 0, 0, 1, 0xaa, 0, 0, 0, 0, 0, 0, 0}
	for _, tc := range []struct {
		name string
		raw  ttlv.TTLV
		into any
	}{
		{"nothing", nil, &encryptResponse{}},
		{"a short header", ttlv.TTLV{0x42, 0x00}, &locateResponse{}},
		{"a byte string where a structure belongs", byteString, &encryptResponse{}},
		{"a byte string into the Decrypt response", byteString, &decryptResponse{}},
	} {
		if err := decodeTTLV(tc.raw, tc.into); err == nil {
			t.Errorf("decodeTTLV of %s = nil, want an error", tc.name)
		}
	}
}

// The state read's answers, from the state the server returned and the
// read's error as internal/kmip words a failed status.
func TestStateAnswer(t *testing.T) {
	prof.Watch(t, prof.Options{})

	failed := func(reason string) error {
		return errors.New("state of key 3: kmip: GetAttributes failed: status=OperationFailed reason=" + reason + ` message="no"`)
	}
	const (
		active = iota
		unavailable
		destroyed
		noAnswer
	)
	for _, tc := range []struct {
		name  string
		state kmip14.State
		err   error
		want  int
	}{
		{"Active", kmip14.StateActive, nil, active},
		{"Destroyed", kmip14.StateDestroyed, nil, destroyed},
		{"Destroyed Compromised", kmip14.StateDestroyedCompromised, nil, destroyed},
		{"Compromised", kmip14.StateCompromised, nil, unavailable},
		{"Deactivated", kmip14.StateDeactivated, nil, unavailable},
		{"Pre-Active", kmip14.StatePreActive, nil, unavailable},
		{"a state KMIP 1.4 does not name", kmip14.State(0x99), nil, unavailable},
		{"Item Not Found", 0, failed("ItemNotFound"), unavailable},
		{"another failed status", 0, failed("PermissionDenied"), noAnswer},
		{"a dial error", 0, errors.New("state of key 3: kmip: dial 127.0.0.1:1: connection refused"), noAnswer},
	} {
		err := stateAnswer([]byte("kms-a/3"), "127.0.0.1:5696", tc.state, tc.err)
		got := noAnswer
		switch {
		case err == nil:
			got = active
		case errors.Is(err, keys.ErrMaterialDestroyed):
			got = destroyed
		case errors.Is(err, keys.ErrMaterialUnavailable):
			got = unavailable
		}
		if got != tc.want {
			t.Errorf("stateAnswer of %s = %v, class %d; want class %d (0 active, 1 unavailable, 2 destroyed, 3 no answer)", tc.name, err, got, tc.want)
		}
		if err != nil && !strings.Contains(err.Error(), "kms-a/3") {
			t.Errorf("stateAnswer of %s = %q, want it to name the master key", tc.name, err)
		}
	}
}

// The master-key ID's qualifier is the server name, or the endpoint when none
// is set; a reconfigure to another endpoint keeps a set name's IDs and moves
// an unset one's to the new endpoint.
func TestMasterKeyIDQualifier(t *testing.T) {
	prof.Watch(t, prof.Options{})

	idOf := func(ks *keyService) string {
		id, err := masterKeyIDOf(ks.conn.Load().qualifier, "3")
		if err != nil {
			t.Fatal(err)
		}
		return string(id)
	}
	for _, tc := range []struct {
		serverName, before, after string
	}{
		{"kms-a", "kms-a/3", "kms-a/3"},
		{"", "127.0.0.1:5696/3", "localhost:5696/3"},
	} {
		cfg := kmipConfig(t, "127.0.0.1:5696")
		cfg.ServerName = tc.serverName
		ks, err := newKeyService(cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := idOf(ks); got != tc.before {
			t.Errorf("master-key ID with server name %q = %q, want %q", tc.serverName, got, tc.before)
		}
		cfg.Endpoint = "localhost:5696"
		if err := ks.reconfigure(cfg); err != nil {
			t.Fatalf("reconfigure to another endpoint: %v", err)
		}
		if got := idOf(ks); got != tc.after {
			t.Errorf("master-key ID with server name %q after a move to localhost:5696 = %q, want %q", tc.serverName, got, tc.after)
		}
	}
}

// reconfigure refuses a changed key, an empty endpoint and a client key that
// does not load, keeping the running client after each, and swaps in a new
// client otherwise.
func TestKeyServiceReconfigure(t *testing.T) {
	prof.Watch(t, prof.Options{})

	cfg := kmipConfig(t, "127.0.0.1:5696")
	ks, err := newKeyService(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	running := ks.conn.Load()

	notAKey := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("garbage")})
	for _, tc := range []struct {
		name string
		edit func(*KMIPConfig)
	}{
		{"a changed key", func(c *KMIPConfig) { c.Key = "another" }},
		{"an empty endpoint", func(c *KMIPConfig) { c.Endpoint = "" }},
		{"a client key that does not parse", func(c *KMIPConfig) { c.ClientKey = notAKey }},
		{"a certificate file that is absent", func(c *KMIPConfig) { c.ClientCert = filepath.Join(t.TempDir(), "absent.pem") }},
		{"a CA file that is absent", func(c *KMIPConfig) { c.ServerCA = filepath.Join(t.TempDir(), "absent.pem") }},
		{"a CA holding no certificate", func(c *KMIPConfig) {
			c.ServerCA = c.ClientCert + ".none"
			_ = os.WriteFile(c.ServerCA, []byte("none"), 0o600)
		}},
		{"an empty key", func(c *KMIPConfig) { c.Key = "" }},
		{"an empty certificate path", func(c *KMIPConfig) { c.ClientCert = "" }},
		{"an empty client key", func(c *KMIPConfig) { c.ClientKey = nil }},
	} {
		next := cfg
		next.Endpoint = "127.0.0.2:5696"
		tc.edit(&next)
		if err := ks.reconfigure(next); err == nil {
			t.Errorf("reconfigure with %s = nil, want an error", tc.name)
		}
		if ks.conn.Load() != running {
			t.Errorf("reconfigure with %s replaced the running client", tc.name)
		}
	}

	next := cfg
	next.Endpoint, next.Timeout = "127.0.0.2:5696", 3*time.Second
	if err := ks.reconfigure(next); err != nil {
		t.Fatalf("reconfigure to a new endpoint and timeout: %v", err)
	}
	if got := ks.conn.Load().client.Endpoint(); got != next.Endpoint {
		t.Errorf("client endpoint after reconfigure = %s, want %s", got, next.Endpoint)
	}

	// Through the provider: a key-service provider takes the change, and
	// refuses a move to a key file.
	p, err := New(context.Background(), Config{KMIP: cfg}, Deps{Records: keystest.NewRecords(), Rand: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Reconfigure(context.Background(), Config{KMIP: next}); err != nil {
		t.Errorf("Reconfigure of a key-service provider to a new endpoint = %v, want nil", err)
	}
	if got := p.custody.(*keyService).conn.Load().client.Endpoint(); got != next.Endpoint {
		t.Errorf("Reconfigure left the client at %s, want %s", got, next.Endpoint)
	}
	if got := p.cfg.Load().KMIP.Endpoint; got != next.Endpoint {
		t.Errorf("Reconfigure left the configuration's endpoint %s, want %s", got, next.Endpoint)
	}
	path, _, _ := keyFile(t)
	if err := p.Reconfigure(context.Background(), Config{KeyFile: path, Passphrase: passphrase}); err == nil {
		t.Error("Reconfigure of a key-service provider to a key file = nil, want an error")
	}
	kp, _, _ := newProvider(t, rand.Reader)
	if err := kp.Reconfigure(context.Background(), Config{KMIP: cfg}); err == nil {
		t.Error("Reconfigure of a key-file provider to a key service = nil, want an error")
	}
}
