package settings

import (
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cubbit/dittofs/internal/bytesize"
	"github.com/cubbit/dittofs/internal/prof"
	adaptersettings "github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The settings structs the decode tests declare sections over.
type (
	decSMB struct {
		adaptersettings.SMB
		Server string `json:"server" class:"live"`
	}
	decCache struct {
		valid
		Size bytesize.ByteSize `json:"size" class:"live"`
	}
	decOIDC struct {
		valid
		ClientSecret entity.SecretRef `json:"client_secret" class:"live"`
	}
	decChan struct {
		valid
		Port  uint16   `json:"port"  class:"live"`
		Extra chan int `json:"extra" class:"live"`
	}
	decMapKey struct {
		valid
		Counts map[int]string `json:"counts" class:"live"`
	}
	decAny struct {
		valid
		Port  uint16 `json:"port"  class:"live"`
		Extra any    `json:"extra" class:"live"`
	}
	// decEmbedTarget is embedded under a json name by decEmbedNamed, as one
	// key whose value is an object.
	decEmbedTarget struct {
		A string `json:"a"`
	}
	decEmbedNamed struct {
		valid
		decEmbedTarget `json:"x" class:"live"`
	}
	// decShared holds a pointer, in a type of its own that is not exported.
	decShared struct {
		Limit *int `json:"limit" class:"live"`
	}
	decSharedOuter struct {
		valid
		decShared
	}
	// decEmpty declares no key.
	decEmpty struct {
		valid
	}
	// decInner names its field as Go does, with no json tag.
	decInner struct {
		Plain string
	}
	// decKinds holds one field of each kind the decode reads, the section
	// the fuzz target decodes into.
	decKinds struct {
		valid
		TLS       adaptersettings.TLS            `json:"tls"       class:"live"`
		Name      string                         `json:"name"      class:"live"`
		List      []string                       `json:"list"      class:"live"`
		Labels    map[string]string              `json:"labels"    class:"live"`
		Limit     *int                           `json:"limit"     class:"live"`
		Waits     []time.Duration                `json:"waits"     class:"live"`
		Deadlines map[string]time.Duration       `json:"deadlines" class:"live"`
		Ranges    []adaptersettings.VersionRange `json:"ranges"    class:"live"`
		Timeout   time.Duration                  `json:"timeout"   class:"live"`
		Size      bytesize.ByteSize              `json:"size"      class:"live"`
		Enabled   bool                           `json:"enabled"   class:"live"`
		Inner     decInner                       `json:"inner"     class:"live"`
		Ratio     float64                        `json:"ratio"     class:"live"`
		Proxy     *adaptersettings.VersionRange  `json:"proxy"     class:"live"`
		_         int
	}
)

// escapesJSON is a JSON string holding every escape JSON has, a character
// of two UTF-8 bytes, one of four, and the same one as a surrogate pair.
const escapesJSON = `"a\"b\\c\/d\be\ff\ng\rh\tié😀\ud83d\ude00"`

// indentedJSON is an object with every kind of JSON whitespace between its
// tokens.
const indentedJSON = "{\r\n\t\"mode\" :\t\"require\",\n\t\"cert_file\": \"c\"\n}\n"

// nodeScope is the node scope RFC 13 §6's example prints, its ID whole.
var nodeScope = entity.SettingScope{
	Kind: entity.ScopeNode,
	ID:   [16]byte{0: 0x3f, 1: 0x2a, 15: 0xc1},
}

// nodeLine is how every refusal at nodeScope starts.
const nodeLine = "setting (node 3f2a00000000000000000000000000c1): "

// marker is a value that must never reach a refusal's text.
const marker = "s3cr3t-marker"

func smbSection() Section {
	def := decSMB{
		SMB: adaptersettings.DefaultSMB(),
	}
	def.Port = 445
	def.IdleTimeout = time.Minute
	def.DisabledOperations = []string{"a", "b"}

	return Of("adapters.smb", At(entity.ScopeNode), def)
}

func nfsSection() Section {
	def := adaptersettings.DefaultNFS()
	def.TLS = adaptersettings.TLS{
		Mode:     "off",
		CertFile: "cert.pem",
	}

	return Of("adapters.nfs", At(entity.ScopeNode), def)
}

func cacheSection() Section {
	return Of("storage.cache", At(entity.ScopeNode), decCache{})
}

func oidcSection() Section {
	return Of("identity.oidc", At(entity.ScopeNode), decOIDC{},
		SecretRef("ClientSecret", entity.SecretPassword))
}

func kindsDefault() decKinds {
	limit := 7

	return decKinds{
		TLS: adaptersettings.TLS{
			Mode:     "off",
			CertFile: "cert.pem",
		},
		Name: "n",
		Proxy: &adaptersettings.VersionRange{
			Min: "3",
			Max: "4.2",
		},
		Enabled: true,
		List:    []string{"a"},
		Labels:  map[string]string{"a": "1"},
		Limit:   &limit,
		Waits:   []time.Duration{time.Second},
		Timeout: time.Minute,
		Size:    bytesize.ByteSize(512),
	}
}

func kindsSection() Section {
	return Of("kinds", At(entity.ScopeNode), kindsDefault())
}

// decodeRow decodes one row of key and value at scope over sec's default.
func decodeRow[T any](sec Section, scope entity.SettingScope, key, value string) (T, error) {
	rows := []entity.Setting{
		{
			Scope: scope,
			Key:   key,
			Value: []byte(value),
		},
	}

	return decode(&sec, sec.def.(T), scope, rows)
}

// refusal is one refused row and the whole line it must print.
type refusalCase struct {
	key   string
	value string
	line  string
}

// checkRefused decodes each case over sec at nodeScope and asserts the
// whole line, kind and that no marker reaches it.
func checkRefused[T any](t *testing.T, sec Section, kind error, cases []refusalCase) {
	t.Helper()

	for _, tc := range cases {
		_, err := decodeRow[T](sec, nodeScope, tc.key, tc.value)
		if err == nil {
			t.Errorf("%s = %q: accepted, want refused with %q", tc.key, tc.value, tc.line)

			continue
		}

		if got := err.Error(); got != nodeLine+tc.line {
			t.Errorf("%s = %q: line %q, want %q", tc.key, tc.value, got, nodeLine+tc.line)
		}

		if !errors.Is(err, kind) {
			t.Errorf("%s = %q: errors.Is(%v) false for %v", tc.key, tc.value, kind, err)
		}

		if strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%s = %q: line %q prints the marker", tc.key, tc.value, err)
		}

		for _, frag := range []string{"json", "invalid character", "unexpected end"} {
			if strings.Contains(err.Error(), frag) {
				t.Errorf("%s = %q: line %q carries decoder text %q", tc.key, tc.value, err, frag)
			}
		}
	}
}

