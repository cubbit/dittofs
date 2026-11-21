package core_test

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// routed is the tables testproto's router returns for versions 1 and 2, of a
// protocol of their own.
func routed(t *testing.T) [2]*core.OpTable {
	t.Helper()
	p := testproto.New(adapter.Deps{})
	st := p.NewConn(nil)
	var tables [2]*core.OpTable
	for i := range tables {
		b := testproto.Append(nil, testproto.Msg{Op: testproto.OpEcho, Version: core.Version(i + 1)})
		h, err := st.Header.Decode(&resources.Buf{B: b[p.Framer().HeaderLen():]}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if tables[i], _, err = st.Router.Route(h, nil); err != nil {
			t.Fatal(err)
		}
	}
	return tables
}

// listing is testproto's framer and stages, listing tables of the test's
// own: what NewServer reads of a listener's protocol before it accepts.
type listing struct {
	*testproto.Protocol
	tables []*core.OpTable
}

func (l listing) Tables() []*core.OpTable { return l.tables }

// seriesOf is sh's series of tb, resolved as NewServer resolves a table its
// listeners list, and found as Route finds it, at tb's first version.
func seriesOf(t *testing.T, sh *core.Shared, tb *core.OpTable) *core.TableSeries {
	t.Helper()
	if err := core.ResolveTables(sh, core.Listener{Protocol: listing{tables: []*core.OpTable{tb}}}); err != nil {
		t.Fatal(err)
	}
	ts, err := sh.Table(tb, tb.Versions.First)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// scrape is m's adapter_ series: each value keyed by its name and labels, as
// name{label=value,...} with the labels in name order, a histogram's value
// its count and its sum under name_sum; and each vector's label names.
func scrape(t *testing.T, m *metrics.Registry) (values map[string]float64, labels map[string][]string) {
	t.Helper()
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	values, labels = map[string]float64{}, map[string][]string{}
	for _, mf := range mfs {
		name, ok := strings.CutPrefix(mf.GetName(), "dittofs_")
		if !ok || !strings.HasPrefix(name, "adapter_") {
			continue
		}
		for _, mt := range mf.GetMetric() {
			var names, pairs []string
			for _, lp := range mt.GetLabel() {
				names = append(names, lp.GetName())
				pairs = append(pairs, lp.GetName()+"="+lp.GetValue())
			}
			labels[name] = names
			key := "{" + strings.Join(pairs, ",") + "}"
			values[name+key] = mt.GetCounter().GetValue() + mt.GetGauge().GetValue() + float64(mt.GetHistogram().GetSampleCount())
			if h := mt.GetHistogram(); h != nil {
				values[name+"_sum"+key] = h.GetSampleSum()
			}
		}
	}
	return values, labels
}

// valuesOf is the values label takes among got's series of name, sorted.
func valuesOf(got map[string]float64, name, label string) []string {
	var vs []string
	for k := range got {
		if rest, ok := strings.CutPrefix(k, name+"{"); ok {
			for _, pair := range strings.Split(strings.TrimSuffix(rest, "}"), ",") {
				if v, ok := strings.CutPrefix(pair, label+"="); ok && !slices.Contains(vs, v) {
					vs = append(vs, v)
				}
			}
		}
	}
	slices.Sort(vs)
	return vs
}

func sortedValues(m map[int]string) []string { return slices.Sorted(maps.Values(m)) }

// orderedValues is m's values in its keys' order: a stage value's key is its
// place in §10.1's list.
func orderedValues(m map[int]string) []string {
	keys := slices.Sorted(maps.Keys(m))
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = m[k]
	}
	return out
}

// reasonOf is the index of a reason label value, as the connection-series
// hooks take it.
func reasonOf(t *testing.T, name string) int {
	t.Helper()
	for r, n := range core.ReasonNames() {
		if n == name {
			return r
		}
	}
	t.Fatalf("no close reason named %s", name)
	return 0
}

// Each record method moves its own series, under its own labels: the
// in-flight count from the connection's label to the table's at Route.
func TestSeriesRecordEachIntoItsOwnSeries(t *testing.T) {
	m := metrics.New("v", "c")
	sh := core.NewShared(adapter.Deps{Metrics: m}, nil)
	tb := routed(t)[0]
	ts := seriesOf(t, sh, tb)
	ls := sh.Label("before")
	const p = "{protocol=" + testproto.Name + "}"
	for _, step := range []struct {
		name string
		do   func()
		want map[string]float64
	}{
		{"admitted", ls.Admitted, map[string]float64{"adapter_requests_in_flight{protocol=before}": 1}},
		{"routed", func() { ts.Routed(ls) }, map[string]float64{"adapter_requests_in_flight{protocol=before}": 0, "adapter_requests_in_flight" + p: 1}},
		{"answered from Route on", ts.Answered, map[string]float64{"adapter_requests_in_flight" + p: 0}},
		{"admitted again", ls.Admitted, map[string]float64{"adapter_requests_in_flight{protocol=before}": 1}},
		{"answered before Route", ls.Answered, map[string]float64{"adapter_requests_in_flight{protocol=before}": 0}},
		{"a replay hit", ls.ReplayHit, map[string]float64{"adapter_replay_hits_total{protocol=before}": 1}},
		{"an unmapped error", ts.UnmappedErr, map[string]float64{"adapter_unmapped_errors_total" + p: 1}},
		{"held", func() { ts.Held(100) }, map[string]float64{"adapter_held_requests" + p: 1, "adapter_held_bytes" + p: 100}},
		{"released", func() { ts.Released(100) }, map[string]float64{"adapter_held_requests" + p: 0, "adapter_held_bytes" + p: 0}},
		{"a hold refused", ts.HoldRefused, map[string]float64{"adapter_hold_refusals_total" + p: 1}},
		{"the hold pool charged", func() { sh.HoldPoolCharged(64) }, map[string]float64{"adapter_hold_pool_bytes{}": 64}},
	} {
		step.do()
		got, _ := scrape(t, m)
		for k, v := range step.want {
			if got[k] != v {
				t.Errorf("after %s, %s = %v, want %v", step.name, k, got[k], v)
			}
		}
	}

	// Each of the labelled ones once per value, the label's before Route and
	// the table's from it.
	refusals, wire := core.StageNames()
	for st := range refusals {
		ls.Refused(st)
		ts.Refused(st)
	}
	// A connection's own series too: in at accept, moved to established,
	// closed at a reason, refused at one at accept, and one byte each way.
	ls.Accepted()
	ls.Established()
	ls.Closed(reasonOf(t, "client"), true)
	ls.RefusedAtAccept(reasonOf(t, "pool"))
	ls.BytesIn().Add(100)
	ls.BytesOut().Add(50)
	for st := range wire {
		ls.Observe(st, time.Millisecond)
		ts.Observe(st, time.Millisecond)
	}
	for i := range core.SplitReasons {
		ts.Split(i)
	}
	for _, op := range tb.Ops {
		ts.Panicked(op)
	}
	mapped := map[string]float64{}
	for _, err := range vfs.Errors() {
		ts.StatusMapped(err)
		for sentinel, name := range core.ServiceErrorNames() {
			if errors.Is(err, sentinel) {
				mapped[name]++
			}
		}
	}
	ts.StatusMapped(core.ErrDecode)
	got, _ := scrape(t, m)
	want := map[string]float64{}
	for _, protocol := range []string{"before", testproto.Name} {
		for _, name := range refusals {
			want["adapter_refusals_total{protocol="+protocol+",stage="+name+"}"] = 1
		}
		for _, name := range wire {
			want["adapter_wire_seconds{protocol="+protocol+",stage="+name+"}"] = 1
			want["adapter_wire_seconds_sum{protocol="+protocol+",stage="+name+"}"] = time.Millisecond.Seconds()
		}
	}
	for _, reason := range core.SplitReasons {
		want["adapter_chain_splits_total{protocol="+testproto.Name+",reason="+reason+"}"] = 1
	}
	for _, op := range tb.Ops {
		want["adapter_handler_panics_total{op="+strings.ToLower(op.Name)+",protocol="+testproto.Name+"}"] = 1
	}
	for _, name := range core.ServiceErrorNames() {
		want["adapter_status_mapped_total{error="+name+",protocol="+testproto.Name+"}"] = mapped[name]
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v after one record of each, want %v", k, got[k], v)
		}
	}
	// A connection's own series, under the label's ("before"): in at accept,
	// moved to established and closed there, one refused at accept, and one
	// byte each way - each into its own, the state gauge back at its level.
	for k, v := range map[string]float64{
		"adapter_connections{protocol=before,state=handshake}":           0,
		"adapter_connections{protocol=before,state=established}":         0,
		"adapter_connection_closes_total{protocol=before,reason=client}": 1,
		"adapter_connection_closes_total{protocol=before,reason=pool}":   1,
		"adapter_bytes_total{direction=in,protocol=before}":              100,
		"adapter_bytes_total{direction=out,protocol=before}":             50,
	} {
		if got[k] != v {
			t.Errorf("%s = %v after a connection's own moves, want %v", k, got[k], v)
		}
	}
}

