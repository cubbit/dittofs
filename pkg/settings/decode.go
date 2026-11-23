package settings

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/cubbit/dittofs/internal/bytesize"
	"github.com/cubbit/dittofs/internal/configunits"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The two types whose values Appendix B's grammar reads, wherever they sit
// in a section's type.
var (
	durationType = reflect.TypeFor[time.Duration]()
	sizeType     = reflect.TypeFor[bytesize.ByteSize]()
)

// maxPrinted is the most bytes of a refused value, an unknown key or a nested
// name that one refusal prints; a longer one is cut and the line says so.
const maxPrinted = 200

// clip cuts s to at most maxPrinted bytes, never inside a character, and
// reports whether it cut.
func clip(s string) (string, bool) {
	if len(s) <= maxPrinted {
		return s, false
	}

	n := maxPrinted
	for i := 0; i < utf8.UTFMax-1 && !utf8.RuneStart(s[n]); i++ {
		n--
	}

	return s[:n], true
}

// cutNote says a printed text was cut, and how long it was.
func cutNote(s string) string {
	return fmt.Sprintf(" (cut, %d bytes in all)", len(s))
}

// quoted prints s as strconv.Quote does, cut at maxPrinted bytes.
func quoted(s string) string {
	head, cut := clip(s)
	if cut {
		return strconv.Quote(head) + cutNote(s)
	}

	return strconv.Quote(s)
}

// printed prints s as it stands, cut at maxPrinted bytes.
func printed(s string) string {
	head, cut := clip(s)
	if cut {
		return head + cutNote(s)
	}

	return s
}

// refusal is one refused row, printed as RFC 13 §6's whole line:
// setting (<scope>): <key>: <rule>. It wraps ErrUnknownKey for a row whose
// key names no field, ErrInvalid for every other refusal.
type refusal struct {
	scope entity.SettingScope
	key   string // printed as it stands: quoted already when the key is unknown
	fault fault
	kind  error
}

func (r *refusal) Error() string {
	where := r.scope.Kind.String()
	if r.scope.Kind != entity.ScopeInstallation {
		where += " " + hex.EncodeToString(r.scope.ID[:])
	}

	line := "setting (" + where + "): " + r.key + ": "
	if r.fault.name != "" {
		line += r.fault.name + ": "
	}

	return line + r.fault.rule
}

func (r *refusal) Unwrap() error {
	return r.kind
}

// fault is what a value broke: the name of the innermost object member it
// sits in, quoted, or none for the row's value itself, and the rule, worded
// by the decode and never by a decoder it calls.
type fault struct {
	name string
	rule string
}

// decode overlays one scope level's rows on base, a deep copy of it, into
// T: each row's key is sec's prefix and a declared json name, or the prefix
// alone when SelfKey declares the section, matched exactly, and its value
// is read strictly by the field's Go type. The first refused row ends the
// decode.
func decode[T any](
	sec *Section,
	base T,
	scope entity.SettingScope,
	rows []entity.Setting,
) (T, error) {
	out := clone(base)
	v := reflect.ValueOf(&out).Elem()
	fields := jsonFields(v.Type())

	var keys []string

	byKey := make(map[string][]int, len(fields))

	for _, f := range fields {
		key := sec.prefix + "." + f.name
		if sec.selfKey {
			key = sec.prefix
		}

		keys = append(keys, key)
		byKey[key] = f.index
	}

	for _, row := range rows {
		index, ok := byKey[row.Key]
		if !ok {
			var zero T

			rule := "unknown key"
			if len(keys) > 0 {
				rule += "; did you mean " + nearest(row.Key, keys) + "?"
			}

			return zero, &refusal{
				scope: scope,
				key:   quoted(row.Key),
				fault: fault{
					rule: rule,
				},
				kind: ErrUnknownKey,
			}
		}

		_, secret := sec.secretRefs[row.Key]
		if f := decodeValue(v.FieldByIndex(index), row.Value, secret); f != nil {
			var zero T

			return zero, &refusal{
				scope: scope,
				key:   row.Key,
				fault: *f,
				kind:  ErrInvalid,
			}
		}
	}

	return out, nil
}

