package content

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/bytesize"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/codec"
	"github.com/cubbit/dittofs/pkg/remote/s3"
	"github.com/cubbit/dittofs/pkg/settings"
)

// The access key and the secret every credential case carries, grepped for
// in each refusal.
const (
	testKey    = "AKIDFAKEKEY"
	testSecret = "fake-secret-value"
)

// ParseCredential reads exactly the JSON object of the two names and
// refuses every other shape, naming the field and never printing the key or
// the secret.
func TestParseCredential(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string // a substring of the error; "" is accepted
	}{
		{"the object", `{"access_key_id":"` + testKey + `","secret_access_key":"` + testSecret + `"}`, ""},
		{"the names in the other order, with whitespace", " {\n \"secret_access_key\": \"" + testSecret + "\", \"access_key_id\": \"" + testKey + "\"\n}\n", ""},
		{"not JSON", testKey + ":" + testSecret, "is not a JSON object"},
		{"empty", ``, "is not a JSON object"},
		{"a JSON string", `"` + testKey + `"`, "is not a JSON object"},
		{"an array", `["` + testKey + `","` + testSecret + `"]`, "is not a JSON object"},
		{"cut short", `{"access_key_id":"` + testKey + `","secret_access_key":"` + testSecret + `"`, "is not a JSON object"},
		{"an unknown field", `{"access_key_id":"` + testKey + `","secret_access_key":"` + testSecret + `","session_token":"t"}`, "a field other than"},
		{"the key as a field name", `{"` + testKey + `":"` + testSecret + `"}`, "a field other than"},
		{"a name in another case", `{"Access_Key_Id":"` + testKey + `","secret_access_key":"` + testSecret + `"}`, "a field other than"},
		{"a name repeated", `{"access_key_id":"` + testKey + `","access_key_id":"` + testKey + `","secret_access_key":"` + testSecret + `"}`, "access_key_id is given twice"},
		{"an empty key", `{"access_key_id":"","secret_access_key":"` + testSecret + `"}`, "access_key_id is empty"},
		{"an empty secret", `{"access_key_id":"` + testKey + `","secret_access_key":""}`, "secret_access_key is empty"},
		{"a number for the secret", `{"access_key_id":"` + testKey + `","secret_access_key":12}`, "secret_access_key is not a string"},
		{"an object for the key", `{"access_key_id":{"v":"` + testKey + `"},"secret_access_key":"` + testSecret + `"}`, "access_key_id is not a string"},
		{"the secret missing", `{"access_key_id":"` + testKey + `"}`, "secret_access_key is missing"},
		{"the key missing", `{"secret_access_key":"` + testSecret + `"}`, "access_key_id is missing"},
		{"a value after the object", `{"access_key_id":"` + testKey + `","secret_access_key":"` + testSecret + `"} x`, "something after the object"},
		{"a second object", `{"access_key_id":"` + testKey + `","secret_access_key":"` + testSecret + `"}{}`, "something after the object"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, secret, err := ParseCredential([]byte(c.value))
			if c.want == "" {
				if err != nil || key != testKey || secret != testSecret {
					t.Fatalf("ParseCredential = (%q, %q, %v), want (%q, %q, nil)", key, secret, err, testKey, testSecret)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ParseCredential = %v, want an error naming %q", err, c.want)
			}
			if key != "" || secret != "" {
				t.Errorf("ParseCredential refused and returned (%q, %q), want both empty", key, secret)
			}
			if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), testSecret) {
				t.Errorf("ParseCredential = %q, which prints the key or the secret", err)
			}
		})
	}
}

// goodCredential is a credential value ParseCredential accepts.
var goodCredential = []byte(`{"access_key_id":"` + testKey + `","secret_access_key":"` + testSecret + `"}`)

// filled is the section's defaults with the required settings set, the
// record a store holding only its required keys reads as.
func filled() remoteSettings {
	s := remoteDefault
	s.Endpoint, s.Bucket = "https://s3.example.test", "bucket"
	return s
}

// clientConfig carries every setting and the credential into the client's
// configuration, the region and the storage class from the defaults.
func TestClientConfigMapsEveryField(t *testing.T) {
	roots := testRoots(t, httptest.NewUnstartedServer(nil))
	s := filled()
	s.TrustRoots = roots

	cfg, err := clientConfig(s, goodCredential, 7, 9*bytesize.MiB.Int64())
	if err != nil {
		t.Fatalf("clientConfig: %v", err)
	}
	want := s3.ClientConfig{Endpoint: "https://s3.example.test", Region: "us-east-1", Bucket: "bucket", StorageClass: "STANDARD",
		AccessKeyID: testKey, SecretAccessKey: testSecret, Limit: 7, MaxBlockSize: 9 * bytesize.MiB.Int64()}
	got := cfg
	got.TrustRoots = nil
	if got != want {
		t.Errorf("clientConfig = %+v, want %+v", got, want)
	}
	want509 := x509.NewCertPool()
	want509.AppendCertsFromPEM([]byte(roots))
	if !cfg.TrustRoots.Equal(want509) {
		t.Errorf("clientConfig: TrustRoots %v, want the pool of the setting's bundle", cfg.TrustRoots)
	}

	other := filled()
	other.Region, other.StorageClass = "eu-west-1", "STANDARD_IA"
	if cfg, err := clientConfig(other, goodCredential, 1, 1); err != nil || cfg.Region != "eu-west-1" || cfg.StorageClass != "STANDARD_IA" || cfg.TrustRoots != nil {
		t.Errorf("clientConfig with region eu-west-1 and STANDARD_IA = (region %q, class %q, roots %v, %v), want (eu-west-1, STANDARD_IA, nil, nil)",
			cfg.Region, cfg.StorageClass, cfg.TrustRoots, err)
	}
}