// Every series carries only the labels §10.1 gives it, and each label only
// its closed set of values: the refusing stages, the timed ones, the table's
// operations lower-cased, the service's errors, the reasons of a split.
func TestSeriesCarryOnlyTheirLabels(t *testing.T) {
	m := metrics.New("v", "c")
	sh := core.NewShared(adapter.Deps{Metrics: m}, nil)
	tb := routed(t)[0]
	seriesOf(t, sh, tb)
	ls := sh.Label(testproto.Name)
	// One record of each connection series, so each vector's children exist to
	// check their labels: a connection in and moved to established, closed at a
	// reason, one refused at accept, one byte each way.
	ls.Accepted()
	ls.Established()
	ls.Closed(reasonOf(t, "client"), true)
	ls.RefusedAtAccept(reasonOf(t, "pool"))
	ls.BytesIn().Add(1)
	ls.BytesOut().Add(1)
	values, got := scrape(t, m)
	want := map[string][]string{
		"adapter_requests_in_flight":      {"protocol"},
		"adapter_refusals_total":          {"protocol", "stage"},
		"adapter_replay_hits_total":       {"protocol"},
		"adapter_wire_seconds":            {"protocol", "stage"},
		"adapter_handler_panics_total":    {"op", "protocol"},
		"adapter_unmapped_errors_total":   {"protocol"},
		"adapter_status_mapped_total":     {"error", "protocol"},
		"adapter_held_requests":           {"protocol"},
		"adapter_held_bytes":              {"protocol"},
		"adapter_hold_pool_bytes":         nil,
		"adapter_hold_refusals_total":     {"protocol"},
		"adapter_chain_splits_total":      {"protocol", "reason"},
		"adapter_reads_shortened_total":   {"protocol"},
		"adapter_connections":             {"protocol", "state"},
		"adapter_connection_closes_total": {"protocol", "reason"},
		"adapter_bytes_total":             {"direction", "protocol"},
	}
	for name, labels := range want {
		if l, ok := got[name]; !ok || !slices.Equal(l, labels) {
			t.Errorf("%s has labels %v (scraped: %v), want %v", name, l, ok, labels)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s is not a series of the request path", name)
		}
	}
	refusals, wire := core.StageNames()
	var ops []string
	for _, op := range tb.Ops {
		ops = append(ops, strings.ToLower(op.Name))
	}
	slices.Sort(ops)
	for _, c := range []struct {
		name, label string
		want        []string
	}{
		{"adapter_refusals_total", "stage", sortedValues(refusals)},
		{"adapter_wire_seconds", "stage", sortedValues(wire)},
		{"adapter_handler_panics_total", "op", ops},
		{"adapter_status_mapped_total", "error", slices.Sorted(maps.Values(core.ServiceErrorNames()))},
		{"adapter_chain_splits_total", "reason", slices.Sorted(slices.Values(core.SplitReasons[:]))},
		{"adapter_requests_in_flight", "protocol", []string{testproto.Name}},
		{"adapter_connections", "state", sortedValues(core.StateNames())},
		{"adapter_connection_closes_total", "reason", sortedValues(core.ReasonNames())},
		{"adapter_bytes_total", "direction", []string{"in", "out"}},
	} {
		if vs := valuesOf(values, c.name, c.label); !slices.Equal(vs, c.want) {
			t.Errorf("%s's %s takes %v, want %v", c.name, c.label, vs, c.want)
		}
	}
}