// decodeValue reads one row's bytes into v. A value that is not one JSON
// value is refused with no value printed, since its bytes may start with a
// credential pasted into a reference field; every refusal of a reference
// field prints neither the value nor a name from it (RFC 13 §6).
func decodeValue(v reflect.Value, raw []byte, secret bool) *fault {
	// decision: json.Valid also bounds the nesting depth, so the parse below
	// recurses at most that deep; the decode itself recurses by Go type.
	if !json.Valid(raw) {
		return &fault{
			rule: "must hold one JSON value",
		}
	}

	p := jsonParser{
		b: raw,
	}

	j, f := p.value("")
	if f == nil {
		f = into(v, j, "")
	}

	if f != nil && secret {
		return &fault{
			rule: "must be a secret reference",
		}
	}

	return f
}

// into reads j into v by v's Go type; name is the innermost object member
// j sits in, quoted, or empty.
func into(v reflect.Value, j jsonValue, name string) *fault {
	t := v.Type()

	refuse := func(rule string) *fault {
		return &fault{
			name: name,
			rule: rule + ", got " + j.got(),
		}
	}

	if !supported(t) {
		return &fault{
			name: name,
			rule: fmt.Sprintf("the field's type is not supported (%v)", t),
		}
	}

	if j.kind == jsonNull {
		if t.Kind() == reflect.Pointer {
			v.SetZero()

			return nil
		}

		return refuse("must have a value")
	}

	switch t {
	case durationType:
		return intoDuration(v, j, refuse)
	case sizeType:
		return intoSize(v, j, refuse)
	}

	switch t.Kind() {
	case reflect.Bool:
		if j.kind != jsonBool {
			return refuse("must be true or false")
		}

		v.SetBool(j.text == "true")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return intoInt(v, j, refuse)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return intoUint(v, j, refuse)
	case reflect.Float32, reflect.Float64:
		if j.kind != jsonNumber {
			return refuse("must be a number")
		}

		n, err := strconv.ParseFloat(j.text, t.Bits())
		if err != nil {
			return refuse(fmt.Sprintf("must fit a %v", t))
		}

		v.SetFloat(n)
	case reflect.String:
		if j.kind != jsonString {
			return refuse("must be a string")
		}

		v.SetString(j.text)
	case reflect.Struct:
		return intoStruct(v, j, refuse)
	case reflect.Slice:
		if j.kind != jsonArray {
			return refuse("must be a list")
		}

		s := reflect.MakeSlice(t, len(j.items), len(j.items))
		for i, item := range j.items {
			if f := into(s.Index(i), item, name); f != nil {
				return f
			}
		}

		v.Set(s)
	case reflect.Array:
		if j.kind != jsonArray || len(j.items) != t.Len() {
			return refuse(fmt.Sprintf("must be a list of %d", t.Len()))
		}

		a := reflect.New(t).Elem()
		for i, item := range j.items {
			if f := into(a.Index(i), item, name); f != nil {
				return f
			}
		}

		v.Set(a)
	case reflect.Map:
		return intoMap(v, j, refuse)
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(t.Elem()))
		}

		return into(v.Elem(), j, name)
	}

	return nil
}

// supported reports whether the decode reads a value of type t; a type it
// does not read, an interface or a channel, is refused rather than handed
// to a decoder that would read it loosely.
func supported(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Bool, reflect.String, reflect.Struct, reflect.Slice, reflect.Array,
		reflect.Pointer, reflect.Float32, reflect.Float64,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	case reflect.Map:
		return t.Key().Kind() == reflect.String
	default:
		return false
	}
}

// intoDuration reads a duration from its string form by Appendix B's
// grammar; a bare number names no unit, and json would read it as
// nanoseconds.
func intoDuration(v reflect.Value, j jsonValue, refuse func(string) *fault) *fault {
	if j.kind != jsonString {
		return refuse("must be a duration with a unit")
	}

	d, err := configunits.ParseDuration(j.text)
	if err != nil {
		return refuse(durationRule(err))
	}

	v.SetInt(int64(d))

	return nil
}

