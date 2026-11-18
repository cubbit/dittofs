package core_test

import (
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	ts, err := sh.Table(tb)
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err := sh.Table(tb); err != nil {
		t.Fatal(err)
	}
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
// refuse and reply_too_big, adapter_wire_seconds observes decode_header to
// send but call_service.
// The stage values take §10.1's order: reply_wait between decode_args and
// resolve_ids, and reply_too_big, which only a refusal takes, last.
func TestStageValuesAreInOrder(t *testing.T) {
	refusals, wire := core.StageNames()
	if got, want := orderedValues(wire), []string{"decode_header", "unwrap", "admit", "replay_check", "route", "decode_args", "reply_wait", "resolve_ids", "map_error", "encode", "wrap", "send"}; !slices.Equal(got, want) {
		t.Errorf("adapter_wire_seconds' stages, in order, are %v, want %v", got, want)
	}
	if got, want := orderedValues(refusals), []string{"decode_header", "unwrap", "admit", "replay_check", "route", "decode_args", "reply_wait", "resolve_ids", "reply_too_big"}; !slices.Equal(got, want) {
		t.Errorf("a refusal's stages, in order, are %v, want %v", got, want)
	}
}

// The error label names every sentinel of the service's closed set once, by
// its name without Err in snake case (§10.1), and each typed value of the
// set matches exactly one of them.
func TestServiceErrorsNameEverySentinel(t *testing.T) {
	names := core.ServiceErrorNames()
	want := map[error]string{
		vfs.ErrNotFound: "not_found", vfs.ErrExist: "exist", vfs.ErrNotEmpty: "not_empty",
		vfs.ErrNotDir: "not_dir", vfs.ErrIsDir: "is_dir", vfs.ErrAccess: "access",
		vfs.ErrPerm: "perm", vfs.ErrStale: "stale", vfs.ErrReadOnly: "read_only",
		vfs.ErrNameTooLong: "name_too_long", vfs.ErrInvalid: "invalid", vfs.ErrNoSpace: "no_space",
		vfs.ErrQuota: "quota", vfs.ErrLocked: "locked", vfs.ErrRangeNotLocked: "range_not_locked",
		vfs.ErrShareViolation: "share_violation", vfs.ErrDeletePending: "delete_pending", vfs.ErrGrace: "grace",
		vfs.ErrDelay: "delay", vfs.ErrBlocked: "blocked", vfs.ErrWrongSecurity: "wrong_security",
		vfs.ErrState: "state", vfs.ErrCross: "cross", vfs.ErrIncomplete: "incomplete",
		vfs.ErrInconsistent: "inconsistent", vfs.ErrLost: "lost", vfs.ErrUnavailable: "unavailable",
		vfs.ErrCorrupt: "corrupt", vfs.ErrIO: "io", vfs.ErrPastEnd: "past_end",
		vfs.ErrLogonFailure: "logon_failure", vfs.ErrUnknownPrincipal: "unknown_principal",
	}
	if !maps.Equal(names, want) {
		t.Errorf("the error label names %v, want %v", names, want)
	}
	snake := regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)
	seen := map[string]bool{}
	for err, name := range names {
		if !snake.MatchString(name) || seen[name] {
			t.Errorf("%v is named %q: not snake case, or named twice", err, name)
		}
		seen[name] = true
	}
	sentinels := 0
	for _, err := range vfs.Errors() {
		if _, typed := err.(interface{ Is(error) bool }); !typed {
			sentinels++
			if _, ok := names[err]; !ok {
				t.Errorf("the sentinel %v has no name", err)
			}
			continue
		}
		n := 0
		for sentinel := range names {
			if errors.Is(err, sentinel) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("the typed %v matches %d named sentinels, want 1", err, n)
		}
	}
	if len(names) != sentinels {
		t.Errorf("%d names for the service's %d sentinels", len(names), sentinels)
	}
}

