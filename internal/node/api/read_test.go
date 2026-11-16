package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/google/uuid"
)

// eventually waits until cond holds, polling it, with one generous backstop.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	backstop := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(backstop) {
			t.Fatalf("%s: never happened", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (s *served) netgroups(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := s.store.PutNetgroup(t.Context(), entity.Netgroup{Name: n, Members: []string{n + ".example"}}, 0); err != nil {
			t.Fatal(err)
		}
	}
}

type tagPage struct {
	Items []struct {
		Name string `json:"name"`
	} `json:"items"`
	Next string `json:"next"`
}

// tags lists the test listing with query q, failing unless it answers 200.
func (s *served) tags(t *testing.T, q string) (names []string, next string) {
	t.Helper()
	r := s.do(t, http.MethodGet, "/api/v1/tags"+q, "")
	if r.status != http.StatusOK {
		t.Fatalf("GET tags%s: answered %d %s, want 200", q, r.status, r.body)
	}
	var p tagPage
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatal(err)
	}
	for _, it := range p.Items {
		names = append(names, it.Name)
	}
	return names, p.Next
}

// A listing resumes from its cursor's key: records inserted behind the
// cursor between pages are not read, and every record there when the
// listing started is read once.
func TestListingResumesFromKey(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	s.netgroups(t, "b", "d", "f", "h")
	var got [][]string
	names, next := s.tags(t, "?limit=2")
	got = append(got, names)
	s.netgroups(t, "a", "c")
	for next != "" {
		if len(got) == 10 {
			t.Fatalf("listing four records two a page: still paging after %d pages, got %q", len(got), got)
		}
		names, next = s.tags(t, "?limit=2&after="+url.QueryEscape(next))
		got = append(got, names)
	}
	want := [][]string{{"b", "d"}, {"f", "h"}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("pages with a and c inserted behind the cursor: %q, want %q", got, want)
	}
}

// ?limit= is 100 when absent and from 1 to 1,000; next is set only when an
// item follows the page.
func TestListingLimit(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	var all []string
	for i := range 101 {
		all = append(all, fmt.Sprintf("n%03d", i))
	}
	s.netgroups(t, all...)
	for _, tc := range []struct {
		q        string
		items    int
		wantNext bool
	}{
		{"", 100, true},
		{"?limit=1", 1, true},
		{"?limit=101", 101, false},
		{"?limit=1000", 101, false},
	} {
		names, next := s.tags(t, tc.q)
		if len(names) != tc.items || (next != "") != tc.wantNext {
			t.Errorf("GET tags%s: %d items, next %q; want %d items, next set %v", tc.q, len(names), next, tc.items, tc.wantNext)
		}
	}
	for _, q := range []string{"?limit=0", "?limit=1001", "?limit=-1", "?limit=ten", "?limit=1&limit=2", "?after=AQ&after=AQ"} {
		r := s.do(t, http.MethodGet, "/api/v1/tags"+q, "")
		field, _, _ := bytes.Cut([]byte(q[1:]), []byte("="))
		if p := r.problem(t); r.status != http.StatusBadRequest || p.Field != string(field) {
			t.Errorf("GET tags%s: answered %d field %q, want 400 naming %s", q, r.status, p.Field, field)
		}
	}
}

// A listing offers filters on the fields it indexes only: any other query
// parameter is refused, naming it.
func TestListingRefusesUnindexedFilter(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	for _, q := range []string{"members", "wait", "Name"} {
		r := s.do(t, http.MethodGet, "/api/v1/tags?"+q+"=x", "")
		if p := r.problem(t); r.status != http.StatusBadRequest || p.Field != q {
			t.Errorf("GET tags?%s=x: answered %d field %q, want 400 naming %s", q, r.status, p.Field, q)
		}
	}
}