// The stage values are §10.1's: a refusal counts under the stages that
// refuse, map_error among them for what core refuses once the call has
// returned, and reply_too_big; adapter_wire_seconds observes decode_header
// to send but call_service.
// The stage values take §10.1's order: reply_wait between decode_args and
// resolve_ids, and reply_too_big, which only a refusal takes, last.
func TestStageValuesAreInOrder(t *testing.T) {
	refusals, wire := core.StageNames()
	if got, want := orderedValues(wire), []string{"decode_header", "unwrap", "admit", "replay_check", "route", "decode_args", "reply_wait", "resolve_ids", "map_error", "encode", "wrap", "send"}; !slices.Equal(got, want) {
		t.Errorf("adapter_wire_seconds' stages, in order, are %v, want %v", got, want)
	}
	if got, want := orderedValues(refusals), []string{"decode_header", "unwrap", "admit", "replay_check", "route", "decode_args", "reply_wait", "resolve_ids", "map_error", "reply_too_big"}; !slices.Equal(got, want) {
		t.Errorf("a refusal's stages, in order, are %v, want %v", got, want)
	}
}

// The error label names every sentinel of the service's closed set once,
// read from Errors()'s own source (§7.3): its one []error literal of bare
// identifiers, which it returns first and in that order, each named as RFC 25
// §2.4 spells it, under names unique and of lower-case letters, digits and
// underscores. Every value it returns past them matches exactly one of them
// under errors.Is without being it, so a sentinel added outside the literal
// fails too.
func TestServiceErrorsNameEverySentinel(t *testing.T) {
	idents := sentinelLiteral(t, filepath.Join("..", "..", "..", "pkg", "vfs"))
	all, names := vfs.Errors(), core.ServiceErrorNames()
	if len(all) < len(idents) || len(names) != len(idents) {
		t.Fatalf("Errors() returns %d values and the label names %d sentinels, for a literal of %d", len(all), len(names), len(idents))
	}
	label := regexp.MustCompile(`^[a-z0-9_]+$`)
	seen := map[string]bool{}
	for i, id := range idents {
		want := snake(strings.TrimPrefix(id, "Err"))
		if got := names[all[i]]; got != want {
			t.Errorf("Errors()[%d], %s, is named %q, want %q", i, id, got, want)
		}
		if !label.MatchString(want) || seen[want] {
			t.Errorf("%s's name %q is not a label value, or is taken twice", id, want)
		}
		seen[want] = true
	}
	sentinels := all[:len(idents)]
	for _, err := range all[len(idents):] {
		n := 0
		for _, s := range sentinels {
			if err == s {
				t.Errorf("%v follows the literal and is one of its sentinels", err)
			}
			if errors.Is(err, s) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%v matches %d of the literal's sentinels, want 1", err, n)
		}
	}
}

