package settings

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/bytesize"
	"github.com/cubbit/dittofs/internal/prof"
	adaptersettings "github.com/cubbit/dittofs/pkg/adapter/settings"
)

// fuzzKeys are the rows of the kinds section, one per field kind.
var fuzzKeys = []string{
	"kinds.tls",
	"kinds.name",
	"kinds.list",
	"kinds.labels",
	"kinds.limit",
	"kinds.waits",
	"kinds.deadlines",
	"kinds.ranges",
	"kinds.timeout",
	"kinds.size",
}

// fuzzSeeds are the values of every row the decode tests pin, accepted and
// refused, each seeded under every key of the kinds section.
var fuzzSeeds = []string{
	`"512MiB"`, `"1B"`, `"1KB"`, `"1TB"`, `"1KiB"`, `"1TiB"`, `"512B"`,
	`"18446744073709551615B"`, `"17179869183GiB"`, `512`, `9007199254740993`,
	`9223372036854775808`, `18446744073709551615`, `"MiB"`, `" 512MiB"`, `""`, `"-1"`,
	`-1`, `1e3`, `"512"`, `"100"`, `"1.5GiB"`, `"4.1GB"`, `"0.1KiB"`, `1.5`, `1.0`,
	`1.5e3`, `"1Gi"`, `"1Mi"`, `"10m"`, `"1g"`, `"100mib"`, `"1kb"`, `"1kib"`, `"16EiB"`,
	`"1 MiB"`, `"512MiB "`, `"1e3"`, "\"1KiB\"", `"17179869184GiB"`,
	`"18446744073709551616B"`, `"18446744073709551615TB"`, `"18446744073709551616"`,
	`"18446744073709551616KB"`, `"18446744073709551616kb"`, `18446744073709551616`,
	`"250ms"`, `"30s"`, `"5m"`, `"1h"`, `"1h30m"`, `"0s"`, `"1500ms"`, `"90s"`, `"1s1h"`,
	`"2562047h"`, `"2562047h47m16s"`, `30`, `0`, `"30"`, `"0"`, `"30s30"`, `" 30s "`,
	`"-5s"`, `"+5s"`, `"1.5s"`, `"30S"`, "\"5µs\"", "\"5μs\"", `"5ns"`, `"5us"`,
	`"1d"`, `"2562048h"`, `"2562047h47m17s"`, `"2562047h2562047h"`, `null`, `"80"`,
	`70000`, `true`, `[]`, `{"port":1,"port":2}`, `{"min":"3","\u006din":"4.0"}`,
	`{"mode":"\ud800"}`, "{\"mo\xffde\":\"off\"}", `{"mod":"off"}`, `{"mode":"require"}`,
	`1 2`, `{}x`, ``, `{"port":"s3cr3t-ma`, `42`, `"s3cr3t-m" x`, `"s3cr3t-m`, `["x"]`,
	`{"b":"2"}`, `["1s","250ms"]`, `{"a":"1m"}`, `[{"min":"3","max":"4.2"}]`, `[30]`,
	`["1.5s"]`, `{"a":"1"}`, `[{"mni":"3"}]`, `[[30]]`,
	escapesJSON, indentedJSON,
}

// FuzzDecode reads rows no one at DittoFS wrote: no decode panics, every
// error is a refusal line, and what the decode accepts agrees with an
// independent reading over that field's domain.
func FuzzDecode(f *testing.F) {
	// decision: the leak check runs on the seed corpus only. Under -fuzz the
	// fuzzing engine keeps its own signal goroutine running, which the check
	// would report as a leak of this target. Plain go test runs every seed,
	// the refusals included, under the check. Withdraw when the fuzzing
	// engine stops leaving its signal goroutine running.
	if flag.Lookup("test.fuzz").Value.String() == "" {
		prof.Watch(f, prof.Options{})
	}

	for _, v := range fuzzSeeds {
		for _, k := range fuzzKeys {
			f.Add(k, v)
		}
	}

	f.Add("adapters.smb.Port", `1`)
	f.Add("kinds.tls", `{"port":1,"port":2}`)

	sec := kindsSection()

	f.Fuzz(func(t *testing.T, key, value string) {
		got, err := decodeRow[decKinds](sec, nodeScope, key, value)

		if key == "kinds.size" {
			checkSize(t, value, got.Size, err)
		}

		if err != nil {
			var r *refusal
			if !errors.As(err, &r) || !strings.HasPrefix(err.Error(), nodeLine) {
				t.Fatalf("%s = %q: error %q is no refusal line", key, value, err)
			}

			if !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrUnknownKey) {
				t.Fatalf("%s = %q: error %q wraps neither ErrInvalid nor ErrUnknownKey", key,
					value, err)
			}

			return
		}

		checkAccepted(t, sec, key, value, got)
	})
}