// A name filter answers a list of zero or one.
func TestNameFilter(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	s.netgroups(t, "b", "d", "f")
	for _, tc := range []struct {
		name string
		want []string
	}{{"d", []string{"d"}}, {"e", nil}, {"z", nil}} {
		if got, next := s.tags(t, "?name="+tc.name); !slices.Equal(got, tc.want) || next != "" {
			t.Errorf("GET tags?name=%s: %q next %q, want %q and no next", tc.name, got, next, tc.want)
		}
	}
	if r := s.do(t, http.MethodGet, "/api/v1/tags?name=e", ""); !bytes.Contains(r.body, []byte(`"items":[]`)) {
		t.Errorf("GET tags?name=e: answered %s, want an empty items list", r.body)
	}
}

// A secret-bearing field reads as the reference it was sealed under and
// its version, never the value written.
func TestSecretReadsAsReference(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	id := uuid.NewString()
	const secret = "hunter2-6f1d"
	if r := s.do(t, http.MethodPut, "/api/v1/vaults/"+id, `{"spec":{"password":"`+secret+`"}}`, "If-None-Match", "*"); r.status != http.StatusOK {
		t.Fatalf("PUT vault: answered %d %s", r.status, r.body)
	}
	r := s.do(t, http.MethodGet, "/api/v1/vaults/"+id, "")
	if r.status != http.StatusOK || bytes.Contains(r.body, []byte(secret)) ||
		!bytes.Contains(r.body, []byte(`"password":"sealed:`)) || !bytes.Contains(r.body, []byte(`"generation":1`)) {
		t.Errorf("GET vault: answered %d %s, want 200 with the sealed reference at generation 1 and never the password", r.status, r.body)
	}
}

type polled struct {
	reply
	err error
}

// poll sends GET path under ctx in the background and delivers its answer.
func (s *served) poll(ctx context.Context, path string) <-chan polled {
	ch := make(chan polled, 1)
	go func() {
		r, err := s.send(ctx, http.MethodGet, path, "")
		ch <- polled{r, err}
	}()
	return ch
}

// settle waits until the poll ch answers or parks, the clock then holding
// more than parked waits, and returns its answer, the zero one if it parked.
func (s *served) settle(t *testing.T, what string, ch <-chan polled, parked int) polled {
	t.Helper()
	var p polled
	eventually(t, what+" answers or parks", func() bool {
		select {
		case p = <-ch:
			return true
		default:
			return s.clock.Pending() > parked
		}
	})
	return p
}

func answered(t *testing.T, what string, ch <-chan polled) polled {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: no answer", what)
		return polled{}
	}
}

func (s *served) putKnob(t *testing.T, id, value string, expect uint64) {
	t.Helper()
	sid, err := entity.ParseShareID(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.PutSetting(t.Context(), entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeNode, ID: sid}, Key: "knob", Value: []byte(`{"value":"` + value + `"}`)}, expect); err != nil {
		t.Fatal(err)
	}
}

// A parked poll re-reads once a second on the API's clock: a change made
// while it is parked is answered on the next second's advance, and with
// none it answers what it read once its wait has passed.
func TestParkedPollAnswersOnTheClock(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	id := uuid.NewString()
	s.putKnob(t, id, "a", 0)
	base := s.clock.Pending()

	ch := s.poll(t.Context(), knobAt(id)+"?wait=30s")
	eventually(t, "the poll parks", func() bool { return s.clock.Pending() == base+1 })
	s.putKnob(t, id, "b", 1)
	select {
	case p := <-ch:
		t.Fatalf("poll answered %d %s before the clock moved, want it parked", p.status, p.body)
	default:
	}
	s.clock.Advance(time.Second)
	if p := answered(t, "poll after the change and a second", ch); p.err != nil || p.status != http.StatusOK || string(p.body) != `{"generation":2,"spec":{"value":"b"}}` {
		t.Errorf("poll after the change: answered %d %s err %v, want 200 at generation 2", p.status, p.body, p.err)
	}

	ch = s.poll(t.Context(), knobAt(id)+"?wait=5s")
	eventually(t, "the second poll parks", func() bool { return s.clock.Pending() == base+1 })
	s.clock.Advance(5 * time.Second)
	if p := answered(t, "poll past its wait", ch); p.err != nil || p.status != http.StatusOK || string(p.body) != `{"generation":2,"spec":{"value":"b"}}` {
		t.Errorf("poll past its wait: answered %d %s err %v, want 200 with the unchanged knob", p.status, p.body, p.err)
	}
}

