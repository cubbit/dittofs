package config

import (
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

// field is one leaf field of a shape as RFC 13's adapter rows state it: its
// mark, and its default (hasDefault false: the node supplies it).
type field struct {
	restart    bool
	def        string
	hasDefault bool
}

// common and rfc13 are copied from RFC 13 Appendix B's adapter rows, plus
// Addrs, which the node supplies (RFC 20 §3.6). Every row is live.
var common = map[string]field{
	"Addrs":             {},
	"DrainDeadline":     {def: "90s", hasDefault: true},
	"IdleTimeout":       {def: "15m", hasDefault: true},
	"WriteDeadline":     {def: "60s", hasDefault: true},
	"HandshakeDeadline": {def: "30s", hasDefault: true},
	"FrameDeadline":     {def: "30s", hasDefault: true},
	"Disabled":          {def: "", hasDefault: true},
}

var rfc13 = map[reflect.Type]map[string]field{
	reflect.TypeFor[NFS](): with(common, map[string]field{
		"TLS.CertFile":   {def: "", hasDefault: true},
		"TLS.KeyFile":    {def: "", hasDefault: true},
		"TLS.ClientCA":   {def: "", hasDefault: true},
		"TLS.MinVersion": {def: "1.3", hasDefault: true},
		"TLS.Mode":       {def: "opportunistic", hasDefault: true},
	}),
	reflect.TypeFor[SMB](): with(common, map[string]field{
		"Dialects.Min": {def: "2.1", hasDefault: true},
		"Dialects.Max": {def: "3.1.1", hasDefault: true},
	}),
}

func with(a, b map[string]field) map[string]field {
	m := map[string]field{}
	for k, v := range a {
		m[k] = v
	}
	for k, v := range b {
		m[k] = v
	}
	return m
}

// leaves lists a shape's leaf fields by path, as restartFields walks them.
func leaves(t reflect.Type, prefix string, out map[string]reflect.StructField) {
	for i := range t.NumField() {
		f := t.Field(i)
		switch {
		case f.Anonymous:
			leaves(f.Type, prefix, out)
		case f.Type.Kind() == reflect.Struct && f.Type.Name() == "":
			leaves(f.Type, prefix+f.Name+".", out)
		default:
			out[prefix+f.Name] = f
		}
	}
}

// Every field's mark and default agree with RFC 13's row, and no field is
// added without a row.
func TestMarksAndDefaultsMatchRFC13(t *testing.T) {
	for typ, rows := range rfc13 {
		got := map[string]reflect.StructField{}
		leaves(typ, "", got)
		for path, f := range got {
			row, ok := rows[path]
			if !ok {
				t.Errorf("%s.%s has no RFC 13 row: add it to the table with its mark and default", typ.Name(), path)
				continue
			}
			tag, marked := f.Tag.Lookup("restart")
			if marked && tag != "true" {
				t.Errorf("%s.%s: restart tag %q; a mark is restart:\"true\" or none", typ.Name(), path, tag)
			}
			if marked != row.restart {
				t.Errorf("%s.%s: restart-only %v, RFC 13 says %v", typ.Name(), path, marked, row.restart)
			}
			def, hasDefault := f.Tag.Lookup("default")
			if hasDefault != row.hasDefault || def != row.def {
				t.Errorf("%s.%s: default %q (tagged %v), RFC 13 says %q (%v)", typ.Name(), path, def, hasDefault, row.def, row.hasDefault)
			}
		}
		for path := range rows {
			if _, ok := got[path]; !ok {
				t.Errorf("%s has no field %s for its RFC 13 row", typ.Name(), path)
			}
		}
	}
}

func validAdapter() Adapter {
	return Adapter{
		Addrs:             []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		DrainDeadline:     90 * time.Second,
		IdleTimeout:       15 * time.Minute,
		WriteDeadline:     60 * time.Second,
		HandshakeDeadline: 30 * time.Second,
		FrameDeadline:     30 * time.Second,
	}
}

func validNFS() NFS {
	s := NFS{Adapter: validAdapter()}
	s.TLS.MinVersion, s.TLS.Mode = "1.3", "opportunistic"
	return s
}

func validSMB() SMB {
	s := SMB{Adapter: validAdapter()}
	s.Dialects.Min, s.Dialects.Max = "2.1", "3.1.1"
	return s
}

// Each invalid value is refused naming its field, never clamped or replaced.
func TestValidateRefusesNamingTheField(t *testing.T) {
	for _, s := range []Settings{validNFS(), validSMB()} {
		if err := Validate(s); err != nil {
			t.Fatalf("Validate(%T) of valid settings: %v", s, err)
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
		{"DrainDeadline", nfs(func(s *NFS) { s.DrainDeadline = 0 })},
		{"IdleTimeout", nfs(func(s *NFS) { s.IdleTimeout = -time.Second })},
		{"WriteDeadline", smb(func(s *SMB) { s.WriteDeadline = 0 })},
		{"HandshakeDeadline", smb(func(s *SMB) { s.HandshakeDeadline = 0 })},
		{"FrameDeadline", nfs(func(s *NFS) { s.FrameDeadline = 0 })},
		{"Disabled[0]", nfs(func(s *NFS) { s.Disabled = []string{""} })},
		{"Disabled[1]", smb(func(s *SMB) { s.Disabled = []string{"READ", "READ"} })},
		{"TLS.Mode", nfs(func(s *NFS) { s.TLS.Mode = "" })},
		{"TLS.MinVersion", nfs(func(s *NFS) { s.TLS.MinVersion = "1.2" })},
		{"TLS.KeyFile", nfs(func(s *NFS) { s.TLS.CertFile = "tls.crt" })},
		{"TLS.ClientCA", nfs(func(s *NFS) { s.TLS.ClientCA = "ca.crt" })},
		{"Dialects.Min", smb(func(s *SMB) { s.Dialects.Min = "2.0.2" })},
		{"Dialects.Max", smb(func(s *SMB) { s.Dialects.Max = "3.2" })},
		{"Dialects.Max", smb(func(s *SMB) { s.Dialects.Min, s.Dialects.Max = "3.1.1", "3.0" })},
	} {
		err := Validate(c.s)
		if err == nil || !strings.Contains(err.Error(), "config: "+c.field+": ") {
			t.Errorf("Validate with a bad %s: %v, want an error naming %s", c.field, err, c.field)
		}
	}
}

// The drain deadline is at least the break deadline (35 s) plus the request
// deadline (30 s).
func TestDrainDeadlineFloor(t *testing.T) {
	s := validNFS()
	s.DrainDeadline = 64 * time.Second
	if err := Validate(s); err == nil || !strings.Contains(err.Error(), "DrainDeadline") {
		t.Errorf("DrainDeadline 64s: %v, want refused naming DrainDeadline", err)
	}
	s.DrainDeadline = 65 * time.Second
	if err := Validate(s); err != nil {
		t.Errorf("DrainDeadline 65s: %v, want accepted", err)
	}
}

// tagged is a test-only shape with a restart-only field, since no adapter
// field is restart-only today.
type tagged struct {
	Adapter
	Nested struct {
		Port int `restart:"true"`
	}
	Live int
}

func (s tagged) common() Adapter { return s.Adapter }

func TestRestartRequiredReadsTheTag(t *testing.T) {
	var old, next tagged
	next.Nested.Port = 1
	err := RestartRequired(old, next)
	if !errors.Is(err, ErrRestartRequired) || !strings.Contains(err.Error(), "Nested.Port") {
		t.Errorf("restart-only Nested.Port changed: %v, want ErrRestartRequired naming Nested.Port", err)
	}
	next = tagged{Live: 1}
	next.DrainDeadline = time.Minute
	if err := RestartRequired(old, next); err != nil {
		t.Errorf("only live fields changed: %v, want nil", err)
	}
	a, b := validNFS(), validNFS()
	b.IdleTimeout, b.TLS.Mode = time.Minute, "require"
	if err := RestartRequired(a, b); err != nil {
		t.Errorf("NFS shapes, every field live: %v, want nil", err)
	}
}

// RestartRequired compares one protocol's shapes; two protocols' are refused,
// and not as a restart, which the node would answer by restarting.
func TestRestartRequiredRefusesTheOtherProtocol(t *testing.T) {
	err := RestartRequired(validNFS(), validSMB())
	if err == nil || errors.Is(err, ErrRestartRequired) {
		t.Errorf("NFS against SMB: %v, want an error that is not ErrRestartRequired", err)
	}
}

func TestAdapterOf(t *testing.T) {
	s := validSMB()
	s.IdleTimeout = time.Hour
	if got := AdapterOf(s); got.IdleTimeout != time.Hour {
		t.Errorf("AdapterOf: IdleTimeout %s, want 1h", got.IdleTimeout)
	}
}