// durationRule words a duration parse's cause.
func durationRule(err error) string {
	switch {
	case errors.Is(err, configunits.ErrNoUnit):
		return "must end with a unit"
	case errors.Is(err, configunits.ErrFraction):
		return "must be a whole number"
	case errors.Is(err, configunits.ErrUnknownUnit):
		return "must end with ms, s, m or h in lower case"
	case errors.Is(err, configunits.ErrOverflow):
		return "must fit 9223372036854775807 nanoseconds"
	default:
		return "must start with a number"
	}
}

// intoSize reads a size by its JSON token: a string in the file's form, a
// whole number and a unit as listed, and a number as an exact count of
// bytes, the record's own form.
func intoSize(v reflect.Value, j jsonValue, refuse func(string) *fault) *fault {
	var (
		n   uint64
		err error
	)

	switch j.kind {
	case jsonString:
		n, err = configunits.ParseFileSize(j.text)
	case jsonNumber:
		n, err = configunits.ParseRecordSizeBytes(j.text)
	default:
		return refuse("must be a size")
	}

	if err != nil {
		return refuse(sizeRule(err))
	}

	v.SetUint(n)

	return nil
}

// sizeRule words a size parse's cause.
func sizeRule(err error) string {
	switch {
	case errors.Is(err, configunits.ErrNoUnit):
		return "must end with a unit"
	case errors.Is(err, configunits.ErrFraction):
		return "must be a whole number"
	case errors.Is(err, configunits.ErrUnknownUnit):
		return "must end with a unit as Appendix B lists it"
	case errors.Is(err, configunits.ErrOverflow):
		return "must fit 18446744073709551615 bytes"
	default:
		return "must start with a number"
	}
}

// intoInt reads a whole number within v's type, never rounding a fraction
// or wrapping past the type.
func intoInt(v reflect.Value, j jsonValue, refuse func(string) *fault) *fault {
	if j.kind != jsonNumber {
		return refuse("must be a number")
	}

	if strings.ContainsAny(j.text, ".eE") {
		return refuse("must be a whole number")
	}

	bits := v.Type().Bits()

	n, err := strconv.ParseInt(j.text, 10, bits)
	if err != nil {
		return refuse(
			fmt.Sprintf("must be between %d and %d", int64(-1)<<(bits-1), uint64(1)<<(bits-1)-1),
		)
	}

	v.SetInt(n)

	return nil
}

// intoUint reads a whole number within v's type; a negative one is out of
// its range.
func intoUint(v reflect.Value, j jsonValue, refuse func(string) *fault) *fault {
	if j.kind != jsonNumber {
		return refuse("must be a number")
	}

	if strings.ContainsAny(j.text, ".eE") {
		return refuse("must be a whole number")
	}

	bits := v.Type().Bits()

	n, err := strconv.ParseUint(j.text, 10, bits)
	if err != nil {
		return refuse(fmt.Sprintf("must be between 0 and %d", uint64(1)<<bits-1))
	}

	v.SetUint(n)

	return nil
}

// intoStruct overlays an object's members on v's fields, each named exactly
// as declared; fields the object does not name keep their values.
func intoStruct(v reflect.Value, j jsonValue, refuse func(string) *fault) *fault {
	if j.kind != jsonObject {
		return refuse("must be an object")
	}

	fields := jsonFields(v.Type())

	for _, m := range j.members {
		name := quoted(m.name)

		i := fieldNamed(fields, m.name)
		if i < 0 {
			var names []string
			for _, f := range fields {
				names = append(names, f.name)
			}

			rule := "unknown name"
			if len(names) > 0 {
				rule += "; did you mean " + strconv.Quote(nearest(m.name, names)) + "?"
			}

			return &fault{
				name: name,
				rule: rule,
			}
		}

		if f := into(v.FieldByIndex(fields[i].index), m.value, name); f != nil {
			return f
		}
	}

	return nil
}