// clientConfig refuses a credential ParseCredential refuses, and settings
// Validate refuses, each wrapping ErrInvalid and printing neither the key
// nor the secret.
func TestClientConfigRefuses(t *testing.T) {
	pair := `"access_key_id":"` + testKey + `","secret_access_key":"` + testSecret + `"`
	for _, c := range []struct {
		name       string
		settings   func(*remoteSettings)
		credential string
	}{
		{"a credential that is not the object", nil, testKey + ":" + testSecret},
		{"an unknown field", nil, `{` + pair + `,"token":"t"}`},
		{"an empty field", nil, `{"access_key_id":"","secret_access_key":"` + testSecret + `"}`},
		{"a value after the object", nil, `{` + pair + `}[]`},
		{"a name in another case", nil, `{"ACCESS_KEY_ID":"` + testKey + `","secret_access_key":"` + testSecret + `"}`},
		{"a repeated name", nil, `{` + pair + `,"secret_access_key":"` + testSecret + `"}`},
		{"an http endpoint", func(s *remoteSettings) { s.Endpoint = "http://s3.example.test" }, string(goodCredential)},
		{"a block target over the carver's range", func(s *remoteSettings) { s.BlockTarget = 128 * bytesize.MiB }, string(goodCredential)},
		{"trust roots holding no certificate", func(s *remoteSettings) { s.TrustRoots = "hello" }, string(goodCredential)},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := filled()
			if c.settings != nil {
				c.settings(&s)
			}
			cfg, err := clientConfig(s, []byte(c.credential), 1, 1)
			if !errors.Is(err, remote.ErrInvalid) {
				t.Fatalf("clientConfig = %v, want ErrInvalid", err)
			}
			if cfg != (s3.ClientConfig{}) {
				t.Errorf("clientConfig refused and returned %+v, want the zero configuration", cfg)
			}
			if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), testSecret) {
				t.Errorf("clientConfig = %q, which prints the key or the secret", err)
			}
		})
	}
}

// testRoots is the PEM bundle of srv's certificate, starting srv over TLS
// on 127.0.0.1:0 if it has not started.
func testRoots(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	if srv.URL == "" {
		srv.StartTLS()
		t.Cleanup(srv.Close)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}

// discard is a Recorder that keeps nothing.
type discard struct{}

func (discard) Call(string, string, string, time.Duration, int64) {}
func (discard) ConnOpened()                                       {}
func (discard) ForeignKey()                                       {}
func (discard) Capability(string, string, bool)                   {}

// A client built from the configuration clientConfig converts reaches a
// service whose certificate the trust roots hold, and with the system's
// roots in their place fails before the service sees a request. The service
// refuses every request 403, so the open fails either way; what tells the
// two apart is whether a request reached it.
func TestClientConfigTrustRootsReachTheHandshake(t *testing.T) {
	var reached atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	roots := testRoots(t, srv)

	for _, c := range []struct {
		name    string
		roots   string
		reaches bool
	}{
		{"trust roots holding the service's certificate", roots, true},
		{"the system's roots", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			reached.Store(0)
			s := filled()
			s.Endpoint, s.TrustRoots = srv.URL, c.roots
			cfg, err := clientConfig(s, goodCredential, 1, 1)
			if err != nil {
				t.Fatalf("clientConfig: %v", err)
			}
			cl, err := s3.NewClient(cfg, s3.Deps{Logger: slog.New(slog.DiscardHandler), Recorder: discard{}})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			t.Cleanup(func() { _ = cl.Close() })

			_, _, err = s3.Open(t.Context(), cl, "ns/", remote.Mutable)
			if err == nil {
				t.Fatalf("Open = nil, want the service's refusal or the handshake's")
			}
			if got := reached.Load() > 0; got != c.reaches {
				t.Errorf("a request reached the service: %v, want %v (Open: %v)", got, c.reaches, err)
			}
			if untrusted := errors.Is(err, remote.ErrTransient); untrusted == c.reaches {
				t.Errorf("Open = %v: ErrTransient %v, want %v: only a refused handshake is transient here", err, untrusted, !c.reaches)
			}
		})
	}
}

// The block format versions the settings gate admits are exactly the
// versions the codec knows, over every version a namespace can write.
func TestFormatVersionsMatchTheCodec(t *testing.T) {
	r := settings.Versions()[entity.FormatBlock]
	for v := range 256 {
		_, known := codec.MaxHeaderLen(uint8(v))
		if admitted := r.Min <= uint32(v) && uint32(v) <= r.Max; known != admitted {
			t.Errorf("block format version %d: the codec knows it %v, the settings gate admits it %v", v, known, admitted)
		}
	}
}
