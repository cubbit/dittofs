package config

import (
	"go/parser"
	"go/token"
	"net/netip"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The defaults are RFC 13 Appendix B's adapter rows, and they validate.
func TestDefaultsAreRFC13s(t *testing.T) {
	common := Adapter{
		DrainTimeout:     90 * time.Second,
		IdleTimeout:      15 * time.Minute,
		WriteTimeout:     60 * time.Second,
		HandshakeTimeout: 30 * time.Second,
		ReadTimeout:      30 * time.Second,
	}
	wantNFS := NFS{Adapter: common, TLS: TLS{Mode: "off"}, Versions: VersionRange{Min: "3", Max: "4.2"}}
	wantSMB := SMB{
		Adapter:        common,
		Dialects:       VersionRange{Min: "2.1", Max: "3.1.1"},
		PendingTimeout: 10 * time.Minute,
		RecallTimeout:  60 * time.Second,
	}
	if got := DefaultNFS(); !reflect.DeepEqual(got, wantNFS) {
		t.Errorf("DefaultNFS() = %+v, want %+v", got, wantNFS)
	}
	if got := DefaultSMB(); !reflect.DeepEqual(got, wantSMB) {
		t.Errorf("DefaultSMB() = %+v, want %+v", got, wantSMB)
	}
	if err := DefaultNFS().Validate(); err != nil {
		t.Errorf("DefaultNFS().Validate(): %v", err)
	}
	if err := DefaultSMB().Validate(); err != nil {
		t.Errorf("DefaultSMB().Validate(): %v", err)
	}
}

// Every field read from a record names its key in a json tag; Addrs, which
// the node supplies, is "-".
func TestEveryFieldNamesItsKey(t *testing.T) {
	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		for i := range typ.NumField() {
			f := typ.Field(i)
			if f.Anonymous {
				walk(f.Type, path)
				continue
			}
			tag, ok := f.Tag.Lookup("json")
			switch {
			case f.Name == "Addrs":
				if tag != "-" {
					t.Errorf("%sAddrs: json tag %q, want \"-\": the node supplies it", path, tag)
				}
			case !ok || tag == "" || tag == "-":
				t.Errorf("%s%s has no json key", path, f.Name)
			}
			if f.Type.Kind() == reflect.Struct && f.Type.PkgPath() == typ.PkgPath() {
				walk(f.Type, path+f.Name+".")
			}
		}
	}
	walk(reflect.TypeFor[NFS](), "NFS.")
	walk(reflect.TypeFor[SMB](), "SMB.")
}

func validNFS() NFS {
	s := DefaultNFS()
	s.Addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	return s
}

func validSMB() SMB {
	s := DefaultSMB()
	s.Addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	return s
}