// A size decodes by its JSON token: a string by the file's form, a number
// by the record's exact count of bytes, each refusal naming its cause and
// printing the value refused.
func TestDecodeSize(t *testing.T) {
	prof.Watch(t, prof.Options{})

	sec := cacheSection()

	for _, tc := range []struct {
		value string
		want  bytesize.ByteSize
	}{
		{`"512MiB"`, 536870912},
		{`"1B"`, 1},
		{`"1KB"`, 1000},
		{`"1MB"`, 1000000},
		{`"1GB"`, 1000000000},
		{`"1TB"`, 1000000000000},
		{`"1KiB"`, 1024},
		{`"1MiB"`, 1048576},
		{`"1GiB"`, 1073741824},
		{`"1TiB"`, 1099511627776},
		{`"512B"`, 512},
		{`"18446744073709551615B"`, 18446744073709551615},
		{`"17179869183GiB"`, 18446744072635809792},
		{`512`, 512},
		{`9007199254740993`, 9007199254740993},
		{`9223372036854775808`, 9223372036854775808},
		{`18446744073709551615`, 18446744073709551615},
	} {
		got, err := decodeRow[decCache](sec, nodeScope, "storage.cache.size", tc.value)
		if err != nil {
			t.Errorf("size %s: %v, want %d", tc.value, err, tc.want)

			continue
		}

		if got.Size != tc.want {
			t.Errorf("size %s = %d, want %d", tc.value, got.Size, tc.want)
		}
	}

	const (
		notANumber = "storage.cache.size: must start with a number, got "
		noUnit     = "storage.cache.size: must end with a unit, got "
		unknown    = "storage.cache.size: must end with a unit as Appendix B lists it, got "
		fraction   = "storage.cache.size: must be a whole number, got "
		overflow   = "storage.cache.size: must fit 18446744073709551615 bytes, got "
	)

	var cases []refusalCase

	for _, tc := range []struct {
		value string
		rule  string
	}{
		{`"MiB"`, notANumber},
		{`" 512MiB"`, notANumber},
		{`""`, notANumber},
		{`"-1"`, notANumber},
		{`-1`, notANumber},
		{`1e3`, notANumber},
		{`"512"`, noUnit},
		{`"100"`, noUnit},
		{`"1.5GiB"`, fraction},
		{`"4.1GB"`, fraction},
		{`"0.1KiB"`, fraction},
		{`1.5`, fraction},
		{`1.0`, fraction},
		{`1.5e3`, fraction},
		{`"1Gi"`, unknown},
		{`"1Mi"`, unknown},
		{`"10m"`, unknown},
		{`"1g"`, unknown},
		{`"100mib"`, unknown},
		{`"1kb"`, unknown},
		{`"1kib"`, unknown},
		{`"16EiB"`, unknown},
		{`"1 MiB"`, unknown},
		{`"512MiB "`, unknown},
		{`"1e3"`, unknown},
		{"\"1KiB\"", unknown},
		{`"17179869184GiB"`, overflow},
		{`"18446744073709551616B"`, overflow},
		{`"18446744073709551615TB"`, overflow},
		{`"18446744073709551616"`, overflow},
		{`"18446744073709551616KB"`, overflow},
		{`"18446744073709551616kb"`, overflow},
		{`18446744073709551616`, overflow},
	} {
		cases = append(cases, refusalCase{
			key:   "storage.cache.size",
			value: tc.value,
			line:  tc.rule + tc.value,
		})
	}

	checkRefused[decCache](t, sec, ErrInvalid, cases)
}

