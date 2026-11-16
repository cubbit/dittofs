package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

type sample struct {
	inner
	Name string   `json:"name"`
	Size uint64   `json:"size"`
	Tags []string `json:"tags"`
	Sub  *struct {
		On bool `json:"on"`
	} `json:"sub"`
}

type inner struct {
	Note string `json:"note"`
}

// An unknown field is refused naming it, nested or not, as is a key that
// matches a field only ignoring case, a key repeated in one object, a value
// of the wrong type and data after the value; an embedded struct's fields
// are the outer one's, and an empty body is the empty request.
func TestDecodeIsStrict(t *testing.T) {
	for _, tc := range []struct {
		body, field string
		ok          bool
	}{
		{`{"name":"a","size":1}`, "", true},
		{`{"note":"n"}`, "", true},
		{``, "", true},
		{`{"name":"a","colour":"red"}`, "colour", false},
		{`{"sub":{"on":true,"off":false}}`, "sub.off", false},
		{`{"NAME":"a"}`, "NAME", false},
		{`{"sub":{"On":true}}`, "sub.On", false},
		{`{"name":"a","name":"b"}`, "name", false},
		{`{"sub":{"on":true,"on":false}}`, "sub.on", false},
		{`{"size":"big"}`, "size", false},
		{`{"name":"a"} {"name":"b"}`, "", false},
		{`{"name":`, "", false},
	} {
		var s sample
		err := decode(strings.NewReader(tc.body), &s)
		if tc.ok {
			if err != nil {
				t.Errorf("%s: refused %v, want accepted", tc.body, err)
			}
			continue
		}
		var f *fieldError
		if !errors.As(err, &f) || !errors.Is(err, vfserr.ErrInvalid) {
			t.Errorf("%s: got %v, want a *fieldError that is ErrInvalid", tc.body, err)
			continue
		}
		if f.field != tc.field {
			t.Errorf("%s: refused field %q, want %q", tc.body, f.field, tc.field)
		}
	}
}

// What decode accepts, encoding/json's own lenient decoding reads alike, and
// re-encoded it decodes strictly to the same value; what it refuses is a
// *fieldError, never a panic.
func FuzzDecode(f *testing.F) {
	for _, s := range []string{`{"name":"a","size":18446744073709551615,"tags":["x"],"sub":{"on":true}}`, `{"x":1}`, `{}`, ``, `{"size":-1}`, `null`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var got sample
		err := decode(bytes.NewReader(b), &got)
		if err != nil {
			var fe *fieldError
			if !errors.As(err, &fe) {
				t.Fatalf("refused with %T %v, want a *fieldError", err, err)
			}
			return
		}
		if len(bytes.TrimSpace(b)) == 0 {
			return
		}
		var lenient sample
		if err := json.Unmarshal(b, &lenient); err != nil {
			t.Fatalf("decode accepted %q, encoding/json refuses it: %v", b, err)
		}
		if !reflect.DeepEqual(got, lenient) {
			t.Fatalf("decode read %+v, encoding/json %+v", got, lenient)
		}
		out, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var again sample
		if err := decode(bytes.NewReader(out), &again); err != nil || !reflect.DeepEqual(again, got) {
			t.Fatalf("re-encoded %s decodes to %+v (%v), want %+v", out, again, err, got)
		}
	})
}

// RFC 7396's own examples (its Appendix A) whose patch is an object, each
// compared as JSON values, and a large integer kept to its last digit.
func TestMergePatchRFC7396(t *testing.T) {
	for _, tc := range []struct{ doc, patch, want string }{
		{`{"a":"b"}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"b"}`, `{"b":"c"}`, `{"a":"b","b":"c"}`},
		{`{"a":"b"}`, `{"a":null}`, `{}`},
		{`{"a":"b","b":"c"}`, `{"a":null}`, `{"b":"c"}`},
		{`{"a":["b"]}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"c"}`, `{"a":["b"]}`, `{"a":["b"]}`},
		{`{"a":{"b":"c"}}`, `{"a":{"b":"d","c":null}}`, `{"a":{"b":"d"}}`},
		{`{"a":[{"b":"c"}]}`, `{"a":[1]}`, `{"a":[1]}`},
		{`{"e":null}`, `{"a":1}`, `{"e":null,"a":1}`},
		{`[1,2]`, `{"a":"b","c":null}`, `{"a":"b"}`},
		{`{}`, `{"a":{"bb":{"ccc":null}}}`, `{"a":{"bb":{}}}`},
		{`{"g":18446744073709551615}`, `{"h":1}`, `{"g":18446744073709551615,"h":1}`},
	} {
		got, err := mergePatch([]byte(tc.doc), []byte(tc.patch))
		if err != nil {
			t.Errorf("%s + %s: %v", tc.doc, tc.patch, err)
			continue
		}
		if !sameJSON(t, got, []byte(tc.want)) {
			t.Errorf("%s + %s: got %s, want %s", tc.doc, tc.patch, got, tc.want)
		}
		if strings.Contains(tc.want, "18446744073709551615") && !bytes.Contains(got, []byte("18446744073709551615")) {
			t.Errorf("%s + %s: got %s, want 18446744073709551615 exact", tc.doc, tc.patch, got)
		}
	}
}

// A patch that is not one JSON object with no key repeated is the request's
// fault, refused ErrInvalid; a document that does not decode is the stored
// record's, refused as no ErrInvalid.
func TestMergePatchRefuses(t *testing.T) {
	for _, patch := range []string{`null`, `["c"]`, `"bar"`, `1`, `{"a":1} {"b":2}`, `{"a":1,"a":2}`, `{"a":{"b":1,"b":2}}`, `{"size":`} {
		if _, err := mergePatch([]byte(`{"a":"b"}`), []byte(patch)); !errors.Is(err, vfserr.ErrInvalid) {
			t.Errorf("patch %s: got %v, want ErrInvalid", patch, err)
		}
	}
	for _, doc := range []string{`{"a":`, ``, `{} {}`} {
		if _, err := mergePatch([]byte(doc), []byte(`{"a":1}`)); err == nil || errors.Is(err, vfserr.ErrInvalid) {
			t.Errorf("document %q: got %v, want an error that is not ErrInvalid", doc, err)
		}
	}
}

