package api

import (
	"encoding"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// apiDescription is the document's description: the conventions every
// operation follows, which its parameters and answers then spell out, and
// what its extension fields mean.
var apiDescription = `The DittoFS node's management API (RFC 23 §4), generated from the API's route table and its wire types.

- A call carries a bearer token unless its operation's security is empty. A caller whose role may not read what a call reaches is answered 404, as though it did not exist; one that may read it but not make the call, 403.
- A read that answers an ETag answers the record's tag. With ?wait=<duration>, above 0 and at most ` + maxWait.String() + `, it is a long poll: it answers once the record changes, or once the wait passes.
- A listing answers one page: ?limit= (default ` + strconv.Itoa(defaultLimit) + `, at most ` + strconv.Itoa(maxLimit) + `) and ?after=, the opaque cursor the previous page answered as next, which the last page leaves out.
- A PUT, a PATCH, a DELETE, or a POST action that changes the spec, carries If-Match with the ETag it read, or, a PUT that may create its record, If-None-Match: *; without one it is refused 428, and with a stale one 412. A PATCH body is a JSON merge patch (RFC 7396) of the PUT body: a field present sets it, and null resets it to its default.
- A POST or a DELETE carries an Idempotency-Key: a retry with the same key and the same request answers what the first answered, and one with another request is refused 422.
- A request body names no field its type does not have. Its schema requires no field: one left out takes its default, and a handler refuses, 400 naming it, one it needs.
- A write that starts an operation answers 202 with the operation and its Location; GET /api/v1/operations/{id}?wait= follows it.
- Every refusal is ` + apiv1.MediaTypeProblem + `: its code names the refusal, and retryable says whether the same request may succeed later. Any refusal may carry Retry-After.
- The API listener also answers the health probes GET /health/live and GET /health/ready, unauthenticated and in text/plain, which this document leaves out.

Extensions on each operation: x-dittofs-role is the least role that may make the call, none for a public one; x-dittofs-role-by-kind is true when the call needs instead the role that starts the kind of the operation it names; x-dittofs-starts is the kind of operation the write starts, answered 202, and x-dittofs-may-start the kind it starts only when what it writes calls for one, answered 202 then and as its own answer otherwise; x-dittofs-waits-applied is true when the write's answer waits, at most ` + appliedWait.String() + `, for the node answering it to apply it.`

// OpenAPI is the API's OpenAPI 3.1 document, as JSON, built from the route
// table, the wire types its adapters carry and the problem table.
func OpenAPI() ([]byte, error) {
	doc, err := openAPI(routes)
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	return append(b, '\n'), err
}

// openAPI is the document of rows. Two rows answering one operation ID are
// refused, naming both.
func openAPI(rows []route) (map[string]any, error) {
	sc := schemas{defs: map[string]any{}, types: map[string]reflect.Type{}}
	problem, err := sc.of(reflect.TypeFor[apiv1.Problem](), modeOut)
	if err != nil {
		return nil, err
	}
	paths := map[string]map[string]any{}
	ids := map[string]string{}
	used := map[int]bool{}
	for i := range rows {
		rt := &rows[i]
		id := operationID(rt)
		if prev, ok := ids[id]; ok {
			return nil, fmt.Errorf("openapi: %s and %s share the operation ID %s", prev, rt.op(), id)
		}
		ids[id] = rt.op()
		op, err := docOperation(rt, id, &sc)
		if err != nil {
			return nil, fmt.Errorf("openapi: %s: %w", rt.op(), err)
		}
		for _, st := range problemStatuses(rt) {
			used[st] = true
		}
		p := rt.pattern
		if rt.verb != "" {
			p += ":" + rt.verb
		}
		if paths[p] == nil {
			paths[p] = map[string]any{}
		}
		paths[p][strings.ToLower(rt.method)] = op
	}
	responses := map[string]any{}
	for st := range used {
		responses[problemRef(st)] = map[string]any{
			"description": http.StatusText(st) + ": a problem body, whose code names the refusal.",
			"headers":     map[string]any{"Retry-After": map[string]any{"description": "Seconds to wait before trying again, when the refusal names a wait.", "schema": map[string]any{"type": "integer"}}},
			"content":     map[string]any{apiv1.MediaTypeProblem: map[string]any{"schema": problem}},
		}
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "DittoFS management API",
			"version":     "v1",
			"description": apiDescription,
		},
		"security": []any{map[string]any{"bearer": []any{}}},
		"paths":    paths,
		"components": map[string]any{
			"schemas":         sc.defs,
			"responses":       responses,
			"securitySchemes": map[string]any{"bearer": map[string]any{"type": "http", "scheme": "bearer"}},
		},
	}, nil
}