// A duration decodes from its string form only, by Appendix B's grammar,
// whole and never negative, its sum within time.Duration.
func TestDecodeDuration(t *testing.T) {
	prof.Watch(t, prof.Options{})

	sec := smbSection()

	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{`"250ms"`, 250 * time.Millisecond},
		{`"30s"`, 30 * time.Second},
		{`"5m"`, 5 * time.Minute},
		{`"1h"`, time.Hour},
		{`"1h30m"`, 90 * time.Minute},
		{`"0s"`, 0},
		{`"1500ms"`, 1500 * time.Millisecond},
		{`"90s"`, 90 * time.Second},
		{`"1s1h"`, 3601 * time.Second},
		{`"2562047h"`, 9223369200000000000},
		{`"2562047h47m16s"`, 9223372036000000000},
	} {
		got, err := decodeRow[decSMB](sec, nodeScope, "adapters.smb.idle_timeout", tc.value)
		if err != nil {
			t.Errorf("duration %s: %v, want %d", tc.value, err, tc.want)

			continue
		}

		if got.IdleTimeout != tc.want {
			t.Errorf("duration %s = %d, want %d", tc.value, got.IdleTimeout, tc.want)
		}
	}

	const (
		bare       = "adapters.smb.idle_timeout: must be a duration with a unit, got "
		noUnit     = "adapters.smb.idle_timeout: must end with a unit, got "
		notANumber = "adapters.smb.idle_timeout: must start with a number, got "
		fraction   = "adapters.smb.idle_timeout: must be a whole number, got "
		unknown    = "adapters.smb.idle_timeout: must end with ms, s, m or h in lower case, got "
		overflow   = "adapters.smb.idle_timeout: must fit 9223372036854775807 nanoseconds, got "
	)

	var cases []refusalCase

	for _, tc := range []struct {
		value string
		rule  string
	}{
		{`30`, bare},
		{`0`, bare},
		{`"30"`, noUnit},
		{`"0"`, noUnit},
		{`"30s30"`, noUnit},
		{`""`, notANumber},
		{`" 30s "`, notANumber},
		{`"-5s"`, notANumber},
		{`"+5s"`, notANumber},
		{`"1.5s"`, fraction},
		{`"30S"`, unknown},
		{"\"5µs\"", unknown},
		{"\"5μs\"", unknown},
		{`"5ns"`, unknown},
		{`"5us"`, unknown},
		{`"1d"`, unknown},
		{`"2562048h"`, overflow},
		{`"2562047h47m17s"`, overflow},
		{`"2562047h2562047h"`, overflow},
	} {
		cases = append(cases, refusalCase{
			key:   "adapters.smb.idle_timeout",
			value: tc.value,
			line:  tc.rule + tc.value,
		})
	}

	checkRefused[decSMB](t, sec, ErrInvalid, cases)
}

