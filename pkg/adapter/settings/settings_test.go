package settings

import (
	"encoding/json"
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
	nfsCommon, smbCommon := common, common
	nfsCommon.Port, smbCommon.Port = 2049, 445
	wantNFS := NFS{Adapter: nfsCommon, TLS: TLS{Mode: "off"}, Versions: VersionRange{Min: "3", Max: "4.2"}}
	wantSMB := SMB{
		Adapter:        smbCommon,
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
// the node supplies, has no key (see TestNoRecordCarriesAddrs).
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
			if f.Name != "Addrs" && (!ok || tag == "" || tag == "-") {
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
		name, field string
		s           Settings
	}{
		{"nfs zero address", "Addrs[0]", nfs(func(s *NFS) { s.Addrs = []netip.Addr{{}} })},
		{"nfs duplicate address", "Addrs[1]", nfs(func(s *NFS) { s.Addrs = append(s.Addrs, s.Addrs[0]) })},
		{"nfs IPv4-mapped duplicate", "Addrs[1]", nfs(func(s *NFS) { s.Addrs = append(s.Addrs, netip.MustParseAddr("::ffff:127.0.0.1")) })},
		{"smb unspecified IPv4 beside a specific address", "Addrs[1]", smb(func(s *SMB) { s.Addrs = append(s.Addrs, netip.IPv4Unspecified()) })},
		{"smb specific address beside unspecified IPv6", "Addrs[1]", smb(func(s *SMB) {
			s.Addrs = []netip.Addr{netip.IPv6Unspecified(), netip.MustParseAddr("fd00::1")}
		})},
		{"nfs zero drain", "DrainTimeout", nfs(func(s *NFS) { s.DrainTimeout = 0 })},
		{"nfs negative idle", "IdleTimeout", nfs(func(s *NFS) { s.IdleTimeout = -time.Second })},
		{"smb zero write", "WriteTimeout", smb(func(s *SMB) { s.WriteTimeout = 0 })},
		{"smb zero handshake", "HandshakeTimeout", smb(func(s *SMB) { s.HandshakeTimeout = 0 })},
		{"nfs zero read", "ReadTimeout", nfs(func(s *NFS) { s.ReadTimeout = 0 })},
		{"nfs empty disabled op", "DisabledOperations[0]", nfs(func(s *NFS) { s.DisabledOperations = []string{""} })},
		{"smb repeated disabled op", "DisabledOperations[1]", smb(func(s *SMB) { s.DisabledOperations = []string{"READ", "READ"} })},
		{"nfs empty TLS mode", "TLS.Mode", nfs(func(s *NFS) { s.TLS.Mode = "" })},
		{"nfs cert with TLS off", "TLS.CertFile", nfs(func(s *NFS) { s.TLS.CertFile = "tls.crt" })},
		{"nfs key with TLS off", "TLS.KeyFile", nfs(func(s *NFS) { s.TLS.KeyFile = "tls.key" })},
		{"nfs CA with TLS off", "TLS.ClientCA", nfs(func(s *NFS) { s.TLS.ClientCA = "ca.crt" })},
		{"nfs require without cert", "TLS.CertFile", nfs(func(s *NFS) { s.TLS.Mode = "require" })},
		{"nfs opportunistic without key", "TLS.KeyFile", nfs(func(s *NFS) { s.TLS.Mode, s.TLS.CertFile = "opportunistic", "tls.crt" })},
		{"smb zero pending", "PendingTimeout", smb(func(s *SMB) { s.PendingTimeout = 0 })},
		{"smb recall too long", "RecallTimeout", smb(func(s *SMB) { s.RecallTimeout = 35 * time.Second })},
		{"nfs version 2", "Versions.Min", nfs(func(s *NFS) { s.Versions.Min = "2" })},
		{"nfs version 4.3", "Versions.Max", nfs(func(s *NFS) { s.Versions.Max = "4.3" })},
		{"nfs versions inverted", "Versions.Max", nfs(func(s *NFS) { s.Versions = VersionRange{Min: "4.1", Max: "4.0"} })},
		{"smb dialect 2.0.2", "Dialects.Min", smb(func(s *SMB) { s.Dialects.Min = "2.0.2" })},
		{"smb dialect 3.2", "Dialects.Max", smb(func(s *SMB) { s.Dialects.Max = "3.2" })},
		{"smb dialects inverted", "Dialects.Max", smb(func(s *SMB) { s.Dialects.Min, s.Dialects.Max = "3.1.1", "3.0" })},
	} {
		err := c.s.Validate()
		if err == nil || !strings.Contains(err.Error(), "settings: "+c.field+": ") {
			t.Errorf("%s: Validate: %v, want an error naming %s", c.name, err, c.field)
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

// Every range of known versions in order is accepted, single-version ranges
// included.
func TestVersionRangesAccepted(t *testing.T) {
	for _, v := range []VersionRange{{"3", "3"}, {"4.0", "4.2"}, {"4.1", "4.1"}} {
		n := validNFS()
		n.Versions = v
		if err := n.Validate(); err != nil {
			t.Errorf("NFS %s..%s: %v, want accepted", v.Min, v.Max, err)
		}
	}
	s := validSMB()
	s.Dialects = VersionRange{Min: "3.0.2", Max: "3.0.2"}
	if err := s.Validate(); err != nil {
		t.Errorf("SMB 3.0.2..3.0.2: %v, want accepted", err)
	}
}

// decode decodes a record over dst as the loader does: over the defaults,
// refusing a key the shape does not name.
//
// ponytail: encoding/json stands in for the node's loader, which does not
// exist yet; once it does, these cases run through it instead.
func decode(record string, dst any) error {
	d := json.NewDecoder(strings.NewReader(record))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}

// A record decoded over the defaults changes only what it names, down to one
// field of a nested range.
func TestDecodeOverDefaultsKeepsWhatTheRecordOmits(t *testing.T) {
	got := DefaultSMB()
	if err := decode(`{"dialects": {"max": "3.0"}}`, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := DefaultSMB()
	want.Dialects.Max = "3.0"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded %+v, want %+v", got, want)
	}
}

// Addrs is the node's: no record carries it, under any key. A record naming
// addrs is refused as an unknown field, and settings holding addresses
// encode to a record that sets none.
func TestNoRecordCarriesAddrs(t *testing.T) {
	s := DefaultNFS()
	if err := decode(`{"addrs": ["127.0.0.1"]}`, &s); err == nil || !strings.Contains(err.Error(), `unknown field "addrs"`) {
		t.Errorf("decode of a record naming addrs: %v, want refused as an unknown field", err)
	}
	record, err := json.Marshal(validNFS())
	if err != nil {
		t.Fatal(err)
	}
	s = DefaultNFS()
	if err := decode(string(record), &s); err != nil || s.Addrs != nil {
		t.Errorf("decode of %s: Addrs = %v, err %v; want no addresses and no error", record, s.Addrs, err)
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

// pkg/adapter/settings is a leaf: it imports only the standard library.
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
				t.Errorf("%s imports %s; pkg/adapter/settings imports only the standard library", f, p)
			}
		}
	}
}