// operationID names a row by its path's literal segments below /api/v1 (or
// /api), hyphens made underscores, then its action: its verb, list or get
// for a GET, create for a row that creates its record, or its method. It
// changes only when the row's method, path or verb does, so a client keying
// on it, such as an MCP tool, keeps its name while the API keeps the call.
func operationID(rt *route) string {
	var words []string
	for _, seg := range strings.Split(strings.TrimPrefix(strings.TrimPrefix(rt.pattern, "/api/"), "v1/"), "/") {
		if !strings.HasPrefix(seg, "{") {
			words = append(words, seg)
		}
	}
	action := strings.ToLower(rt.method)
	switch {
	case rt.verb != "":
		action = rt.verb
	case rt.method == http.MethodGet && rt.serve.form == formList:
		action = "list"
	case rt.creates():
		action = "create"
	}
	return strings.ReplaceAll(strings.Join(append(words, action), "_"), "-", "_")
}

// roleOf is the least role that holds right r.
//
// decision: the roles form a ladder, each holding every right the one below
// it holds, so the least role holding a right says every role that does.
// Overturned if a role holds a right one above it lacks; then the extension
// lists the roles.
func roleOf(r auth.Right) entity.APIRole {
	for role := entity.RoleNone; role < entity.RoleAdmin; role++ {
		if _, hold := auth.Allowed(auth.Caller{Role: role}, r); hold {
			return role
		}
	}
	return entity.RoleAdmin
}

var pathParam = regexp.MustCompile(`\{(\w+)\}`)

// schema is the string schema p describes.
func (p *param) schema() map[string]any {
	s := map[string]any{"type": "string"}
	if p.format != "" {
		s["format"] = p.format
	}
	if p.example != "" {
		s["examples"] = []string{p.example}
	}
	if p.enum != nil {
		s["enum"] = p.enum
	}
	return s
}

// idOf is what a path's {id} names: the collection before it, in the
// singular.
func idOf(pattern string) string {
	before, _, _ := strings.Cut(pattern, "/{id}")
	return strings.TrimSuffix(before[strings.LastIndex(before, "/")+1:], "s")
}

// duration is a Go duration as ?wait= parses it.
var duration = map[string]any{"type": "string", "pattern": `^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`}

