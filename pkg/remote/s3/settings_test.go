package s3

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

// testCert is a PEM CERTIFICATE block of a self-signed certificate, built
// from a fixed key seed.
func testCert(t *testing.T) string {
	t.Helper()
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test root"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(0, 0).Add(100 * 365 * 24 * time.Hour),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// good is a Settings every rule accepts.
func good() Settings {
	return Settings{Endpoint: "https://s3.example.com", Region: "us-east-1", Bucket: "b", StorageClass: "STANDARD"}
}

// Validate, the call the control plane makes, refuses each broken field
// beside an accepted neighbour, naming the field and the rule.
func TestSettingsValidate(t *testing.T) {
	cert := testCert(t)
	// corrupted is cert with one base64 character of its body replaced by a
	// character outside the alphabet, so pem.Decode drops the block.
	lines := strings.Split(cert, "\n")
	lines[1] = "*" + lines[1][1:]
	corrupted := strings.Join(lines, "\n")
	// badDER is a well-formed PEM CERTIFICATE block whose bytes are no
	// certificate.
	badDER := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a certificate")}))
	key := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("k")}))

	cases := []struct {
		name string
		edit func(*Settings)
		want []string // substrings of the error; nil is accepted
	}{
		{"defaults", func(*Settings) {}, nil},
		{"https with a port and a path", func(s *Settings) { s.Endpoint = "https://s3.example.com:9000/x" }, nil},
		{"http endpoint", func(s *Settings) { s.Endpoint = "http://s3.example.com" }, []string{"endpoint", "not https"}},
		{"no scheme", func(s *Settings) { s.Endpoint = "s3.example.com" }, []string{"endpoint", "not https"}},
		{"host-less endpoint", func(s *Settings) { s.Endpoint = "https://" }, []string{"endpoint", "no host"}},
		{"endpoint with user info", func(s *Settings) { s.Endpoint = "https://user:pw@s3.example.com" }, []string{"endpoint", "user info"}},
		{"port only endpoint", func(s *Settings) { s.Endpoint = "https://:443" }, []string{"endpoint", "no host"}},
		{"unparsable endpoint", func(s *Settings) { s.Endpoint = "https://exa mple.com\x7f" }, []string{"endpoint", "not a URL"}},
		{"empty endpoint", func(s *Settings) { s.Endpoint = "" }, []string{"endpoint", "not https"}},
		{"other region", func(s *Settings) { s.Region = "eu-west-1" }, nil},
		{"empty region", func(s *Settings) { s.Region = "" }, []string{"region", "empty"}},
		{"empty bucket", func(s *Settings) { s.Bucket = "" }, []string{"bucket", "empty"}},
		{"empty storage class", func(s *Settings) { s.StorageClass = "" }, []string{"storage_class", "empty"}},
		{"lower-case class", func(s *Settings) { s.StorageClass = "standard" }, []string{"storage_class", "no restore"}},
		{"one trust root", func(s *Settings) { s.TrustRoots = cert }, nil},
		{"trust root with CRLF line ends", func(s *Settings) { s.TrustRoots = strings.ReplaceAll(cert, "\n", "\r\n") }, nil},
		{"two trust roots with whitespace", func(s *Settings) { s.TrustRoots = "\n" + cert + "\n\n" + cert + "  \n" }, nil},
		{"trust roots of whitespace", func(s *Settings) { s.TrustRoots = " \n" }, []string{"trust_roots", "no certificate"}},
		{"trust roots of plain text", func(s *Settings) { s.TrustRoots = "hello" }, []string{"trust_roots", "before the first block"}},
		{"good root beside a corrupted one", func(s *Settings) { s.TrustRoots = cert + corrupted }, []string{"trust_roots", "block 2", "well-formed"}},
		{"corrupted root before a good one", func(s *Settings) { s.TrustRoots = corrupted + cert }, []string{"trust_roots", "block 1", "well-formed"}},
		{"good root beside one that does not parse", func(s *Settings) { s.TrustRoots = cert + badDER }, []string{"trust_roots", "block 2", "parses"}},
		{"a block that is not a certificate", func(s *Settings) { s.TrustRoots = cert + key }, []string{"trust_roots", "block 2", `"PRIVATE KEY"`}},
		{"text left over", func(s *Settings) { s.TrustRoots = cert + "trailing" }, []string{"trust_roots", "after block 1"}},
		{"text between blocks", func(s *Settings) { s.TrustRoots = cert + "x\n" + cert }, []string{"trust_roots", "after block 1"}},
		{"unterminated block", func(s *Settings) { s.TrustRoots = strings.TrimSuffix(cert, "-----END CERTIFICATE-----\n") }, []string{"trust_roots", "block 1", "well-formed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := good()
			c.edit(&s)
			err := s.Validate()
			if c.want == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error naming %q", c.want)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("Validate() = %q, want it to name %q", err, w)
				}
			}
		})
	}
}