// A row's key names a declared field exactly as declared; any other is
// refused naming the closest declared key, the refused key escaped and its
// value never printed.
func TestDecodeKeys(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const hintPort = "; did you mean adapters.smb.port?"

	embedded, err := decodeRow[decEmbedNamed](Of("emb", At(entity.ScopeNode), decEmbedNamed{}),
		nodeScope, "emb.x", `{"a":"y"}`)
	if err != nil || embedded.A != "y" {
		t.Errorf("embedded struct under a json name: got %q, %v, want y", embedded.A, err)
	}

	checkRefused[decEmpty](t, Of("empty", At(entity.ScopeNode), decEmpty{}), ErrUnknownKey,
		[]refusalCase{
			{"empty.x", `1`, `"empty.x": unknown key`},
		})

	checkRefused[decKinds](t, kindsSection(), ErrUnknownKey, []refusalCase{
		{"kinds.__", `1`, `"kinds.__": unknown key; did you mean kinds.tls?`},
		{"kinds._", `1`, `"kinds._": unknown key; did you mean kinds.tls?`},
	})

	checkRefused[decSMB](t, smbSection(), ErrUnknownKey, []refusalCase{
		{"adapters.smb.Port", `1`, `"adapters.smb.Port": unknown key` + hintPort},
		{
			"adapters.smb.WRITE_TIMEOUT",
			`1`,
			`"adapters.smb.WRITE_TIMEOUT": unknown key; did you mean adapters.smb.write_timeout?`,
		},
		{"adapters.smb.prot", `"` + marker + `"`, `"adapters.smb.prot": unknown key` + hintPort},
		{
			"adapters.smb.draim_timeout",
			`"` + marker + `"`,
			`"adapters.smb.draim_timeout": unknown key; did you mean adapters.smb.drain_timeout?`,
		},
		{"adapters.smb.po\nrt", `1`, `"adapters.smb.po\nrt": unknown key` + hintPort},
		{"adapters.smb.\xffport", `1`, `"adapters.smb.\xffport": unknown key` + hintPort},
		{"adapters.smb", `{"port":1}`, `"adapters.smb": unknown key` + hintPort},
	})

	checkRefused[adaptersettings.NFS](t, nfsSection(), ErrUnknownKey, []refusalCase{
		{
			"adapters.nfs.tls.mode",
			`"require"`,
			`"adapters.nfs.tls.mode": unknown key; did you mean adapters.nfs.tls?`,
		},
	})

	wantName := nodeLine + `adapters.nfs.tls: "MODE": unknown name; did you mean "mode"?`

	_, err = decodeRow[adaptersettings.NFS](nfsSection(), nodeScope, "adapters.nfs.tls",
		`{"MODE":"require"}`)
	if err == nil || err.Error() != wantName {
		t.Errorf("nested name in another case: got %v, want %q", err, wantName)
	}

	installation := entity.SettingScope{
		Kind: entity.ScopeInstallation,
	}

	_, err = decodeRow[decSMB](smbSection(), installation, "adapters.smb.prot", `1`)

	want := `setting (installation): "adapters.smb.prot": unknown key` + hintPort
	if err == nil || err.Error() != want {
		t.Errorf("installation scope: got %v, want %q", err, want)
	}
}