// docOperation is the document's operation of rt.
func docOperation(rt *route, id string, sc *schemas) (map[string]any, error) {
	a := rt.serve
	op := map[string]any{"operationId": id, "x-dittofs-role": roleOf(rt.right).String()}
	if rt.right == auth.Public {
		op["security"] = []any{}
	}
	if rt.desc != "" {
		op["description"] = rt.desc
	}
	if a.roleByKind {
		op["x-dittofs-role-by-kind"] = true
	}
	if rt.mayStart != "" {
		op["x-dittofs-may-start"] = string(rt.mayStart)
	}
	if rt.starts != "" {
		op["x-dittofs-starts"] = string(rt.starts)
	}
	if a.raises {
		op["x-dittofs-waits-applied"] = true
	}
	var params []any
	add := func(in, name string, required bool, schema map[string]any, desc string) {
		p := map[string]any{"in": in, "name": name, "schema": schema}
		if required {
			p["required"] = true
		}
		if desc != "" {
			p["description"] = desc
		}
		params = append(params, p)
	}
	str := map[string]any{"type": "string"}
	for _, m := range pathParam.FindAllStringSubmatch(rt.pattern, -1) {
		switch m[1] {
		case "id":
			add("path", "id", true, map[string]any{"type": "string", "format": "uuid"}, "The "+idOf(rt.pattern)+"'s ID.")
		case "sub":
			if rt.sub == nil {
				return nil, fmt.Errorf("a row whose path has {sub} says what it names")
			}
			add("path", "sub", true, rt.sub.schema(), rt.sub.desc)
		default:
			return nil, fmt.Errorf("no description of the path parameter {%s}", m[1])
		}
	}
	// query adds a query parameter, as rt.params describe it.
	query := func(name, desc string) {
		schema := str
		for _, p := range rt.params {
			if p.name == name {
				schema, desc = p.schema(), p.desc
			}
		}
		add("query", name, false, schema, desc)
	}
	switch a.form {
	case formWatch:
		desc := "Long poll: a duration above 0 and at most " + maxWait.String() + " answers once the record changes, or once the wait passes."
		schema := duration
		if len(a.words) > 0 {
			alts := []any{duration}
			for _, w := range a.words {
				alts = append(alts, map[string]any{"const": w.name})
				desc += " " + w.name + " waits " + w.desc + "."
			}
			schema = map[string]any{"anyOf": alts}
		}
		add("query", "wait", false, schema, desc)
	case formList:
		add("query", "limit", false, map[string]any{"type": "integer", "minimum": 1, "maximum": maxLimit, "default": defaultLimit}, "The page size.")
		add("query", "after", false, str, "The cursor the previous page answered as next.")
		for _, f := range a.filters {
			query(f, "Filter on "+f+".")
		}
	case formWrite:
		for _, q := range rt.query {
			query(q, "")
		}
		p := rt.precondition()
		if p.key {
			add("header", "Idempotency-Key", true, map[string]any{"type": "string", "maxLength": maxKey}, "At most "+strconv.Itoa(maxKey)+" bytes; a retry with the same key and request answers what the first answered.")
		}
		switch {
		case p.create:
			add("header", "If-Match", false, str, "The ETag the write expects; the request carries it or If-None-Match.")
			add("header", "If-None-Match", false, map[string]any{"const": "*"}, "A PUT creating its record.")
		case p.match:
			add("header", "If-Match", true, str, "The ETag the write expects.")
		}
	}
	if params != nil {
		op["parameters"] = params
	}
	if a.body != nil && a.body != reflect.TypeFor[none]() {
		ct, m := "application/json", modeIn
		if rt.method == http.MethodPatch {
			ct, m = "application/merge-patch+json", modePatch
		}
		s, err := sc.of(a.body, m)
		if err != nil {
			return nil, err
		}
		if m == modePatch {
			// A patch is a JSON object, though the values in it may be
			// null: its write refuses a null or an empty body.
			s["type"] = "object"
		}
		op["requestBody"] = map[string]any{"required": true, "content": map[string]any{ct: map[string]any{"schema": s}}}
	}
	responses := map[string]any{}
	for _, st := range problemStatuses(rt) {
		responses[strconv.Itoa(st)] = map[string]any{"$ref": "#/components/responses/" + problemRef(st)}
	}
	// headers names the success answer's headers, each true when it is
	// always sent.
	status, out, headers := http.StatusOK, a.out, map[string]bool{}
	switch {
	case a.form == formWatch:
		headers["ETag"] = true
	case a.form == formWrite && rt.starts != "":
		status, out, headers = http.StatusAccepted, reflect.TypeFor[apiv1.Operation](), map[string]bool{"Location": true, "ETag": false}
	case a.form == formWrite && rt.creates():
		status, headers = http.StatusCreated, map[string]bool{"ETag": true, "Location": true}
	case a.form == formWrite:
		// A delete that removes its record answers no ETag.
		headers["ETag"] = rt.method != http.MethodDelete
	}
	answer := func(status int, out reflect.Type, headers map[string]bool) error {
		if out == nil {
			return nil
		}
		s, err := sc.of(out, modeOut)
		if err != nil {
			return err
		}
		r := map[string]any{"description": http.StatusText(status), "content": map[string]any{"application/json": map[string]any{"schema": s}}}
		if len(headers) > 0 {
			h := map[string]any{}
			for name, always := range headers {
				h[name] = map[string]any{"schema": str, "required": always}
			}
			r["headers"] = h
		}
		responses[strconv.Itoa(status)] = r
		return nil
	}
	if err := answer(status, out, headers); err != nil {
		return nil, err
	}
	if rt.mayStart != "" {
		if err := answer(http.StatusAccepted, reflect.TypeFor[apiv1.Operation](), map[string]bool{"Location": true, "ETag": false}); err != nil {
			return nil, err
		}
	}
	op["responses"] = responses
	return op, nil
}