// ?wait= is a duration above 0 and at most 60 s, given once, and a read
// takes no other query parameter.
func TestWaitBounds(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	for _, tc := range []struct{ q, field string }{
		{"wait=61s", "wait"}, {"wait=0s", "wait"}, {"wait=-1s", "wait"}, {"wait=soon", "wait"},
		{"wait=5s&wait=junk", "wait"}, {"wiat=5s", "wiat"},
	} {
		// Settled, so a wait accepted in error parks and is reported, not
		// waited out.
		r := s.settle(t, "GET knob?"+tc.q, s.poll(t.Context(), knobAt(uuid.NewString())+"?"+tc.q), s.clock.Pending())
		var p apiv1.Problem
		if err := json.Unmarshal(r.body, &p); err != nil || r.status != http.StatusBadRequest || p.Field != tc.field {
			t.Errorf("GET knob?%s: answered %d %s, want 400 naming %s", tc.q, r.status, r.body, tc.field)
		}
	}
	if r := s.do(t, http.MethodGet, knobAt(uuid.NewString()), ""); r.status != http.StatusOK || string(r.body) != `{"generation":0,"spec":{"value":""}}` {
		t.Errorf("GET knob with no wait: answered %d %s, want 200 at once", r.status, r.body)
	}
}

// A node parks at most 256 polls and refuses the next 503 unavailable.
func TestParkedPollsAreBounded(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	id := uuid.NewString()
	base := s.clock.Pending()
	var parked []<-chan polled
	for range 256 {
		parked = append(parked, s.poll(t.Context(), knobAt(id)+"?wait=30s"))
	}
	eventually(t, "256 polls park", func() bool { return s.clock.Pending() == base+256 })
	next := s.poll(t.Context(), knobAt(id)+"?wait=30s")
	refused := s.settle(t, "the 257th poll", next, base+256)
	s.clock.Advance(30 * time.Second)
	if refused.status == 0 {
		refused = answered(t, "the 257th poll", next)
	}
	var p apiv1.Problem
	if err := json.Unmarshal(refused.body, &p); err != nil || refused.status != http.StatusServiceUnavailable || p.Code != apiv1.CodeUnavailable {
		t.Errorf("the 257th poll: answered %d %s, want 503 unavailable", refused.status, refused.body)
	}
	for i, ch := range parked {
		if p := answered(t, "a parked poll", ch); p.status != http.StatusOK {
			t.Errorf("parked poll %d past its wait: answered %d %s, want 200", i, p.status, p.body)
		}
	}
	// Each answered poll gave its slot back, so the next one parks.
	if early := s.settle(t, "a poll after the 256 drained", s.poll(t.Context(), knobAt(id)+"?wait=30s"), base); early.status != 0 {
		t.Errorf("a poll after the 256 drained: answered %d %s, want it parked", early.status, early.body)
	}
}

// A parked poll outlives the write timeout that bounds every other call.
func TestParkedPollOutlivesWriteTimeout(t *testing.T) {
	const timeout = 200 * time.Millisecond
	s := serveWith(t, as(entity.RoleViewer, false), func(srv *api.Server) { srv.SetWriteTimeout(timeout) })
	id := uuid.NewString()
	s.putKnob(t, id, "a", 0)
	base := s.clock.Pending()
	ch := s.poll(t.Context(), knobAt(id)+"?wait=30s")
	eventually(t, "the poll parks", func() bool { return s.clock.Pending() == base+1 })
	// The write timeout is the HTTP server's, on the wall clock, which no
	// test clock moves: this bounded wait is the assertion that the parked
	// poll is still held past it.
	<-time.After(3 * timeout)
	s.putKnob(t, id, "b", 1)
	s.clock.Advance(time.Second)
	if p := answered(t, "poll past the write timeout", ch); p.err != nil || p.status != http.StatusOK || string(p.body) != `{"generation":2,"spec":{"value":"b"}}` {
		t.Errorf("poll parked past the write timeout: answered %d %s err %v, want 200 at generation 2", p.status, p.body, p.err)
	}
}