// A server resolves a table's series once and a label's once; two tables
// under one label share their series, and so does a stages label of that
// name; a compound row's Members are resolved with their parent.
func TestSeriesResolvedOncePerServer(t *testing.T) {
	m := metrics.New("v", "c")
	sh := core.NewShared(adapter.Deps{Metrics: m}, nil)
	var n atomic.Int64
	core.CountLookups(sh, &n)
	tables := routed(t)
	members := tables[0].Ops[testproto.OpCompound].Members
	if _, err := sh.Table(members); err == nil {
		t.Fatal("a compound row's Members, which carry no Protocol, resolved on their own")
	}
	v1, err := sh.Table(tables[0])
	if err != nil {
		t.Fatal(err)
	}
	once := n.Load()
	if again, err := sh.Table(tables[0]); again != v1 || err != nil || n.Load() != once {
		t.Errorf("the second resolution of one table gave %p (%v), looking up %d children; want %p and none", again, err, n.Load()-once, v1)
	}
	if _, err := sh.Table(members); err != nil {
		t.Errorf("a compound row's Members, resolved with their parent: %v", err)
	}
	v2, err := sh.Table(tables[1])
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

// Two first resolutions of one table at once, on one server, resolve it
// once: both get the series a later call gets.
func TestSeriesTwoFirstResolutionsAtOnce(t *testing.T) {
	for range 20 {
		sh := core.NewShared(adapter.Deps{Metrics: metrics.New("v", "c")}, nil)
		tb := routed(t)[0]
		var got [2]*core.TableSeries
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range got {
			wg.Go(func() {
				<-start
				ts, err := sh.Table(tb)
				if err != nil {
					t.Errorf("Table: %v", err)
				}
				got[i] = ts
			})
		}
		close(start)
		wg.Wait()
		later, err := sh.Table(tb)
		if err != nil {
			t.Fatalf("Table: %v", err)
		}
		if got[0] != later || got[1] != later {
			t.Fatalf("two first resolutions at once gave %p and %p, and a later one %p", got[0], got[1], later)
		}
	}
}

// A table with no Protocol is refused, naming its operations, every time,
// and resolves no series.
func TestSeriesRefuseATableWithoutProtocol(t *testing.T) {
	m := metrics.New("v", "c")
	sh := core.NewShared(adapter.Deps{Metrics: m}, nil)
	tb := core.NewOpTable(table(), false, &core.Op{Code: 7, Name: "Read", Versions: core.Range{First: 1, Last: 1}})
	for range 2 {
		if _, err := sh.Table(tb); err == nil || !strings.Contains(err.Error(), "Read") {
			t.Errorf("a table with no Protocol: %v, want an error naming Read", err)
		}
	}
	if got, _ := scrape(t, m); len(got) != 1 {
		t.Errorf("a refused table left series %v, want the hold pool's alone", got)
	}
}

// Two servers on one registry, as a node's adapters are, register each
// vector once and record into the same series.
func TestSeriesTwoServersOnOneRegistry(t *testing.T) {
	m := metrics.New("v", "c")
	tb := routed(t)[0]
	for range 2 {
		ts, err := core.NewShared(adapter.Deps{Metrics: m}, nil).Table(tb)
		if err != nil {
			t.Fatal(err)
		}
		ts.UnmappedErr()
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
		ts, err := core.NewShared(adapter.Deps{Metrics: m}, nil).Table(tb)
		if err != nil {
			t.Fatal(err)
		}
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
	ts, err := sh.Table(tb)
	if err != nil {
		t.Fatal(err)
	}
	members := tb.Ops[testproto.OpCompound].Members
	ms, err := sh.Table(members)
	if err != nil {
		t.Fatal(err)
	}
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
		ts, err := sh.Table(tb)
		if err != nil {
			t.Fatal(err)
		}
		ls := sh.Label(testproto.Name)
		op := tb.Ops[testproto.OpEcho]
		client, pool := reasonOf(t, "client"), reasonOf(t, "pool")
		if n := testing.AllocsPerRun(100, func() { record(sh, ts, ls, op, client, pool) }); n != 0 {
			t.Errorf("with %s, recording allocates %v times per run, want 0", name, n)
		}
		if n := testing.AllocsPerRun(100, func() {
			_, _ = sh.Table(tb)
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
	ts, err := sh.Table(tb)
	if err != nil {
		t.Fatal(err)
	}
	ls := sh.Label(testproto.Name)
	resolved := n.Load()
	record(sh, ts, ls, tb.Ops[testproto.OpEcho], reasonOf(t, "client"), reasonOf(t, "pool"))
	if n.Load() != resolved {
		t.Errorf("recording looked up %d children, want none", n.Load()-resolved)
	}
}