// problemStatuses is every status a refusal of rt answers: 500 and 503 for
// any call; 401, 403 and 404 for one that authenticates; 400 for one that
// takes parameters or a body; for a write, 409, 422 when its answer is
// recorded under its key, 428 when its precondition requires a header and
// 412 when it compares a tag; and the status of each error a watched read's
// wait refuses with.
func problemStatuses(rt *route) []int {
	a := rt.serve
	st := []int{http.StatusInternalServerError, http.StatusServiceUnavailable}
	if rt.right != auth.Public {
		st = append(st, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound)
	}
	if a.form != formRead {
		st = append(st, http.StatusBadRequest)
	}
	if a.form == formWrite {
		st = append(st, http.StatusConflict)
		if a.replays && !rt.keyless {
			st = append(st, http.StatusUnprocessableEntity)
		}
		p := rt.precondition()
		if p.key || p.compares() {
			st = append(st, http.StatusPreconditionRequired)
		}
		if p.compares() {
			st = append(st, http.StatusPreconditionFailed)
		}
	}
	for _, err := range a.refuses {
		p, _ := problemOf(err)
		st = append(st, p.Status)
	}
	slices.Sort(st)
	return slices.Compact(st)
}

func problemRef(status int) string { return "Problem" + strconv.Itoa(status) }

// mode is what a schema describes: an answer, a request body, or a merge
// patch of one.
type mode uint8

const (
	modeOut mode = iota
	modeIn
	modePatch
)

// suffix names a component in each mode.
var suffix = [...]string{modeOut: "", modeIn: "Request", modePatch: "Patch"}

// schemas builds JSON Schemas from Go types as encoding/json writes and reads
// them: an exported, non-generic named struct becomes a component, named by
// its type and mode and referenced; any other struct is written inline. In
// an answer, a field tagged omitempty or omitzero is optional and every
// other required. A request body requires no field, since its decoder
// refuses none left out, and admits no field its type lacks, since its
// decoder refuses one. A merge patch's every value may also be null, at
// every depth but inside an array, which a patch replaces whole.
type schemas struct {
	defs  map[string]any
	types map[string]reflect.Type
}