// A value that json would fold, null over a field, a duplicate name, text
// that is not UTF-8, a wrong type or range, is refused naming the field and
// the rule; a malformed value and a secret reference print no value.
func TestDecodeValues(t *testing.T) {
	prof.Watch(t, prof.Options{})

	checkRefused[decSMB](t, smbSection(), ErrInvalid, []refusalCase{
		{"adapters.smb.port", `null`, "adapters.smb.port: must have a value, got null"},
		{"adapters.smb.port", `"80"`, `adapters.smb.port: must be a number, got "80"`},
		{
			"adapters.smb.port",
			`70000`,
			"adapters.smb.port: must be between 0 and 65535, got 70000",
		},
		{"adapters.smb.port", `-1`, "adapters.smb.port: must be between 0 and 65535, got -1"},
		{"adapters.smb.port", `1.5`, "adapters.smb.port: must be a whole number, got 1.5"},
		{"adapters.smb.server", `true`, "adapters.smb.server: must be a string, got true"},
		{"adapters.smb.port", `1 2`, "adapters.smb.port: must hold one JSON value"},
		{"adapters.smb.port", `{}x`, "adapters.smb.port: must hold one JSON value"},
		{"adapters.smb.port", ``, "adapters.smb.port: must hold one JSON value"},
	})

	checkRefused[adaptersettings.NFS](t, nfsSection(), ErrInvalid, []refusalCase{
		{
			"adapters.nfs.tls",
			`{"Mode":"off"}`,
			`adapters.nfs.tls: "Mode": unknown name; did you mean "mode"?`,
		},
		{
			"adapters.nfs.tls",
			`{"key_fil":"k"}`,
			`adapters.nfs.tls: "key_fil": unknown name; did you mean "key_file"?`,
		},
		{"adapters.nfs.tls", `{"mode":1}`, `adapters.nfs.tls: "mode": must be a string, got 1`},
		{"adapters.nfs.tls", `[ ]`, "adapters.nfs.tls: must be an object, got []"},
		{
			"adapters.nfs.tls",
			`{"mode":"\ud800\u0041"}`,
			`adapters.nfs.tls: "mode": must be valid UTF-8`,
		},
		{"adapters.nfs.tls", `[]`, "adapters.nfs.tls: must be an object, got []"},
		{
			"adapters.nfs.tls",
			`{"port":1,"port":2}`,
			`adapters.nfs.tls: "port" appears twice, got duplicate name "port"`,
		},
		{
			"adapters.nfs.versions",
			`{"min":"3","\u006din":"4.0"}`,
			`adapters.nfs.versions: "min" appears twice, got duplicate name "min"`,
		},
		{"adapters.nfs.tls", `{"mode":"\ud800"}`, `adapters.nfs.tls: "mode": must be valid UTF-8`},
		{
			"adapters.nfs.tls",
			"{\"mo\xffde\":\"off\"}",
			`adapters.nfs.tls: "mo\xffde": must be valid UTF-8`,
		},
		{
			"adapters.nfs.tls",
			`{"mod":"off"}`,
			`adapters.nfs.tls: "mod": unknown name; did you mean "mode"?`,
		},
		{
			"adapters.nfs.tls",
			`{"port":"s3cr3t-ma`,
			"adapters.nfs.tls: must hold one JSON value",
		},
	})

	const secretLine = "identity.oidc.client_secret: must be a secret reference"

	checkRefused[decOIDC](t, oidcSection(), ErrInvalid, []refusalCase{
		{"identity.oidc.client_secret", `42`, secretLine},
		{"identity.oidc.client_secret", `[1]`, secretLine},
		{"identity.oidc.client_secret", `{"s3cr3t-marker":1,"s3cr3t-marker":2}`, secretLine},
		{"identity.oidc.client_secret", "{\"s3cr3t-marker\xff\":1}", secretLine},
		{"identity.oidc.client_secret", `"` + marker + `"`, secretLine},
		{
			"identity.oidc.client_secret",
			`"s3cr3t-m" x`,
			"identity.oidc.client_secret: must hold one JSON value",
		},
		{
			"identity.oidc.client_secret",
			`"s3cr3t-m`,
			"identity.oidc.client_secret: must hold one JSON value",
		},
	})

	got, err := decodeRow[decKinds](kindsSection(), nodeScope, "kinds.limit", `null`)
	if err != nil || got.Limit != nil {
		t.Errorf("null into a pointer: got %v, %v, want nil, no error", got.Limit, err)
	}

	port, err := decodeRow[decSMB](smbSection(), nodeScope, "adapters.smb.port", `0`)
	if err != nil || port.Port != 0 {
		t.Errorf("port 0: got %d, %v, want 0 accepted", port.Port, err)
	}

	got, err = decodeRow[decKinds](kindsSection(), nodeScope, "kinds.name", escapesJSON)
	if want := "a\"b\\c/d\be\ff\ng\rh\tié😀😀"; err != nil || got.Name != want {
		t.Errorf("every escape: got %q, %v, want %q", got.Name, err, want)
	}

	got, err = decodeRow[decKinds](kindsSection(), nodeScope, "kinds.tls", indentedJSON)

	wantTLS := adaptersettings.TLS{
		Mode:     "require",
		CertFile: "c",
	}
	if err != nil || got.TLS != wantTLS {
		t.Errorf("indented object: got %+v, %v, want %+v", got.TLS, err, wantTLS)
	}

	got, err = decodeRow[decKinds](kindsSection(), nodeScope, "kinds.ratio", `1.5`)
	if err != nil || got.Ratio != 1.5 {
		t.Errorf("float: got %v, %v, want 1.5", got.Ratio, err)
	}

	checkRefused[decKinds](t, kindsSection(), ErrInvalid, []refusalCase{
		{"kinds.ratio", `"1.5"`, `kinds.ratio: must be a number, got "1.5"`},
	})
}