// sentinelLiteral is the identifiers of the one []error composite literal in
// the Errors function of the package in dir, in order; a second such literal,
// or an element that is not a bare identifier, fails the test.
func sentinelLiteral(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Go files in %s: %v", dir, err)
	}
	var lits []*ast.CompositeLit
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "Errors" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if cl, ok := n.(*ast.CompositeLit); ok {
					if at, ok := cl.Type.(*ast.ArrayType); ok && at.Len == nil {
						if el, ok := at.Elt.(*ast.Ident); ok && el.Name == "error" {
							lits = append(lits, cl)
						}
					}
				}
				return true
			})
		}
	}
	if len(lits) != 1 {
		t.Fatalf("Errors() holds %d []error literals, want one", len(lits))
	}
	idents := make([]string, len(lits[0].Elts))
	for i, e := range lits[0].Elts {
		id, ok := e.(*ast.Ident)
		if !ok {
			t.Fatalf("element %d of Errors()'s literal is a %T, want a bare identifier", i, e)
		}
		idents[i] = id.Name
	}
	return idents
}

// snake spells an identifier as RFC 25 §2.4's label values: a run of
// capitals is one word, its last capital starting the next word where a
// lower-case letter follows, and digits join the word before them.
func snake(id string) string {
	r := []rune(id)
	var b strings.Builder
	for i, c := range r {
		next := i+1 < len(r) && unicode.IsLower(r[i+1])
		if i > 0 && unicode.IsUpper(c) && (unicode.IsLower(r[i-1]) || next) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(c))
	}
	return b.String()
}