var (
	timeType          = reflect.TypeFor[time.Time]()
	rawType           = reflect.TypeFor[json.RawMessage]()
	jsonMarshaler     = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// typed is a schema of JSON type name, which in a patch may also be null.
func typed(name string, m mode) map[string]any {
	if m == modePatch {
		return map[string]any{"type": []string{name, "null"}}
	}
	return map[string]any{"type": name}
}

// enums is the values of each closed set a body field takes, which its
// schema names.
var enums = map[reflect.Type][]string{
	reflect.TypeFor[apiv1.JoinMode](): {string(apiv1.JoinOnline), string(apiv1.JoinOffline)},
}

func (sc *schemas) of(t reflect.Type, m mode) (map[string]any, error) {
	// ponytail: a pointer is described as what it points to, so a nil one
	// without omitempty, which encodes as null, is not admitted in an
	// answer; every pointer on the wire is omitempty today. Add null to the
	// type when one is not.
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var s map[string]any
	switch {
	case t == timeType:
		s = typed("string", m)
		s["format"] = "date-time"
		return s, nil
	case t == rawType || t.Kind() == reflect.Interface:
		return map[string]any{}, nil
	case reflect.PointerTo(t).Implements(jsonMarshaler):
		return nil, fmt.Errorf("%s encodes itself, which no schema here describes", t)
	case reflect.PointerTo(t).Implements(textMarshalerType):
		return typed("string", m), nil
	}
	switch t.Kind() {
	case reflect.Bool:
		return typed("boolean", m), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return typed("integer", m), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		s = typed("integer", m)
		s["minimum"] = 0
		return s, nil
	case reflect.String:
		s = typed("string", m)
		if vs, ok := enums[t]; ok {
			s["enum"] = vs
		}
		return s, nil
	case reflect.Slice, reflect.Array:
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			s = typed("string", m)
			s["contentEncoding"] = "base64"
			return s, nil
		}
		im := m
		if m == modePatch {
			im = modeIn
		}
		items, err := sc.of(t.Elem(), im)
		s = typed("array", m)
		s["items"] = items
		return s, err
	case reflect.Map:
		// A map's keys are names its handler checks, so a patch's map is
		// merged as an object, its values nullable.
		v, err := sc.of(t.Elem(), m)
		s = typed("object", m)
		s["additionalProperties"] = v
		return s, err
	case reflect.Struct:
		return sc.object(t, m)
	}
	return nil, fmt.Errorf("%s has no JSON schema here", t)
}

// object is struct t's schema in mode m: a reference to its component when
// it has one, and the schema of the one struct it embeds when it is
// unexported and adds no field of its own to it. An answer's object is
// open: a field added to it is no change a client must refuse.
func (sc *schemas) object(t reflect.Type, m mode) (map[string]any, error) {
	name := t.Name()
	component := name != "" && t.PkgPath() != "" && !strings.Contains(name, "[") && name[0] >= 'A' && name[0] <= 'Z'
	if !component {
		if e, ok := soleEmbed(t); ok {
			return sc.of(e, m)
		}
	}
	name += suffix[m]
	ref := map[string]any{"$ref": "#/components/schemas/" + name}
	if component {
		if prev, ok := sc.types[name]; ok {
			if prev != t {
				return nil, fmt.Errorf("%s and %s are both named %s", prev, t, name)
			}
			return ref, nil
		}
		sc.types[name] = t
	}
	props, required := map[string]any{}, []string{}
	for _, f := range wireFields(t, nil) {
		s, err := sc.of(f.typ, m)
		if err != nil {
			return nil, err
		}
		props[f.name] = s
		if m == modeOut && !f.optional {
			required = append(required, f.name)
		}
	}
	s := typed("object", m)
	if len(props) > 0 {
		s["properties"] = props
	}
	if len(required) > 0 {
		s["required"] = required
	}
	if m != modeOut {
		s["additionalProperties"] = false
	}
	if !component {
		return s, nil
	}
	sc.defs[name] = s
	return ref, nil
}

// soleEmbed is the one struct t embeds untagged, when t has no other field
// encoding/json writes.
func soleEmbed(t reflect.Type) (reflect.Type, bool) {
	var e reflect.Type
	for i := range t.NumField() {
		f := t.Field(i)
		switch tag := f.Tag.Get("json"); {
		case f.Anonymous && tag == "" && f.Type.Kind() == reflect.Struct:
			if e != nil {
				return nil, false
			}
			e = f.Type
		case f.IsExported() && tag != "-":
			return nil, false
		}
	}
	return e, e != nil
}