// A read reads the store: a second node over the same store reads a write
// the first one made, with nothing between them but the store.
func TestReadOnAnotherNode(t *testing.T) {
	disk := newOnDisk(t)
	second, err := kvstore.Open(t.Context(), disk.fault)
	if err != nil {
		t.Fatal(err)
	}
	caller, _ := one(entity.RoleOperator)
	first := serve(t, caller, disk.use)
	// The second node is the API alone, since a test watches one harness.
	srv := api.NewWithTestRows(api.Deps{Addr: "127.0.0.1:0", MetricsAddr: "127.0.0.1:0", Metrics: metrics.New("test", "test"),
		Logger: slog.New(slog.NewTextHandler(t.Output(), nil)), Authenticate: caller, Live: func() bool { return true },
		Store: second.ControlPlane(), Clock: first.clock, Node: testNode})
	addr, _, err := srv.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := srv.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	other := &served{url: "http://" + addr.String(), client: first.client}
	id := uuid.NewString()
	if r := other.do(t, http.MethodGet, knobAt(id), ""); r.status != http.StatusOK || string(r.body) != `{"generation":0,"spec":{"value":""}}` {
		t.Fatalf("GET knob on the second node before the write: answered %d %s, want 200 at generation 0", r.status, r.body)
	}
	if r := first.do(t, http.MethodPut, knobAt(id), `{"spec":{"value":"on"}}`, "If-None-Match", "*"); r.status != http.StatusOK {
		t.Fatalf("PUT knob on the first node: answered %d %s", r.status, r.body)
	}
	if r := other.do(t, http.MethodGet, knobAt(id), ""); r.status != http.StatusOK || string(r.body) != `{"generation":1,"spec":{"value":"on"}}` {
		t.Errorf("GET knob on the second node: answered %d %s, want 200 at generation 1", r.status, r.body)
	}
}

// A GET answers its record's tag as its ETag, which a PUT's If-Match then
// names.
func TestReadETag(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	id := uuid.NewString()
	s.putKnob(t, id, "a", 0)
	r := s.do(t, http.MethodGet, knobAt(id), "")
	if r.status != http.StatusOK || r.header.Get("ETag") != `"1"` {
		t.Fatalf("GET knob: answered %d %s ETag %q, want 200 ETag \"1\"", r.status, r.body, r.header.Get("ETag"))
	}
	if p := s.do(t, http.MethodPut, knobAt(id), `{"spec":{"value":"b"}}`, "If-Match", r.header.Get("ETag")); p.status != http.StatusOK {
		t.Errorf("PUT knob with the ETag GET read: answered %d %s, want 200", p.status, p.body)
	}
}

// A first read that already answers the wait answers at once, never
// parking, and a row may take a wait word that names no duration.
func TestWaitAlreadyDone(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	id := uuid.NewString()
	s.putKnob(t, id, api.KnobOn, 0)
	base := s.clock.Pending()
	p := s.settle(t, "GET knob?wait=on with the knob on", s.poll(t.Context(), knobAt(id)+"?wait="+api.KnobOn), base)
	if p.err != nil || p.status != http.StatusOK || string(p.body) != `{"generation":1,"spec":{"value":"on"}}` || s.clock.Pending() != base {
		t.Fatalf("GET knob?wait=on with the knob on: answered %d %s err %v, %d waits on the clock; want 200 at generation 1 and %d waits", p.status, p.body, p.err, s.clock.Pending(), base)
	}

	other := uuid.NewString()
	s.putKnob(t, other, "off", 0)
	ch := s.poll(t.Context(), knobAt(other)+"?wait="+api.KnobOn)
	eventually(t, "the poll on the knob that is off parks", func() bool { return s.clock.Pending() == base+1 })
	s.putKnob(t, other, api.KnobOn, 1)
	s.clock.Advance(time.Second)
	if p := answered(t, "poll once the knob is on", ch); p.err != nil || p.status != http.StatusOK || string(p.body) != `{"generation":2,"spec":{"value":"on"}}` {
		t.Errorf("GET knob?wait=on once the knob turned on: answered %d %s err %v, want 200 at generation 2", p.status, p.body, p.err)
	}
}

