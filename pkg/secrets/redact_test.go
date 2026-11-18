package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const canary = "CANARY-7f3a-opened-secret"

// TestVersionNeverPrintsValue prints an opened version through every path a
// log line, an error or a dump takes, and fails if its value shows, in clear
// or in the base64 form json gives a byte slice.
func TestVersionNeverPrintsValue(t *testing.T) {
	v := Version{Number: 7, Value: []byte(canary), Meta: Meta{KeyVersion: 3, HandedOver: true}}

	const text = "{number:7 meta:{key_version:3 handed_over:true}}"

	cases := []struct {
		name string
		got  func() (string, error)
		want string
	}{
		{"%v", sprintf("%v", v), text},
		{"%+v", sprintf("%+v", v), text},
		{
			"%#v",
			sprintf("%#v", v),
			"secrets.Version{Number:7, Meta:secrets.Meta{KeyVersion:3, HandedOver:true}}",
		},
		{"Sprint", sprint(v), text},
		{
			"String",
			func() (string, error) {
				return v.String(), nil
			},
			text,
		},
		{"pointer", sprintf("%v", &v), text},
		{"slice", sprintf("%v", []Version{v}), "[" + text + "]"},
		{
			"json",
			marshal(json.Marshal, v),
			`{"number":7,"meta":{"key_version":3,"handed_over":true}}`,
		},
		{
			"yaml",
			marshal(yaml.Marshal, v),
			"number: 7\nmeta:\n    key_version: 3\n    handed_over: true\n",
		},
		{"slog", slogLine(v), `"v":{"number":7,"meta":{"key_version":3,"handed_over":true}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.got()
			if err != nil {
				t.Fatalf("print: %v", err)
			}

			leaks(t, got)

			if c.name == "slog" {
				if !strings.Contains(got, c.want) {
					t.Errorf("slog line %q, want it to hold %q", got, c.want)
				}

				return
			}

			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestVersionLogValueIsAGroup checks that slog is handed the version's
// number and meta, never the struct: without LogValue the JSON handler would
// fall back to MarshalJSON and print the same text.
func TestVersionLogValueIsAGroup(t *testing.T) {
	v := Version{Number: 7, Value: []byte(canary), Meta: Meta{KeyVersion: 3, HandedOver: true}}

	lv := v.LogValue()
	if lv.Kind() != slog.KindGroup {
		t.Fatalf("LogValue kind %v, want %v", lv.Kind(), slog.KindGroup)
	}

	const wantAttrs = "[number=7 meta=[key_version=3 handed_over=true]]"
	if got := fmt.Sprint(lv.Group()); got != wantAttrs {
		t.Errorf("LogValue attrs %s, want %s", got, wantAttrs)
	}

	var seen []string

	h := slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				seen = append(seen, strings.Join(groups, ".")+":"+a.Key)
			}

			return a
		},
	})
	slog.New(h).Info("opened", "v", v)

	want := []string{"v:number", "v.meta:key_version", "v.meta:handed_over"}
	if !slices.Equal(seen, want) {
		t.Errorf("ReplaceAttr saw %q, want %q", seen, want)
	}
}

func leaks(t *testing.T, out string) {
	t.Helper()

	for _, form := range []string{canary, base64.StdEncoding.EncodeToString([]byte(canary))} {
		if strings.Contains(out, form) {
			t.Errorf("output %q holds the secret value %q", out, form)
		}
	}
}

func sprintf(format string, a any) func() (string, error) {
	return func() (string, error) {
		return fmt.Sprintf(format, a), nil
	}
}

func sprint(a any) func() (string, error) {
	return func() (string, error) {
		return fmt.Sprint(a), nil
	}
}

func marshal(m func(any) ([]byte, error), a any) func() (string, error) {
	return func() (string, error) {
		b, err := m(a)

		return string(b), err
	}
}

func slogLine(a any) func() (string, error) {
	return func() (string, error) {
		var buf bytes.Buffer

		slog.New(slog.NewJSONHandler(&buf, nil)).Info("opened", "v", a)

		return buf.String(), nil
	}
}