// Each invalid value is refused naming its field, never clamped or replaced.
func TestValidateRefusesNamingTheField(t *testing.T) {
	for _, s := range []Settings{validNFS(), validSMB()} {
		if err := s.Validate(); err != nil {
			t.Fatalf("%T.Validate() of valid settings: %v", s, err)
		}
	}
	nfs := func(f func(*NFS)) Settings { s := validNFS(); f(&s); return s }
	smb := func(f func(*SMB)) Settings { s := validSMB(); f(&s); return s }
	for _, c := range []struct {
		field string
		s     Settings
	}{
		{"Addrs[0]", nfs(func(s *NFS) { s.Addrs = []netip.Addr{{}} })},
		{"Addrs[1]", nfs(func(s *NFS) { s.Addrs = append(s.Addrs, s.Addrs[0]) })},
		{"Addrs[1]", nfs(func(s *NFS) { s.Addrs = append(s.Addrs, netip.MustParseAddr("::ffff:127.0.0.1")) })},
		{"Addrs[1]", smb(func(s *SMB) { s.Addrs = append(s.Addrs, netip.IPv4Unspecified()) })},
		{"Addrs[1]", smb(func(s *SMB) {
			s.Addrs = []netip.Addr{netip.IPv6Unspecified(), netip.MustParseAddr("fd00::1")}
		})},
		{"DrainTimeout", nfs(func(s *NFS) { s.DrainTimeout = 0 })},
		{"IdleTimeout", nfs(func(s *NFS) { s.IdleTimeout = -time.Second })},
		{"WriteTimeout", smb(func(s *SMB) { s.WriteTimeout = 0 })},
		{"HandshakeTimeout", smb(func(s *SMB) { s.HandshakeTimeout = 0 })},
		{"ReadTimeout", nfs(func(s *NFS) { s.ReadTimeout = 0 })},
		{"DisabledOperations[0]", nfs(func(s *NFS) { s.DisabledOperations = []string{""} })},
		{"DisabledOperations[1]", smb(func(s *SMB) { s.DisabledOperations = []string{"READ", "READ"} })},
		{"TLS.Mode", nfs(func(s *NFS) { s.TLS.Mode = "" })},
		{"TLS.CertFile", nfs(func(s *NFS) { s.TLS.CertFile = "tls.crt" })},
		{"TLS.KeyFile", nfs(func(s *NFS) { s.TLS.KeyFile = "tls.key" })},
		{"TLS.ClientCA", nfs(func(s *NFS) { s.TLS.ClientCA = "ca.crt" })},
		{"TLS.CertFile", nfs(func(s *NFS) { s.TLS.Mode = "require" })},
		{"TLS.KeyFile", nfs(func(s *NFS) { s.TLS.Mode, s.TLS.CertFile = "opportunistic", "tls.crt" })},
		{"PendingTimeout", smb(func(s *SMB) { s.PendingTimeout = 0 })},
		{"RecallTimeout", smb(func(s *SMB) { s.RecallTimeout = 35 * time.Second })},
		{"Versions.Min", nfs(func(s *NFS) { s.Versions.Min = "2" })},
		{"Versions.Max", nfs(func(s *NFS) { s.Versions.Max = "4.3" })},
		{"Versions.Max", nfs(func(s *NFS) { s.Versions = VersionRange{Min: "4.1", Max: "4.0"} })},
		{"Dialects.Min", smb(func(s *SMB) { s.Dialects.Min = "2.0.2" })},
		{"Dialects.Max", smb(func(s *SMB) { s.Dialects.Max = "3.2" })},
		{"Dialects.Max", smb(func(s *SMB) { s.Dialects.Min, s.Dialects.Max = "3.1.1", "3.0" })},
	} {
		err := c.s.Validate()
		if err == nil || !strings.Contains(err.Error(), "config: "+c.field+": ") {
			t.Errorf("Validate with a bad %s: %v, want an error naming %s", c.field, err, c.field)
		}
	}
}

// Validate checks TLS's shape, not its files: opportunistic or require with a
// certificate and key is accepted, whether or not the files exist.
func TestTLSModesWithFilesAccepted(t *testing.T) {
	for _, mode := range []string{"opportunistic", "require"} {
		s := validNFS()
		s.TLS = TLS{Mode: mode, CertFile: "missing.crt", KeyFile: "missing.key", ClientCA: "missing-ca.crt"}
		if err := s.Validate(); err != nil {
			t.Errorf("Mode %s with a certificate and key: %v, want accepted", mode, err)
		}
	}
}

// A range of one version is accepted.
func TestSingleVersionRange(t *testing.T) {
	n := validNFS()
	n.Versions = VersionRange{Min: "4.1", Max: "4.1"}
	if err := n.Validate(); err != nil {
		t.Errorf("NFS 4.1..4.1: %v, want accepted", err)
	}
	s := validSMB()
	s.Dialects = VersionRange{Min: "3.0.2", Max: "3.0.2"}
	if err := s.Validate(); err != nil {
		t.Errorf("SMB 3.0.2..3.0.2: %v, want accepted", err)
	}
}

// The drain timeout is at least the break deadline (35 s) plus the request
// deadline (30 s).
func TestDrainTimeoutFloor(t *testing.T) {
	s := validNFS()
	s.DrainTimeout = 64 * time.Second
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "DrainTimeout") {
		t.Errorf("DrainTimeout 64s: %v, want refused naming DrainTimeout", err)
	}
	s.DrainTimeout = 65 * time.Second
	if err := s.Validate(); err != nil {
		t.Errorf("DrainTimeout 65s: %v, want accepted", err)
	}
}

// pkg/adapter/config is a leaf: it imports only the standard library.
func TestImportsOnlyTheStandardLibrary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(strings.Split(p, "/")[0], ".") {
				t.Errorf("%s imports %s; pkg/adapter/config imports only the standard library", f, p)
			}
		}
	}
}