// A server resolves each table's series once, testproto's tables listed by
// two listeners among them, and a label's once; two tables under one label
// share their series, and so does a stages label of that name; a compound
// row's Members are resolved with their table.
func TestSeriesResolvedOncePerServer(t *testing.T) {
	p := testproto.New(adapter.Deps{})
	tables := p.Tables()
	resolve := func(ls ...core.Listener) (*core.Shared, *metrics.Registry, *atomic.Int64) {
		t.Helper()
		m := metrics.New("v", "c")
		sh := core.NewShared(adapter.Deps{Metrics: m}, nil)
		n := new(atomic.Int64)
		core.CountLookups(sh, n)
		if err := core.ResolveTables(sh, ls...); err != nil {
			t.Fatal(err)
		}
		return sh, m, n
	}
	sh, m, n := resolve(core.Listener{Protocol: p}, core.Listener{Protocol: p.Bare()})
	v1, err := sh.Table(tables[0], 1)
	if err != nil {
		t.Fatal(err)
	}
	if sh.Resolved(tables[0].Ops[testproto.OpCompound].Members) == nil {
		t.Error("a compound row's Members are not resolved with their table")
	}
	v2, err := sh.Table(tables[1], 2)
	if err != nil {
		t.Fatal(err)
	}
	ls := sh.Label(testproto.Name)
	before := n.Load()
	if sh.Label(testproto.Name) != ls || n.Load() != before {
		t.Error("a label resolved twice")
	}
	ls.Admitted()
	v1.Routed(ls)
	ls.Admitted()
	v2.Routed(ls)
	ls.Admitted()
	got, _ := scrape(t, m)
	if k := "adapter_requests_in_flight{protocol=" + testproto.Name + "}"; got[k] != 3 {
		t.Errorf("%s = %v with one request under each table and one under the label, want 3: one series", k, got[k])
	}
}