// Stop answers a parked poll with its last read rather than waiting out
// its wait.
func TestStopAnswersParkedPoll(t *testing.T) {
	var srv *api.Server
	s := serveWith(t, as(entity.RoleViewer, false), func(v *api.Server) { srv = v })
	id := uuid.NewString()
	s.putKnob(t, id, "a", 0)
	base := s.clock.Pending()
	ch := s.poll(t.Context(), knobAt(id)+"?wait=30s")
	eventually(t, "the poll parks", func() bool { return s.clock.Pending() == base+1 })
	// The poll waits on the test clock, which nothing advances, so this
	// bounded wait is the assertion that Stop does not wait the poll out.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Errorf("Stop with a parked poll: %v, want nil", err)
	}
	if p := answered(t, "the parked poll on Stop", ch); p.err != nil || p.status != http.StatusOK || string(p.body) != `{"generation":1,"spec":{"value":"a"}}` {
		t.Errorf("parked poll on Stop: answered %d %s err %v, want 200 with its last read", p.status, p.body, p.err)
	}
}

// A node with no clock refuses a read that would park 503 unavailable.
func TestWaitWithNoClock(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false), func(d *api.Deps) { d.Clock = nil })
	p := answered(t, "GET knob?wait=5s with no clock", s.poll(t.Context(), knobAt(uuid.NewString())+"?wait=5s"))
	if p.err != nil || p.status != http.StatusServiceUnavailable || p.problem(t).Code != apiv1.CodeUnavailable {
		t.Errorf("GET knob?wait=5s with no clock: answered %d %s err %v, want 503 unavailable", p.status, p.body, p.err)
	}
}

// A parked poll whose caller goes away ends its call.
func TestCancelledPollEnds(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	id := uuid.NewString()
	base := s.clock.Pending()
	ctx, cancel := context.WithCancel(t.Context())
	ch := s.poll(ctx, knobAt(id)+"?wait=30s")
	eventually(t, "the poll parks", func() bool { return s.clock.Pending() == base+1 })
	cancel()
	if p := answered(t, "the cancelled poll", ch); !errors.Is(p.err, context.Canceled) {
		t.Fatalf("cancelled poll: answered %d %s err %v, want the client's cancellation", p.status, p.body, p.err)
	}
	eventually(t, "the cancelled poll's call ends", func() bool {
		for _, sp := range s.spans.Ended() {
			if sp.Name() == "GET /api/v1/knobs/{id}" {
				return true
			}
		}
		return false
	})
}

// A cursor naming a key outside its listing's range is refused, naming
// after; one inside it resumes there.
func TestCursorOutsideListing(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	s.netgroups(t, "b", "d")
	cursor := func(key string) string {
		return base64.RawURLEncoding.EncodeToString(append([]byte{1}, key...))
	}
	for _, key := range []string{"0", api.TagsTo, "~"} {
		r := s.do(t, http.MethodGet, "/api/v1/tags?after="+cursor(key), "")
		if r.status != http.StatusBadRequest || r.problem(t).Field != "after" {
			t.Errorf("GET tags after %q: answered %d %s, want 400 naming after", key, r.status, r.body)
		}
	}
	if names, _ := s.tags(t, "?after="+cursor("b")); !slices.Equal(names, []string{"d"}) {
		t.Errorf("GET tags after b: %q, want [d]", names)
	}
}