// intoMap replaces v whole with the object's members; its keys are the
// operator's own names, checked only for duplicates and UTF-8.
func intoMap(v reflect.Value, j jsonValue, refuse func(string) *fault) *fault {
	if j.kind != jsonObject {
		return refuse("must be an object")
	}

	t := v.Type()
	m := reflect.MakeMapWithSize(t, len(j.members))

	for _, member := range j.members {
		e := reflect.New(t.Elem()).Elem()
		if f := into(e, member.value, quoted(member.name)); f != nil {
			return f
		}

		m.SetMapIndex(reflect.ValueOf(member.name).Convert(t.Key()), e)
	}

	v.Set(m)

	return nil
}

// jsonField is one name an object of a struct type may hold, and the index
// path of the field it sets.
type jsonField struct {
	name  string
	index []int
}

// jsonFields lists t's names as Of reads them: a json:"-" field is none, an
// embedded struct with no json name lends its own, an unexported field is
// none unless it embeds a struct, and a field with no json name is named as
// Go names it.
func jsonFields(t reflect.Type) []jsonField {
	var out []jsonField

	for sf := range t.Fields() {
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}

		name, _, _ := strings.Cut(tag, ",")
		if sf.Anonymous && name == "" && sf.Type.Kind() == reflect.Struct {
			for _, inner := range jsonFields(sf.Type) {
				out = append(out, jsonField{
					name:  inner.name,
					index: append([]int{sf.Index[0]}, inner.index...),
				})
			}

			continue
		}

		// json skips an unexported field unless it embeds a struct, which a
		// json name makes one key, as Of declares it.
		if !sf.IsExported() && (!sf.Anonymous || sf.Type.Kind() != reflect.Struct) {
			continue
		}

		if name == "" {
			name = sf.Name
		}

		out = append(out, jsonField{
			name:  name,
			index: sf.Index,
		})
	}

	return out
}

// fieldNamed returns the index of the field named name, compared exactly,
// or -1.
func fieldNamed(fields []jsonField, name string) int {
	for i, f := range fields {
		if f.name == name {
			return i
		}
	}

	return -1
}

// nearest returns the candidate that differs from s only in case, else the
// one closest to s by edit distance, the first declared on a tie; a refusal always names one.
func nearest(s string, candidates []string) string {
	best, bestDist := "", -1

	for _, c := range candidates {
		if strings.EqualFold(s, c) {
			return c
		}
	}

	for _, c := range candidates {
		if d := editDistance(s, c); bestDist < 0 || d < bestDist {
			best, bestDist = c, d
		}
	}

	return best
}

// editDistance is the Levenshtein distance between a and b, by byte.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)

	for i := range prev {
		prev[i] = i
	}

	for i := 1; i <= len(a); i++ {
		cur[0] = i

		for k := 1; k <= len(b); k++ {
			cost := 1
			if a[i-1] == b[k-1] {
				cost = 0
			}

			cur[k] = min(prev[k]+1, cur[k-1]+1, prev[k-1]+cost)
		}

		prev, cur = cur, prev
	}

	return prev[len(b)]
}

// copyFields deep-copies v's fields into out, going into an embedded struct
// of an unexported type, whose exported fields jsonFields reaches and the
// decode writes.
func copyFields(out, v reflect.Value) {
	for i := range v.NumField() {
		switch f := out.Field(i); {
		case f.CanSet():
			f.Set(deepCopy(v.Field(i)))
		case v.Type().Field(i).Anonymous && f.Kind() == reflect.Struct:
			copyFields(f, v.Field(i))
		}
	}
}

// clone returns a copy of v that shares with v no slice, map or pointer it
// reaches through exported fields. An interface value, an any field among
// them, and an unexported field, an embedded struct of an unexported type
// excepted, are copied as they are, so what they point to stays shared.
func clone[T any](v T) T {
	return deepCopy(reflect.ValueOf(&v).Elem()).Interface().(T)
}