// Every class of RFC 4 C.1's list is accepted; the archive classes and a
// class on no list are refused.
func TestSettingsValidateStorageClass(t *testing.T) {
	for _, class := range []string{"STANDARD", "REDUCED_REDUNDANCY", "STANDARD_IA", "ONEZONE_IA",
		"INTELLIGENT_TIERING", "GLACIER_IR", "EXPRESS_ONEZONE"} {
		s := good()
		s.StorageClass = class
		if err := s.Validate(); err != nil {
			t.Errorf("Validate() with storage class %q = %v, want nil", class, err)
		}
	}
	for _, class := range []string{"GLACIER", "DEEP_ARCHIVE", "OUTPOSTS"} {
		s := good()
		s.StorageClass = class
		err := s.Validate()
		if err == nil || !strings.Contains(err.Error(), "storage_class") || !strings.Contains(err.Error(), class) ||
			!strings.Contains(err.Error(), "no restore") {
			t.Errorf("Validate() with storage class %q = %v, want a refusal naming storage_class, the class and the rule", class, err)
		}
	}
}

// No refusal of the endpoint prints it, since url.Parse's error would.
func TestSettingsValidateEndpointNotPrinted(t *testing.T) {
	for _, endpoint := range []string{"https://user:hunter2@host", "https://hunter2\x7f", "https://:hunter2@"} {
		s := good()
		s.Endpoint = endpoint
		err := s.Validate()
		if err == nil || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("Validate() with endpoint %q = %v, want a refusal that does not print it", endpoint, err)
		}
	}
}

// No refusal of the trust roots prints the PEM text.
func TestSettingsValidateTrustRootsNotPrinted(t *testing.T) {
	cert := testCert(t)
	body := strings.Split(cert, "\n")[1]
	badDER := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a certificate")}))
	for _, roots := range []string{cert + "x" + body, cert + badDER, body, strings.Replace(cert, "CERTIFICATE", "X"+body, 1)} {
		s := good()
		s.TrustRoots = roots
		err := s.Validate()
		if err == nil {
			t.Errorf("Validate() with trust roots %q = nil, want a refusal", roots)
			continue
		}
		for _, line := range strings.Split(roots, "\n") {
			if len(line) > 8 && !strings.HasPrefix(line, "-----") && strings.Contains(err.Error(), line) {
				t.Errorf("Validate() = %q, want it not to print the trust roots' line %q", err, line)
			}
		}
	}
}

func TestNamespaceSettingsValidate(t *testing.T) {
	cases := []struct {
		prefix string
		want   string // a substring of the error; "" is accepted
	}{
		{"", `prefix: does not end in "/"`},
		{"ns", `prefix: does not end in "/"`},
		{"ns/", ""},
		{"a/b/", ""},
	}
	for _, c := range cases {
		t.Run(c.prefix, func(t *testing.T) {
			err := NamespaceSettings{Prefix: c.prefix}.Validate()
			switch {
			case c.want == "" && err != nil:
				t.Errorf("Validate() with prefix %q = %v, want nil", c.prefix, err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("Validate() with prefix %q = %v, want an error naming %q", c.prefix, err, c.want)
			}
		})
	}
}