// A patch's null resets a field to its default once the result is decoded
// into a fresh value, as a PATCH of a spec is; a field the patch leaves out
// keeps its value.
func TestMergePatchNullResetsToDefault(t *testing.T) {
	doc := []byte(`{"name":"a","size":7,"tags":["x"]}`)
	out, err := mergePatch(doc, []byte(`{"size":null,"tags":["y"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var s sample
	if err := decode(bytes.NewReader(out), &s); err != nil {
		t.Fatal(err)
	}
	if s.Size != 0 || s.Name != "a" || !reflect.DeepEqual(s.Tags, []string{"y"}) {
		t.Errorf("patched spec %+v: want size reset to 0, name kept a, tags [y]", s)
	}
	out, err = mergePatch([]byte(`{"name":"old"}`), []byte(`{"Name":"new"}`))
	if err != nil {
		t.Fatal(err)
	}
	var f *fieldError
	if err := decode(bytes.NewReader(out), &s); !errors.As(err, &f) || f.field != "Name" {
		t.Errorf("patch {\"Name\":\"new\"} over {\"name\":\"old\"}: decoding %s got %v, want refused naming Name", out, err)
	}
}

// What mergePatch answers is JSON, applying the same patch again changes
// nothing (RFC 7396 is idempotent), every member the patch nulls is absent
// from the result, and every other member the patch sets carries the
// patch's value.
func FuzzMergePatch(f *testing.F) {
	f.Add([]byte(`{"a":{"b":"c"},"n":1}`), []byte(`{"a":{"b":null,"c":2},"n":null}`))
	f.Add([]byte(`[1]`), []byte(`{"x":null}`))
	f.Fuzz(func(t *testing.T, doc, patch []byte) {
		out, err := mergePatch(doc, patch)
		if err != nil {
			return
		}
		if !json.Valid(out) {
			t.Fatalf("%s + %s answered %s, not JSON", doc, patch, out)
		}
		again, err := mergePatch(out, patch)
		if err != nil || !sameJSON(t, again, out) {
			t.Fatalf("%s + %s twice: %s (%v), want %s", doc, patch, again, err, out)
		}
		p, _ := decodeAny(patch)
		if path, ok := carries(decodeAnyOrNil(out), p, ""); !ok {
			t.Fatalf("%s + %s answered %s: %s is not what the patch sets", doc, patch, out, path)
		}
	})
}

// carries reports whether o holds what patch p sets: each null member
// absent, each object member carrying the member's own, and each other
// member equal; otherwise it names the first member that does not.
func carries(o, p any, path string) (string, bool) {
	pm, ok := p.(map[string]any)
	if !ok {
		return path, reflect.DeepEqual(o, p)
	}
	om, ok := o.(map[string]any)
	if !ok {
		return path, false
	}
	for k, v := range pm {
		ov, present := om[k]
		if v == nil {
			if present {
				return path + "." + k, false
			}
			continue
		}
		if at, ok := carries(ov, v, path+"."+k); !ok {
			return at, false
		}
	}
	return "", true
}

// A cursor decodes to the key it was made from within its listing's range,
// and is refused outside it, of another version even with a key inside it,
// or not base64url.
func TestCursor(t *testing.T) {
	lo, hi := []byte("NG\x00"), []byte("NG\xff")
	key := []byte("NG\x00staff")
	got, err := decodeCursor(encodeCursor(key), lo, hi)
	if err != nil || !bytes.Equal(got, key) {
		t.Errorf("round trip of %q: got %q, %v", key, got, err)
	}
	v2 := base64.RawURLEncoding.EncodeToString(append([]byte{2}, key...))
	for _, s := range []string{encodeCursor([]byte("SH\x00x")), encodeCursor(hi), "!!", "", "AA", v2} {
		if _, err := decodeCursor(s, lo, hi); !errors.Is(err, vfserr.ErrInvalid) {
			t.Errorf("cursor %q: got %v, want refused ErrInvalid", s, err)
		}
	}
}

// Any key a cursor decodes to lies within the listing's range, and encodes to
// a cursor decoding to the same key.
func FuzzCursor(f *testing.F) {
	f.Add(encodeCursor([]byte("NG\x00a")), []byte("NG\x00"), []byte("NG\xff"))
	f.Add("AU5H", []byte("N"), []byte(nil))
	f.Fuzz(func(t *testing.T, s string, lo, hi []byte) {
		if len(hi) == 0 {
			hi = nil
		}
		key, err := decodeCursor(s, lo, hi)
		if err != nil {
			return
		}
		if bytes.Compare(key, lo) < 0 || hi != nil && bytes.Compare(key, hi) >= 0 {
			t.Fatalf("cursor %q decoded to %q, outside [%q, %q)", s, key, lo, hi)
		}
		if again, err := decodeCursor(encodeCursor(key), lo, hi); err != nil || !bytes.Equal(again, key) {
			t.Fatalf("key %q re-encoded decodes to %q, %v", key, again, err)
		}
	})
}

func decodeAnyOrNil(b []byte) any {
	v, _ := decodeAny(b)
	return v
}

func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	va, err := decodeAny(a)
	if err != nil {
		t.Fatalf("%s: %v", a, err)
	}
	vb, err := decodeAny(b)
	if err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return reflect.DeepEqual(va, vb)
}