// A row replaces exactly its field over the default, a struct row only the
// names it holds, a list or a map row its whole field; no decode and no
// later change by the caller reaches the section's default.
func TestDecodeOverlay(t *testing.T) {
	prof.Watch(t, prof.Options{})

	sec := smbSection()

	got, err := decodeRow[decSMB](sec, nodeScope, "adapters.smb.port", `12445`)
	if err != nil {
		t.Fatalf("port row: %v", err)
	}

	if got.Port != 12445 || got.IdleTimeout != time.Minute {
		t.Errorf("port row: port %d, idle %v, want 12445 and the default 1m", got.Port,
			got.IdleTimeout)
	}

	got, err = decodeRow[decSMB](sec, nodeScope, "adapters.smb.disabled_operations", `["x"]`)
	if err != nil {
		t.Fatalf("list row: %v", err)
	}

	if !slices.Equal(got.DisabledOperations, []string{"x"}) {
		t.Errorf("list row = %q, want [x]", got.DisabledOperations)
	}

	got.DisabledOperations = append(got.DisabledOperations[:0], "y")

	none, err := decode(&sec, sec.def.(decSMB), nodeScope, nil)
	if err != nil {
		t.Fatalf("no rows: %v", err)
	}

	if !reflect.DeepEqual(none, sec.def.(decSMB)) {
		t.Errorf("no rows = %+v, want the default %+v", none, sec.def)
	}

	none.DisabledOperations[0] = "z"
	if def := sec.def.(decSMB).DisabledOperations; !slices.Equal(def, []string{"a", "b"}) {
		t.Errorf("default after decodes = %q, want [a b]", def)
	}

	nfs, err := decodeRow[adaptersettings.NFS](nfsSection(), nodeScope, "adapters.nfs.tls",
		`{"mode":"require"}`)
	if err != nil {
		t.Fatalf("struct row: %v", err)
	}

	if nfs.TLS.Mode != "require" || nfs.TLS.CertFile != "cert.pem" {
		t.Errorf("struct row = %+v, want mode require over cert.pem", nfs.TLS)
	}

	proxy, err := decodeRow[decKinds](kindsSection(), nodeScope, "kinds.proxy", `{"min":"4.0"}`)
	if err != nil || proxy.Proxy == nil || *proxy.Proxy != (adaptersettings.VersionRange{
		Min: "4.0",
		Max: "4.2",
	}) {
		t.Errorf("pointer to struct row: got %+v, %v, want min 4.0 over max 4.2", proxy.Proxy, err)
	}

	kinds, err := decodeRow[decKinds](kindsSection(), nodeScope, "kinds.labels", `{"b":"2"}`)
	if err != nil {
		t.Fatalf("map row: %v", err)
	}

	if !reflect.DeepEqual(kinds.Labels, map[string]string{"b": "2"}) {
		t.Errorf("map row = %v, want map[b:2] alone", kinds.Labels)
	}

	seven := 7
	shared := Of("p", At(entity.ScopeNode), decSharedOuter{
		decShared: decShared{
			Limit: &seven,
		},
	})

	written, err := decodeRow[decSharedOuter](shared, nodeScope, "p.limit", `99`)
	if err != nil || *written.Limit != 99 {
		t.Fatalf("limit row: got %v, %v, want 99", written.Limit, err)
	}

	again, err := decode(&shared, shared.def.(decSharedOuter), nodeScope, nil)
	if err != nil || *again.Limit != 7 || seven != 7 {
		t.Errorf("after a decode that wrote through an embedded struct: limit %v, caller's %d, want 7 and 7",
			again.Limit, seven)
	}

	def := kindsDefault()
	declared := Of("kinds", At(entity.ScopeNode), def)
	def.List[0] = "changed"
	def.Labels["a"] = "changed"
	*def.Limit = 99

	after, err := decode(&declared, declared.def.(decKinds), nodeScope, nil)
	if err != nil {
		t.Fatalf("after the caller's change: %v", err)
	}

	if !reflect.DeepEqual(after, kindsDefault()) {
		t.Errorf("after the caller's change = %+v, want the declared default", after)
	}
}