// NewServer refuses, naming it, a listed table with no Protocol and one
// whose Versions has First past Last; and two listed tables of one Protocol
// that both hold a row name, whether neither sets Versions, one does, or
// both do with ranges that overlap, a compound row's Members among them,
// which take their table's Protocol and Versions. A refused server registers
// no series. It accepts testproto's tables, whose version 1 compound row's
// Members hold that table's names, one table two listeners list, checked
// once, two tables of different Protocols that share a name, and two
// compound rows of one table that name one Members table, one table.
func TestNewServerChecksTheListedTables(t *testing.T) {
	p := testproto.New(adapter.Deps{})
	v1, v2 := p.Tables()[0], p.Tables()[1]
	with := func(tb *core.OpTable, edits ...func(*core.OpTable)) *core.OpTable {
		c := *tb
		for _, edit := range edits {
			edit(&c)
		}
		return &c
	}
	at := func(first, last core.Version) func(*core.OpTable) {
		return func(c *core.OpTable) { c.Versions = &core.Range{First: first, Last: last} }
	}
	every := func(c *core.OpTable) { c.Versions = nil }
	only := func(code uint32) func(*core.OpTable) {
		return func(c *core.OpTable) { c.Ops = map[uint32]*core.Op{code: c.Ops[code]} }
	}
	lists := func(tables ...*core.OpTable) []core.Listener {
		return []core.Listener{tcp(listing{p, tables}, 0, "a")}
	}
	twoCompounds := func(c *core.OpTable) {
		second := *c.Ops[testproto.OpCompound]
		second.Code, second.Name = 1<<20, "Compound2"
		c.Ops = maps.Clone(c.Ops)
		c.Ops[second.Code] = &second
	}
	for _, c := range []struct {
		name   string
		ls     []core.Listener
		refuse string // what the refusal names; empty: accepted
	}{
		{"testproto's tables", []core.Listener{tcp(p, 0, "a")}, ""},
		{"one table two listeners list", []core.Listener{tcp(p, 0, "a"), tcp(p.Bare(), 0, "b")}, ""},
		{"two tables of different Protocols", lists(v1, with(v2, every, func(c *core.OpTable) { c.Protocol = "alt" })), ""},
		{"the later version's table listed first", lists(v2, v1), ""},
		{"two compound rows naming one Members table", lists(with(v1, twoCompounds)), ""},
		{"a table with no Protocol", lists(with(v1, only(testproto.OpEcho), func(c *core.OpTable) { c.Protocol = "" })), "the table of Echo has no Protocol"},
		{"Versions first past last", lists(with(v2, only(testproto.OpEcho), at(2, 1))), "the table of Echo has Versions 2 to 1, first past last"},
		{"two tables, neither setting Versions", lists(with(v2, every), with(v2, every)), "both hold the row"},
		{"two tables, one setting Versions", lists(with(v2, every), v2), "both hold the row"},
		{"two tables whose Versions overlap", lists(with(v2, at(1, 2)), v2), "both hold the row"},
		{"a member's name in another table of its versions", lists(with(v1, only(testproto.OpCompound)), with(v2, only(testproto.OpEcho), at(1, 1))), "both hold the row echo"},
	} {
		t.Run(c.name, func(t *testing.T) {
			deps := newDeps()
			m := metrics.New("v", "c")
			deps.Metrics = m
			srv, err := core.NewServer(deps, on("127.0.0.1"), c.ls...)
			switch {
			case c.refuse == "" && err != nil:
				t.Fatalf("NewServer refused valid tables: %v", err)
			case c.refuse == "":
				stopAtEnd(t, srv)
			case err == nil || !strings.Contains(err.Error(), c.refuse):
				t.Errorf("NewServer: %v; want a refusal naming %s", err, c.refuse)
			}
			if got, _ := scrape(t, m); c.refuse != "" && len(got) != 0 {
				t.Errorf("a refused server registered %d series, want none", len(got))
			}
		})
	}
}

// Two servers on one registry, as a node's adapters are, register each
// vector once and record into the same series.
func TestSeriesTwoServersOnOneRegistry(t *testing.T) {
	m := metrics.New("v", "c")
	tb := routed(t)[0]
	for range 2 {
		seriesOf(t, core.NewShared(adapter.Deps{Metrics: m}, nil), tb).UnmappedErr()
	}
	got, _ := scrape(t, m)
	if k := "adapter_unmapped_errors_total{protocol=" + testproto.Name + "}"; got[k] != 2 {
		t.Errorf("%s = %v after one record on each server, want 2", k, got[k])
	}
}

// One table on two servers, each with a registry of its own, records into
// each server's own: the series live with the server, not the table.
func TestSeriesOneTableOnTwoRegistries(t *testing.T) {
	tb := routed(t)[0]
	ms := [2]*metrics.Registry{metrics.New("v", "c"), metrics.New("v", "c")}
	for i, m := range ms {
		ts := seriesOf(t, core.NewShared(adapter.Deps{Metrics: m}, nil), tb)
		for range i + 1 {
			ts.UnmappedErr()
		}
	}
	k := "adapter_unmapped_errors_total{protocol=" + testproto.Name + "}"
	for i, m := range ms {
		if got, _ := scrape(t, m); got[k] != float64(i+1) {
			t.Errorf("server %d's registry reads %s = %v, want its own %d", i, k, got[k], i+1)
		}
	}
}