// checkAccepted holds an accepted row to an independent reading: json over
// the default for a field json reads the same way, json into a zero field
// for a map or a list of objects, and a decode of its re-encoding for a
// duration.
func checkAccepted(t *testing.T, sec Section, key, value string, got decKinds) {
	t.Helper()

	want := kindsDefault()

	var field, wantField any

	switch key {
	case "kinds.tls":
		field, wantField = got.TLS, &want.TLS
	case "kinds.name":
		field, wantField = got.Name, &want.Name
	case "kinds.list":
		field, wantField = got.List, &want.List
	case "kinds.limit":
		field, wantField = got.Limit, &want.Limit
	case "kinds.labels":
		field, wantField = got.Labels, new(map[string]string)
	case "kinds.ranges":
		field, wantField = got.Ranges, new([]adaptersettings.VersionRange)
	case "kinds.timeout":
		checkReencoded(t, sec, key, value, got.Timeout, encodeDuration(got.Timeout))

		return
	case "kinds.waits":
		parts := make([]string, len(got.Waits))
		for i, d := range got.Waits {
			parts[i] = encodeDuration(d)
		}

		checkReencoded(t, sec, key, value, got.Waits, "["+strings.Join(parts, ",")+"]")

		return
	case "kinds.deadlines":
		var parts []string
		for k, d := range got.Deadlines {
			parts = append(parts, fmt.Sprintf("%q:%s", k, encodeDuration(d)))
		}

		checkReencoded(t, sec, key, value, got.Deadlines, "{"+strings.Join(parts, ",")+"}")

		return
	default:
		return
	}

	if err := json.Unmarshal([]byte(value), wantField); err != nil {
		t.Fatalf("%s = %q: accepted as %v, json refuses it: %v", key, value, field, err)
	}

	wantValue := reflect.ValueOf(wantField).Elem().Interface()
	if !reflect.DeepEqual(field, wantValue) {
		t.Fatalf("%s = %q: got %#v, json reads %#v", key, value, field, wantValue)
	}
}

// encodeDuration writes d in the largest of h, m, s and ms that divides it
// exactly, as a JSON string; zero is 0s.
func encodeDuration(d time.Duration) string {
	for _, u := range []struct {
		name string
		d    time.Duration
	}{
		{"h", time.Hour},
		{"m", time.Minute},
		{"s", time.Second},
		{"ms", time.Millisecond},
	} {
		if d != 0 && d%u.d == 0 {
			return fmt.Sprintf(`"%d%s"`, d/u.d, u.name)
		}
	}

	if d == 0 {
		return `"0s"`
	}

	return fmt.Sprintf(`"%dns"`, d)
}

// checkReencoded decodes the re-encoding of an accepted duration row and
// requires the same value.
func checkReencoded(t *testing.T, sec Section, key, value string, got any, encoded string) {
	t.Helper()

	again, err := decodeRow[decKinds](sec, nodeScope, key, encoded)
	if err != nil {
		t.Fatalf("%s = %q: accepted as %v, its re-encoding %s refused: %v", key, value, got,
			encoded, err)
	}

	field := map[string]any{
		"kinds.timeout":   again.Timeout,
		"kinds.waits":     again.Waits,
		"kinds.deadlines": again.Deadlines,
	}[key]
	if !reflect.DeepEqual(field, got) {
		t.Fatalf("%s = %q: got %v, its re-encoding %s decodes to %v", key, value, got, encoded,
			field)
	}
}

// fileSize is Appendix B's file form of a size, whole number and unit.
var fileSize = regexp.MustCompile(`^([0-9]+)(B|KB|MB|GB|TB|KiB|MiB|GiB|TiB)$`)

// unitFactor is each listed unit's bytes, recomputed apart from the parser.
var unitFactor = map[string]int64{
	"B":   1,
	"KB":  1000,
	"MB":  1000 * 1000,
	"GB":  1000 * 1000 * 1000,
	"TB":  1000 * 1000 * 1000 * 1000,
	"KiB": 1 << 10,
	"MiB": 1 << 20,
	"GiB": 1 << 30,
	"TiB": 1 << 40,
}

// checkSize holds a size row to its product in math/big over the whole
// numbers: a JSON string of digits and a listed unit, or a JSON number of
// digits, is accepted exactly when its product fits 2^64-1 and refused when
// it does not; any other value is refused.
func checkSize(t *testing.T, value string, got bytesize.ByteSize, err error) {
	t.Helper()

	product, inDomain := sizeProduct(value)

	limit := new(big.Int).SetUint64(^uint64(0))
	fits := inDomain && product.Cmp(limit) <= 0

	switch {
	case fits && err != nil:
		t.Fatalf("size %q: refused (%v), want %s", value, err, product)
	case fits && new(big.Int).SetUint64(uint64(got)).Cmp(product) != 0:
		t.Fatalf("size %q = %d, want %s", value, got, product)
	case !fits && err == nil:
		t.Fatalf("size %q accepted as %d, want refused (in the domain %v)", value, got,
			inDomain)
	}
}

// sizeProduct reads value as json reads a token and returns its exact
// product, or false outside the whole-number domain.
func sizeProduct(value string) (*big.Int, bool) {
	var s string
	if err := json.Unmarshal([]byte(value), &s); err == nil {
		m := fileSize.FindStringSubmatch(s)
		if m == nil {
			return nil, false
		}

		n, _ := new(big.Int).SetString(m[1], 10)

		return n.Mul(n, big.NewInt(unitFactor[m[2]])), true
	}

	var n json.Number
	if err := json.Unmarshal([]byte(value), &n); err != nil {
		return nil, false
	}

	digits, ok := new(big.Int).SetString(n.String(), 10)
	if !ok || strings.ContainsAny(n.String(), "+-") {
		return nil, false
	}

	return digits, true
}