// The duration grammar and the strict object check apply by Go type at any
// depth; a field of a type the decode does not read is refused, never
// panicking.
func TestDecodeFieldKinds(t *testing.T) {
	prof.Watch(t, prof.Options{})

	sec := kindsSection()

	got, err := decodeRow[decKinds](sec, nodeScope, "kinds.waits", `["1s","250ms"]`)
	if err != nil ||
		!slices.Equal(got.Waits, []time.Duration{time.Second, 250 * time.Millisecond}) {
		t.Errorf("waits: got %v, %v, want [1s 250ms]", got.Waits, err)
	}

	got, err = decodeRow[decKinds](sec, nodeScope, "kinds.deadlines", `{"a":"1m"}`)
	if err != nil || !reflect.DeepEqual(got.Deadlines, map[string]time.Duration{"a": time.Minute}) {
		t.Errorf("deadlines: got %v, %v, want map[a:1m]", got.Deadlines, err)
	}

	got, err = decodeRow[decKinds](sec, nodeScope, "kinds.ranges", `[{"min":"3","max":"4.2"}]`)

	want := []adaptersettings.VersionRange{
		{
			Min: "3",
			Max: "4.2",
		},
	}
	if err != nil || !slices.Equal(got.Ranges, want) {
		t.Errorf("ranges: got %v, %v, want %v", got.Ranges, err, want)
	}

	deep := strings.Repeat("[", 5000) + strings.Repeat("]", 5000)

	checkRefused[decKinds](t, sec, ErrInvalid, []refusalCase{
		{"kinds.waits", `[30]`, "kinds.waits: must be a duration with a unit, got 30"},
		{"kinds.waits", `["1.5s"]`, `kinds.waits: must be a whole number, got "1.5s"`},
		{"kinds.deadlines", `{"a":"1"}`, `kinds.deadlines: "a": must end with a unit, got "1"`},
		{"kinds.ranges", `[{"mni":"3"}]`, `kinds.ranges: "mni": unknown name; did you mean "min"?`},
		{"kinds.waits", `[[30]]`, "kinds.waits: must be a duration with a unit, got [30]"},
		{
			"kinds.timeout",
			`[` + deep + `]`,
			"kinds.timeout: must be a duration with a unit, got " +
				strings.Repeat("[", maxPrinted) + " (cut, " + strconv.Itoa(len(deep)+2) + " bytes in all)",
		},
	})

	checkRefused[decChan](t, Of("adapters.smb", At(entity.ScopeNode), decChan{}), ErrInvalid,
		[]refusalCase{
			{
				"adapters.smb.extra",
				`1`,
				"adapters.smb.extra: the field's type is not supported (chan int)",
			},
		})

	checkRefused[decKinds](t, sec, ErrInvalid, []refusalCase{
		{
			"kinds.limit",
			`9223372036854775808`,
			"kinds.limit: must be between -9223372036854775808 and 9223372036854775807, " +
				"got 9223372036854775808",
		},
	})

	checkRefused[decKinds](t, sec, ErrInvalid, []refusalCase{
		{"kinds.limit", `"7"`, `kinds.limit: must be a number, got "7"`},
		{"kinds.limit", `1e3`, "kinds.limit: must be a whole number, got 1e3"},
		{"kinds.enabled", `1`, "kinds.enabled: must be true or false, got 1"},
	})

	checked, err := decodeRow[decKinds](sec, nodeScope, "kinds.enabled", `false`)
	if err != nil || checked.Enabled {
		t.Errorf("enabled false over true: got %v, %v, want false", checked.Enabled, err)
	}

	checked, err = decodeRow[decKinds](sec, nodeScope, "kinds.inner", `{"Plain":"x"}`)
	if err != nil || checked.Inner.Plain != "x" {
		t.Errorf("untagged nested name: got %q, %v, want x", checked.Inner.Plain, err)
	}

	checkRefused[decMapKey](t, Of("kinds", At(entity.ScopeNode), decMapKey{}), ErrInvalid,
		[]refusalCase{
			{
				"kinds.counts",
				`{"1":"a"}`,
				"kinds.counts: the field's type is not supported (map[int]string)",
			},
		})

	checkRefused[decAny](t, Of("adapters.smb", At(entity.ScopeNode), decAny{}), ErrInvalid,
		[]refusalCase{
			{
				"adapters.smb.extra",
				`{"a":1}`,
				"adapters.smb.extra: the field's type is not supported (interface {})",
			},
		})
}