// A compound member's panic counts under its own operation, with its
// parent's label; an operation outside the table counts nothing and does
// not crash.
func TestSeriesPanicsByOperation(t *testing.T) {
	m := metrics.New("v", "c")
	sh := core.NewShared(adapter.Deps{Metrics: m}, nil)
	tb := routed(t)[0]
	ts := seriesOf(t, sh, tb)
	members := tb.Ops[testproto.OpCompound].Members
	ms := sh.Resolved(members)
	member := members.Ops[testproto.OpEcho]
	ms.Panicked(member)
	ts.Panicked(&core.Op{Code: 1 << 30, Name: "Absent"})
	got, _ := scrape(t, m)
	if k := "adapter_handler_panics_total{op=" + strings.ToLower(member.Name) + ",protocol=" + testproto.Name + "}"; got[k] != 1 {
		t.Errorf("%s = %v after one member's panic, want 1", k, got[k])
	}
	if k := "adapter_handler_panics_total{op=absent,protocol=" + testproto.Name + "}"; got[k] != 0 {
		t.Errorf("%s = %v, want no series for an operation outside the table", k, got[k])
	}
}

// spent is a typed service error, which counts under its sentinel.
var spent error = &vfs.Delay{Cause: vfs.DelayResources}

// record calls every record method once, leaving each gauge where it was.
// client and pool are the close reasons of its connection series, resolved
// once outside it.
func record(sh *core.Shared, ts *core.TableSeries, ls *core.LabelSeries, op *core.Op, client, pool int) {
	ls.Admitted()
	ts.Routed(ls)
	ts.Answered()
	ls.Admitted()
	ls.Answered()
	ls.ReplayHit()
	ls.Refused(0)
	ts.Refused(0)
	ls.Observe(0, time.Millisecond)
	ts.Observe(0, time.Millisecond)
	// A connection's own series, net zero on the gauge: in at accept, moved
	// to established and closed there; a close at accept moves no gauge.
	ls.Accepted()
	ls.Established()
	ls.Closed(client, true)
	ls.RefusedAtAccept(pool)
	ls.BytesIn().Add(1)
	ls.BytesOut().Add(1)
	ts.UnmappedErr()
	ts.Panicked(op)
	ts.StatusMapped(spent)
	ts.Held(100)
	ts.Released(100)
	ts.HoldRefused()
	ts.Split(0)
	sh.HoldPoolCharged(64)
	sh.HoldPoolCharged(-64)
}

// Recording on resolved series allocates nothing, with a registry and with
// none, and nor does finding the series Route finds once per request.
func TestSeriesRecordAllocatesNothing(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	for name, d := range map[string]adapter.Deps{"a registry": {Metrics: metrics.New("v", "c")}, "no registry": {}} {
		sh := core.NewShared(d, nil)
		tb := routed(t)[0]
		ts := seriesOf(t, sh, tb)
		ls := sh.Label(testproto.Name)
		op := tb.Ops[testproto.OpEcho]
		client, pool := reasonOf(t, "client"), reasonOf(t, "pool")
		if n := testing.AllocsPerRun(100, func() { record(sh, ts, ls, op, client, pool) }); n != 0 {
			t.Errorf("with %s, recording allocates %v times per run, want 0", name, n)
		}
		if n := testing.AllocsPerRun(100, func() {
			_, _ = sh.Table(tb, 1)
			sh.Label(testproto.Name)
		}); n != 0 {
			t.Errorf("with %s, finding a resolved table's and label's series allocates %v times per run, want 0", name, n)
		}
	}
}

// Recording looks up no child: every one was resolved with its series.
func TestSeriesRecordLooksNothingUp(t *testing.T) {
	sh := core.NewShared(adapter.Deps{Metrics: metrics.New("v", "c")}, nil)
	var n atomic.Int64
	core.CountLookups(sh, &n)
	tb := routed(t)[0]
	ts := seriesOf(t, sh, tb)
	ls := sh.Label(testproto.Name)
	resolved := n.Load()
	record(sh, ts, ls, tb.Ops[testproto.OpEcho], reasonOf(t, "client"), reasonOf(t, "pool"))
	if n.Load() != resolved {
		t.Errorf("recording looked up %d children, want none", n.Load()-resolved)
	}
}