// deepCopy copies v and everything it reaches through exported fields,
// slices, arrays, maps and pointers; an unexported field is copied as it
// is, an embedded struct of an unexported type excepted.
func deepCopy(v reflect.Value) reflect.Value {
	out := reflect.New(v.Type()).Elem()
	out.Set(v)

	switch v.Kind() {
	case reflect.Struct:
		copyFields(out, v)
	case reflect.Array:
		for i := range v.Len() {
			out.Index(i).Set(deepCopy(v.Index(i)))
		}
	case reflect.Slice:
		if v.IsNil() {
			return out
		}

		s := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			s.Index(i).Set(deepCopy(v.Index(i)))
		}

		out.Set(s)
	case reflect.Map:
		if v.IsNil() {
			return out
		}

		m := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			m.SetMapIndex(it.Key(), deepCopy(it.Value()))
		}

		out.Set(m)
	case reflect.Pointer:
		if v.IsNil() {
			return out
		}

		p := reflect.New(v.Type().Elem())
		p.Elem().Set(deepCopy(v.Elem()))
		out.Set(p)
	}

	return out
}

// jsonKind is which of JSON's six kinds a value is.
type jsonKind uint8

const (
	jsonNull jsonKind = iota + 1
	jsonBool
	jsonNumber
	jsonString
	jsonArray
	jsonObject
)

// jsonValue is one parsed JSON value: a number or a literal as written, a
// string unescaped, a list's items and an object's members in order.
type jsonValue struct {
	kind    jsonKind
	text    string
	raw     []byte
	items   []jsonValue
	members []jsonMember
}

// jsonMember is one name and value of an object.
type jsonMember struct {
	name  string
	value jsonValue
}

// got prints the value as the caller wrote it: a string quoted, any other
// value compact.
func (j jsonValue) got() string {
	switch j.kind {
	case jsonString:
		return quoted(j.text)
	case jsonArray, jsonObject:
		var b bytes.Buffer
		if err := json.Compact(&b, j.raw); err != nil {
			return printed(string(j.raw))
		}

		return printed(b.String())
	default:
		return printed(j.text)
	}
}

// jsonParser reads a JSON value that json.Valid accepted, refusing what json
// would fold: a duplicate name, after unescaping, and text that is not
// UTF-8, a lone surrogate escape included.
type jsonParser struct {
	b []byte
	i int
}

// malformed is the fault of bytes the parser cannot read; json.Valid
// refuses them first, so it is reached only if the two disagree.
var malformed = fault{
	rule: "must hold one JSON value",
}

func (p *jsonParser) space() {
	for p.i < len(p.b) && strings.IndexByte(" \t\r\n", p.b[p.i]) >= 0 {
		p.i++
	}
}

// value reads one value; name is the member it sits in, quoted, or empty.
func (p *jsonParser) value(name string) (jsonValue, *fault) {
	p.space()

	if p.i >= len(p.b) {
		return jsonValue{}, &malformed
	}

	start := p.i

	var (
		j jsonValue
		f *fault
	)

	switch p.b[p.i] {
	case '{':
		j, f = p.object()
	case '[':
		j, f = p.array(name)
	case '"':
		var text string

		text, _, f = p.str(name)
		j = jsonValue{
			kind: jsonString,
			text: text,
		}
	case 't', 'f', 'n':
		j, f = p.literal()
	default:
		for p.i < len(p.b) && strings.IndexByte("+-.eE0123456789", p.b[p.i]) >= 0 {
			p.i++
		}

		j = jsonValue{
			kind: jsonNumber,
			text: string(p.b[start:p.i]),
		}
	}

	if f != nil {
		return jsonValue{}, f
	}

	j.raw = p.b[start:p.i]

	return j, nil
}

func (p *jsonParser) literal() (jsonValue, *fault) {
	for _, l := range []struct {
		text string
		kind jsonKind
	}{
		{"true", jsonBool},
		{"false", jsonBool},
		{"null", jsonNull},
	} {
		if bytes.HasPrefix(p.b[p.i:], []byte(l.text)) {
			p.i += len(l.text)

			return jsonValue{
				kind: l.kind,
				text: l.text,
			}, nil
		}
	}

	return jsonValue{}, &malformed
}

func (p *jsonParser) array(name string) (jsonValue, *fault) {
	j := jsonValue{
		kind: jsonArray,
	}

	p.i++
	p.space()

	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++

		return j, nil
	}

	for {
		item, f := p.value(name)
		if f != nil {
			return jsonValue{}, f
		}

		j.items = append(j.items, item)

		p.space()

		if p.i >= len(p.b) {
			return jsonValue{}, &malformed
		}

		c := p.b[p.i]
		p.i++

		switch c {
		case ',':
		case ']':
			return j, nil
		default:
			return jsonValue{}, &malformed
		}
	}
}