// A refusal prints the value, the unknown key and the nested name it
// refuses, each at most maxPrinted bytes: a longer one is cut where a
// character ends and the line says how long it was; a shorter one prints
// whole.
func TestDecodeClip(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const mib = 1 << 20

	big := strings.Repeat("a", mib)
	head := strings.Repeat("a", maxPrinted)
	digits := strings.Repeat("1", mib)
	note := " (cut, " + strconv.Itoa(mib) + " bytes in all)"

	// two bytes for é, four for the emoji: the cut at maxPrinted falls inside it.
	twoByte := strings.Repeat("a", maxPrinted-1) + strings.Repeat("é", 10)
	fourByte := strings.Repeat("a", maxPrinted-2) + strings.Repeat("😀", 10)

	// long is 300 bytes: cut to 200, short enough to repeat in a line.
	long := strings.Repeat("b", 300)
	longHead := long[:maxPrinted]
	longNote := " (cut, 300 bytes in all)"

	// atEdge ends in a four-byte character that starts before the cut and
	// ends after it.
	atEdge := strings.Repeat("a", maxPrinted-3) + "😀"
	whole := strings.Repeat("a", maxPrinted)
	over := whole + "a"

	cases := []struct {
		key   string
		value string
		line  string
	}{
		{
			"kinds.enabled",
			`"` + atEdge + `"`,
			`kinds.enabled: must be true or false, got "` + strings.Repeat("a", maxPrinted-3) + `"` +
				" (cut, " + strconv.Itoa(len(atEdge)) + " bytes in all)",
		},
		{
			"kinds.enabled",
			`"` + whole + `"`,
			`kinds.enabled: must be true or false, got "` + whole + `"`,
		},
		{
			"kinds.enabled",
			`"` + over + `"`,
			`kinds.enabled: must be true or false, got "` + whole + `" (cut, 201 bytes in all)`,
		},
		{
			"kinds.labels",
			`{"` + long + `":1}`,
			`kinds.labels: "` + longHead + `"` + longNote + `: must be a string, got 1`,
		},
		{
			"kinds.labels",
			`{"` + long + "\":\"\xff\"}",
			`kinds.labels: "` + longHead + `"` + longNote + `: must be valid UTF-8`,
		},
		{
			"kinds.labels",
			`{"` + long + "\xff\":1}",
			`kinds.labels: "` + longHead + `" (cut, 301 bytes in all): must be valid UTF-8`,
		},
		{
			"kinds.labels",
			`{"` + long + `":"x","` + long + `":"y"}`,
			`kinds.labels: "` + longHead + `"` + longNote + ` appears twice, got duplicate name "` +
				longHead + `"` + longNote,
		},
		{
			"kinds.enabled",
			`"` + big + `"`,
			`kinds.enabled: must be true or false, got "` + head + `"` + note,
		},
		{
			"kinds.name",
			digits,
			`kinds.name: must be a string, got ` + strings.Repeat("1", maxPrinted) + note,
		},
		{
			"kinds.__" + big,
			`1`,
			`"kinds.__` + head[:maxPrinted-len("kinds.__")] + `"` +
				" (cut, " + strconv.Itoa(mib+len("kinds.__")) + " bytes in all)" +
				`: unknown key; did you mean kinds.name?`,
		},
		{
			"kinds.inner",
			`{"` + big + `":1}`,
			`kinds.inner: "` + head + `"` + note + `: unknown name; did you mean "Plain"?`,
		},
		{
			"kinds.enabled",
			`"` + twoByte + `"`,
			`kinds.enabled: must be true or false, got "` + strings.Repeat("a", maxPrinted-1) + `"` +
				" (cut, " + strconv.Itoa(len(twoByte)) + " bytes in all)",
		},
		{
			"kinds.enabled",
			`"` + fourByte + `"`,
			`kinds.enabled: must be true or false, got "` + strings.Repeat("a", maxPrinted-2) + `"` +
				" (cut, " + strconv.Itoa(len(fourByte)) + " bytes in all)",
		},
	}

	for _, tc := range cases {
		_, err := decodeRow[decKinds](kindsSection(), nodeScope, tc.key, tc.value)
		if err == nil {
			t.Errorf("%.30s: accepted, want refused", tc.key)

			continue
		}

		if got, want := err.Error(), nodeLine+tc.line; got != want {
			t.Errorf("%.30s = %.30q: line of %d bytes %.400q, want %.400q", tc.key, tc.value,
				len(got), got, want)
		}

		if !utf8.ValidString(err.Error()) {
			t.Errorf("%.30s: line %.400q is not valid UTF-8", tc.key, err)
		}
	}
}