func (p *jsonParser) object() (jsonValue, *fault) {
	j := jsonValue{
		kind: jsonObject,
	}

	p.i++
	p.space()

	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++

		return j, nil
	}

	seen := make(map[string]bool)

	for {
		p.space()

		if p.i >= len(p.b) || p.b[p.i] != '"' {
			return jsonValue{}, &malformed
		}

		name, raw, f := p.str("")
		if f != nil {
			return jsonValue{}, &fault{
				name: quoted(raw),
				rule: f.rule,
			}
		}

		if seen[name] {
			return jsonValue{}, &fault{
				rule: fmt.Sprintf("%s appears twice, got duplicate name %s", quoted(name), quoted(name)),
			}
		}

		seen[name] = true

		p.space()

		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return jsonValue{}, &malformed
		}

		p.i++

		value, f := p.value(quoted(name))
		if f != nil {
			return jsonValue{}, f
		}

		j.members = append(j.members, jsonMember{
			name:  name,
			value: value,
		})

		p.space()

		if p.i >= len(p.b) {
			return jsonValue{}, &malformed
		}

		c := p.b[p.i]
		p.i++

		switch c {
		case ',':
		case '}':
			return j, nil
		default:
			return jsonValue{}, &malformed
		}
	}
}

// str reads a string at p.i, its opening quote; it returns the text
// unescaped and the bytes between the quotes as written. Text that is not
// UTF-8, or a surrogate escape outside a pair, is refused, where json would
// put U+FFFD in its place.
func (p *jsonParser) str(name string) (string, string, *fault) {
	p.i++
	start := p.i

	var (
		b     strings.Builder
		valid = true
	)

	for p.i < len(p.b) {
		c := p.b[p.i]

		switch {
		case c == '"':
			raw := string(p.b[start:p.i])
			p.i++

			if !valid {
				return "", raw, &fault{
					name: name,
					rule: "must be valid UTF-8",
				}
			}

			return b.String(), raw, nil
		case c == '\\':
			r, ok := p.escape()
			if r < 0 {
				return "", "", &malformed
			}

			valid = valid && ok

			b.WriteRune(r)
		case c < utf8.RuneSelf:
			b.WriteByte(c)

			p.i++
		default:
			r, size := utf8.DecodeRune(p.b[p.i:])
			if r == utf8.RuneError && size == 1 {
				valid = false
			}

			b.Write(p.b[p.i : p.i+size])
			p.i += size
		}
	}

	return "", "", &malformed
}

// escape reads one escape at p.i, its backslash, and returns its rune and
// whether it is valid text: false for a surrogate outside a pair. A rune
// below zero is bytes json.Valid would have refused.
func (p *jsonParser) escape() (rune, bool) {
	if p.i+1 >= len(p.b) {
		return -1, false
	}

	c := p.b[p.i+1]
	p.i += 2

	switch c {
	case '"', '\\', '/':
		return rune(c), true
	case 'b':
		return '\b', true
	case 'f':
		return '\f', true
	case 'n':
		return '\n', true
	case 'r':
		return '\r', true
	case 't':
		return '\t', true
	case 'u':
		r := p.hex4()
		if r < 0 || !utf16.IsSurrogate(r) {
			return r, r >= 0
		}

		if r >= 0xdc00 || !bytes.HasPrefix(p.b[p.i:], []byte(`\u`)) {
			return utf8.RuneError, false
		}

		save := p.i
		p.i += 2

		low := p.hex4()
		if pair := utf16.DecodeRune(r, low); pair != utf8.RuneError {
			return pair, true
		}

		p.i = save

		return utf8.RuneError, false
	default:
		return -1, false
	}
}

// hex4 reads four hex digits at p.i, or returns -1.
func (p *jsonParser) hex4() rune {
	if p.i+4 > len(p.b) {
		return -1
	}

	n, err := strconv.ParseUint(string(p.b[p.i:p.i+4]), 16, 16)
	if err != nil {
		return -1
	}

	p.i += 4

	return rune(n)
}
