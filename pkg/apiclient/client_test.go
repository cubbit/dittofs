package apiclient_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/apiclient"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/google/uuid"
)

// eventually polls cond until it holds, failing the test after a generous
// bound.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); !cond(); {
		if time.Now().After(end) {
			t.Fatalf("%s: did not happen", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// problem is err's problem, failing the test, naming what, when it carries
// none.
func problem(t *testing.T, what string, err error) *apiv1.Problem {
	t.Helper()
	var p *apiv1.Problem
	if !errors.As(err, &p) {
		t.Fatalf("%s: error %v, want a problem", what, err)
	}
	return p
}

// keys is the Idempotency-Key each request carried.
func keys(rs []seen) []string {
	var ks []string
	for _, r := range rs {
		ks = append(ks, r.header.Get("Idempotency-Key"))
	}
	return ks
}

// New takes an https URL, an http URL on loopback or a socket's path, and
// refuses plain http to any other host.
func TestNewRefusesPlainHTTPOffLoopback(t *testing.T) {
	for _, tc := range []struct {
		server string
		ok     bool
	}{
		{"https://dfs.example:8080", true},
		{"http://127.0.0.1:8080", true},
		{"http://localhost:8080/", true},
		{"http://[::1]:8080", true},
		{"/run/dittofs/api.sock", true},
		{"http://dfs.example:8080", false},
		{"http://10.0.0.1:8080", false},
		{"ftp://127.0.0.1", false},
	} {
		if _, err := apiclient.New(tc.server); (err == nil) != tc.ok {
			t.Errorf("New(%q): error %v, want accepted %v", tc.server, err, tc.ok)
		}
	}
}

// Over http on loopback, over the socket and over https with the server's
// CA, a share is created, found by its name through the list filter, read
// with its ETag, and renamed under that ETag; https without the CA is
// refused.
func TestResourceOverEachTransport(t *testing.T) {
	s := serve(t, admin())
	tlsFront := httptest.NewTLSServer(s.front)
	t.Cleanup(tlsFront.Close)
	pool := x509.NewCertPool()
	pool.AddCert(tlsFront.Certificate())
	if _, err := client(t, tlsFront.URL, newClock()).Shares().Get(t.Context(), uuid.NewString()); err == nil || errors.As(err, new(*apiv1.Problem)) {
		t.Errorf("https without the server's CA: error %v, want the certificate refused", err)
	}
	ns := s.namespace(t)
	for _, tc := range []struct {
		name, server string
		opts         []apiclient.Option
	}{
		{"http", s.url, nil},
		{"socket", s.socket(t), nil},
		{"https", tlsFront.URL, []apiclient.Option{apiclient.WithTLS(&tls.Config{RootCAs: pool})}},
	} {
		c := client(t, tc.server, newClock(), tc.opts...)
		name := "builds-" + tc.name
		made, err := c.CreateShare(t.Context(), newShare(name, ns))
		if err != nil || made.Spec.Name != name || made.ETag == "" {
			t.Fatalf("%s: CreateShare: %+v err %v, want %s with an ETag", tc.name, made, err, name)
		}
		var found []string
		for sh, err := range c.Shares().List(t.Context(), url.Values{"name": {name}}) {
			if err != nil {
				t.Fatalf("%s: List by name: %v", tc.name, err)
			}
			found = append(found, sh.ID)
		}
		if !slices.Equal(found, []string{made.ID}) {
			t.Errorf("%s: List name=%s: %q, want [%s]", tc.name, name, found, made.ID)
		}
		got, err := c.Shares().Get(t.Context(), made.ID)
		if err != nil || got.ETag != made.ETag {
			t.Fatalf("%s: Get: ETag %q err %v, want %q", tc.name, got.ETag, err, made.ETag)
		}
		renamed, err := c.Shares().Patch(t.Context(), made.ID, map[string]any{"spec": map[string]any{"name": name + "-ci"}}, got.ETag)
		if err != nil || renamed.Spec.Name != name+"-ci" || renamed.ETag == got.ETag {
			t.Errorf("%s: Patch: %+v err %v, want renamed %s-ci under a new ETag", tc.name, renamed, err, name)
		}
	}
}

// A create whose answer is lost is sent again with the same key and body,
// and the server's replay of its record is the answer: one share.
func TestLostCreateAnswerIsOneShare(t *testing.T) {
	s := serve(t, admin())
	s.front.add(fault{match: on(http.MethodPost, "/api/v1/shares"), drop: true})
	clock := newClock()
	c := client(t, s.url, clock)
	ns := s.namespace(t)
	type created struct {
		sh  apiclient.Tagged[apiv1.ShareSpec, apiv1.ShareStatus]
		err error
	}
	done := make(chan created, 1)
	go func() {
		sh, err := c.CreateShare(t.Context(), newShare("builds", ns))
		done <- created{sh, err}
	}()
	eventually(t, "the client waits after the lost answer", func() bool { return clock.Pending() == 1 })
	clock.Advance(time.Second)
	got := <-done
	made, err := got.sh, got.err
	if err != nil {
		t.Fatalf("CreateShare with its first answer lost: %v", err)
	}
	if ids := s.shareIDs(t); !slices.Equal(ids, []string{made.ID}) {
		t.Errorf("shares after the retried create: %q, want only the answered %s", ids, made.ID)
	}
	posts := s.front.requests(http.MethodPost)
	if ks := keys(posts); len(ks) != 2 || ks[0] == "" || ks[0] != ks[1] || posts[0].body != posts[1].body {
		t.Errorf("POSTs sent: keys %q, want two sendings of one call under one key and body", ks)
	}
}

// A retryable refusal is sent again only once the Retry-After it names has
// passed on the client's clock, under the same key.
func TestRetryableRefusalWaitsRetryAfter(t *testing.T) {
	s := serve(t, admin())
	s.front.add(fault{match: on(http.MethodPost, "/api/v1/shares"), answer: &scripted{
		status: http.StatusServiceUnavailable,
		header: http.Header{"Content-Type": {apiv1.MediaTypeProblem}, "Retry-After": {"7"}},
		body:   `{"status":503,"code":"unavailable","title":"Service Unavailable","retryable":true,"request_id":"r1"}`,
	}})
	clock := newClock()
	c := client(t, s.url, clock)
	ns := s.namespace(t)
	type made struct {
		id  string
		err error
	}
	done := make(chan made, 1)
	go func() {
		sh, err := c.CreateShare(t.Context(), newShare("builds", ns))
		done <- made{sh.ID, err}
	}()
	eventually(t, "the client waits after the 503", func() bool { return clock.Pending() == 1 })
	clock.Advance(7*time.Second - time.Millisecond)
	if n := clock.Pending(); n != 1 {
		t.Fatalf("a millisecond before the Retry-After of 7 s: %d waits pending, want the client still waiting", n)
	}
	clock.Advance(time.Millisecond)
	got := <-done
	if got.err != nil {
		t.Fatalf("CreateShare after the 503: %v", got.err)
	}
	if ids := s.shareIDs(t); !slices.Equal(ids, []string{got.id}) {
		t.Errorf("shares after the retry: %q, want [%s]", ids, got.id)
	}
	if ks := keys(s.front.requests(http.MethodPost)); len(ks) != 2 || ks[0] == "" || ks[0] != ks[1] {
		t.Errorf("POSTs sent: keys %q, want two under one key", ks)
	}
}

// A write under a stale tag is refused 412 and not sent again; a PUT with
// "*" sends If-None-Match: *, refused 412 on a record that exists.
func TestStaleTagStops(t *testing.T) {
	s := serve(t, admin())
	c := client(t, s.url, newClock())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second) // a backstop: the client's clock never moves
	defer cancel()
	sh, err := c.CreateShare(ctx, newShare("builds", s.namespace(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Shares().Patch(ctx, sh.ID, map[string]any{"spec": map[string]any{"name": "ci"}}, sh.ETag); err != nil {
		t.Fatalf("Patch under the current tag: %v", err)
	}
	_, err = c.Shares().Patch(ctx, sh.ID, map[string]any{"spec": map[string]any{"name": "qa"}}, sh.ETag)
	if p := problem(t, "Patch under the stale tag", err); p.Status != http.StatusPreconditionFailed || p.Code != apiv1.CodeGenerationMismatch {
		t.Errorf("Patch under the stale tag: %d %q, want 412 generation_mismatch", p.Status, p.Code)
	}
	if n := len(s.front.requests(http.MethodPatch)); n != 2 {
		t.Errorf("PATCHes sent: %d, want 2, the refused one once", n)
	}

	pol, err := c.ExportPolicies().Get(ctx, sh.ID)
	if err != nil || pol.ETag == "" {
		t.Fatalf("Get the export policy: ETag %q err %v", pol.ETag, err)
	}
	put, err := c.ExportPolicies().Put(ctx, sh.ID, policy, pol.ETag)
	if err != nil || put.ETag == pol.ETag {
		t.Errorf("Put the export policy under its tag: ETag %q err %v, want a new tag", put.ETag, err)
	}
	_, err = c.ExportPolicies().Put(ctx, sh.ID, policy, "*")
	if p := problem(t, "Put * on an existing policy", err); p.Status != http.StatusPreconditionFailed {
		t.Errorf("Put * on an existing policy: %d %q, want 412", p.Status, p.Code)
	}
	puts := s.front.requests(http.MethodPut)
	if last := puts[len(puts)-1].header; last.Get("If-None-Match") != "*" || last.Get("If-Match") != "" {
		t.Errorf("Put *: If-None-Match %q If-Match %q, want * and none", last.Get("If-None-Match"), last.Get("If-Match"))
	}
}

// An action answering the resource it changed returns it, with no
// operation.
func TestActionAnswersResource(t *testing.T) {
	s := serve(t, admin())
	c := client(t, s.url, newClock())
	sh, err := c.CreateShare(t.Context(), newShare("builds", s.namespace(t)))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Shares().Action(t.Context(), sh.ID, "quiesce", nil, "")
	if err != nil || res.Operation != nil || res.Resource == nil || res.Resource.Status.State != "quiesced" || res.Resource.ETag == "" {
		t.Errorf(":quiesce: %+v err %v, want the share quiesced with its ETag and no operation", res, err)
	}
}

// A delete answered 202 returns its operation, which Wait follows to its
// end, and which the operation reads and listing answer; a cancel is
// refused not_cancellable.
func TestDeleteAndWait(t *testing.T) {
	s := serve(t, admin())
	c := client(t, s.url, newClock())
	sh, err := c.CreateShare(t.Context(), newShare("builds", s.namespace(t)))
	if err != nil {
		t.Fatal(err)
	}
	op, err := c.Shares().Delete(t.Context(), sh.ID, sh.ETag)
	if err != nil || op == nil || op.Kind != apiv1.OpShareDelete || op.Target.ID != sh.ID {
		t.Fatalf("Delete: %+v err %v, want a share.delete on %s", op, err, sh.ID)
	}
	if got, err := c.Operation(t.Context(), op.ID); err != nil || got.ID != op.ID || got.State != op.State {
		t.Errorf("Operation(%s): %+v err %v, want it as Delete answered", op.ID, got, err)
	}
	var listed []string
	for o, err := range c.Operations(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		listed = append(listed, o.ID)
	}
	if !slices.Equal(listed, []string{op.ID}) {
		t.Errorf("Operations: %q, want [%s]", listed, op.ID)
	}
	err = c.CancelOperation(t.Context(), op.ID)
	if p := problem(t, "CancelOperation", err); p.Code != apiv1.CodeNotCancellable {
		t.Errorf("CancelOperation: %q, want not_cancellable", p.Code)
	}

	base := s.clock.Pending()
	type waited struct {
		op  apiv1.Operation
		err error
	}
	done := make(chan waited, 1)
	go func() {
		o, err := c.Wait(t.Context(), *op)
		done <- waited{o, err}
	}()
	eventually(t, "the wait's poll parks", func() bool { return s.clock.Pending() > base })
	id := entity.OperationID(uuid.MustParse(op.ID))
	rec, _, err := s.store.Operation(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	rec.State = entity.OpSucceeded
	if _, err := s.store.PutOperation(t.Context(), rec, rec.Generation); err != nil {
		t.Fatalf("end the operation: %v", err)
	}
	s.clock.Advance(time.Second)
	if w := <-done; w.err != nil || w.op.State != apiv1.OpSucceeded {
		t.Errorf("Wait: %s err %v, want succeeded", w.op.State, w.err)
	}
}

// A field of the answer the client does not know is ignored.
func TestUnknownFieldIgnored(t *testing.T) {
	s := serve(t, admin())
	c := client(t, s.url, newClock())
	sh, err := c.CreateShare(t.Context(), newShare("builds", s.namespace(t)))
	if err != nil {
		t.Fatal(err)
	}
	s.front.add(fault{match: on(http.MethodGet, "/api/v1/shares/"+sh.ID), edit: func(b []byte) []byte {
		b = bytes.Replace(b, []byte(`{"id"`), []byte(`{"later":{"x":1},"id"`), 1)
		return bytes.Replace(b, []byte(`"spec":{`), []byte(`"spec":{"later":true,`), 1)
	}})
	got, err := c.Shares().Get(t.Context(), sh.ID)
	if err != nil || got.Spec.Name != "builds" {
		t.Errorf("Get answered with fields the client does not know: %+v err %v, want builds", got, err)
	}
	if fs := s.front.requests(http.MethodGet); !strings.Contains(fs[len(fs)-1].uri, sh.ID) {
		t.Fatalf("the last GET was %s, want the share's", fs[len(fs)-1].uri)
	}
}

// A server serving no v1 is refused at first contact, naming both
// versions, and is sent no call.
func TestServerWithoutV1Refused(t *testing.T) {
	s := serve(t, admin())
	s.front.add(fault{match: on(http.MethodGet, "/api/version"), answer: &scripted{
		status: http.StatusOK, header: http.Header{"Content-Type": {"application/json"}}, body: `{"majors":["v2"]}`,
	}})
	_, err := client(t, s.url, newClock()).Shares().Get(t.Context(), uuid.NewString())
	if err == nil || !strings.Contains(err.Error(), "v1") || !strings.Contains(err.Error(), "v2") {
		t.Errorf("a call to a server serving v2 only: error %v, want one naming v1 and v2", err)
	}
	if gets := s.front.requests(http.MethodGet); len(gets) != 1 || gets[0].uri != "/api/version" {
		t.Errorf("requests sent: %+v, want only GET /api/version", gets)
	}
}

// tokens is a token source of the test's own: Token answers token, and
// Refresh what refresh does with the token that failed.
type tokens struct {
	token   string
	refresh func(failed string) (string, error)
}

func (ts tokens) Token(context.Context) (string, error) { return ts.token, nil }
func (ts tokens) Refresh(_ context.Context, failed string) (string, error) {
	return ts.refresh(failed)
}

// A call refused 401 is refreshed once and sent again with its body and its
// key; when the refresh fails, the 401's problem is returned and nothing is
// sent again.
func TestUnauthorizedRefreshesOnce(t *testing.T) {
	t.Run("refreshed", func(t *testing.T) {
		s := serve(t, admin())
		s.front.add(fault{match: func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer stale" }, answer: &scripted{
			status: http.StatusUnauthorized, header: http.Header{"Content-Type": {apiv1.MediaTypeProblem}},
			body: `{"status":401,"code":"unauthenticated","title":"Unauthorized","retryable":false,"request_id":"r1"}`,
		}})
		ts := tokens{"stale", func(failed string) (string, error) {
			if failed != "stale" {
				return "", errors.New("refreshed " + failed + ", want stale")
			}
			return "fresh", nil
		}}
		c := client(t, s.url, newClock(), apiclient.WithTokenSource(ts))
		sh, err := c.CreateShare(t.Context(), newShare("builds", s.namespace(t)))
		if err != nil {
			t.Fatalf("CreateShare refused 401 once: %v", err)
		}
		if ids := s.shareIDs(t); !slices.Equal(ids, []string{sh.ID}) {
			t.Errorf("shares: %q, want [%s]", ids, sh.ID)
		}
		posts := s.front.requests(http.MethodPost)
		if len(posts) != 2 || posts[0].header.Get("Authorization") != "Bearer stale" || posts[1].header.Get("Authorization") != "Bearer fresh" ||
			keys(posts)[0] == "" || keys(posts)[0] != keys(posts)[1] || posts[0].body != posts[1].body {
			t.Errorf("POSTs sent: %+v, want the stale one and then the fresh one, under one key and body", posts)
		}
	})
	t.Run("refresh fails", func(t *testing.T) {
		s := serve(t, nil)
		ts := tokens{"stale", func(string) (string, error) { return "", errors.New("the session is gone") }}
		_, err := client(t, s.url, newClock(), apiclient.WithTokenSource(ts)).Shares().Get(t.Context(), uuid.NewString())
		if p := problem(t, "Get refused 401", err); p.Status != http.StatusUnauthorized || p.Code != apiv1.CodeUnauthenticated {
			t.Errorf("Get refused 401 with the refresh failing: %d %q, want 401 unauthenticated", p.Status, p.Code)
		}
		if gets := s.front.requests(http.MethodGet); len(gets) != 2 {
			t.Errorf("GETs sent: %d, want the version and the call once", len(gets))
		}
	})
}

// A create or an action without an idempotency key sends none, and one
// whose answer is lost is not sent again.
func TestWithoutIdempotencyKey(t *testing.T) {
	s := serve(t, admin())
	c := client(t, s.url, newClock())
	sh, err := c.CreateShare(t.Context(), newShare("builds", s.namespace(t)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Shares().Create(t.Context(), apiv1.ShareSpec{Name: "ci"}, apiclient.WithoutIdempotencyKey())
	if p := problem(t, "Create without a key", err); p.Code != apiv1.CodePreconditionRequired {
		t.Errorf("Create without a key: %q, want precondition_required", p.Code)
	}
	_, err = c.Shares().Action(t.Context(), sh.ID, "quiesce", nil, "", apiclient.WithoutIdempotencyKey())
	if p := problem(t, "Action without a key", err); p.Code != apiv1.CodePreconditionRequired {
		t.Errorf("Action without a key: %q, want precondition_required", p.Code)
	}
	if ks := keys(s.front.requests(http.MethodPost)); !slices.Equal(ks[1:], []string{"", ""}) || ks[0] == "" {
		t.Errorf("POST keys: %q, want the create's and then none twice", ks)
	}

	s.front.add(fault{match: on(http.MethodPost, "/api/v1/shares/"+sh.ID+":resume"), drop: true})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second) // a backstop: a retry would wait on a clock that never moves
	defer cancel()
	_, err = c.Shares().Action(ctx, sh.ID, "resume", nil, "", apiclient.WithoutIdempotencyKey())
	if err == nil || errors.As(err, new(*apiv1.Problem)) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Action without a key, its answer lost: error %v, want the lost answer, returned at once", err)
	}
	if n := len(slices.DeleteFunc(s.front.requests(http.MethodPost), func(r seen) bool { return !strings.HasSuffix(r.uri, ":resume") })); n != 1 {
		t.Errorf(":resume sent %d times, want once", n)
	}
}

// A listing follows every page, and the name filter finds one share or
// none.
func TestListPages(t *testing.T) {
	s := serve(t, admin())
	c := client(t, s.url, newClock())
	ns := s.namespace(t)
	var want []string
	for _, name := range []string{"a", "b", "c"} {
		sh, err := c.CreateShare(t.Context(), newShare(name, ns))
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, sh.ID)
	}
	for _, tc := range []struct {
		filter url.Values
		want   []string
	}{
		{url.Values{"limit": {"1"}}, want},
		{url.Values{"name": {"b"}}, want[1:2]},
		{url.Values{"name": {"none"}}, nil},
	} {
		var got []string
		for sh, err := range c.Shares().List(t.Context(), tc.filter) {
			if err != nil {
				t.Fatalf("List %v: %v", tc.filter, err)
			}
			got = append(got, sh.ID)
		}
		if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(tc.want))) {
			t.Errorf("List %v: %q, want %q", tc.filter, got, tc.want)
		}
	}
}

// Nodes, the audit and the health read through the client.
func TestReads(t *testing.T) {
	s := serve(t, admin())
	c := client(t, s.url, newClock())
	node, err := s.nodes.RegisterNode(t.Context(), entity.Node{ID: entity.NodeID(uuid.New()), Address: "10.0.0.7:12049", Roles: entity.NodeRoleProtocol | entity.NodeRoleStorage, Binary: "v9"})
	if err != nil {
		t.Fatal(err)
	}
	nodeID := uuid.UUID(node.ID).String()
	if got, err := c.Nodes().Get(t.Context(), nodeID); err != nil || got.Status.Binary != "v9" {
		t.Errorf("Nodes().Get(%s): %+v err %v, want binary v9", nodeID, got, err)
	}
	sh, err := c.CreateShare(t.Context(), newShare("builds", s.namespace(t)))
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	for a, err := range c.Audit(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, a.Target.ID)
	}
	if !slices.Contains(targets, sh.ID) {
		t.Errorf("Audit targets: %q, want the create of %s", targets, sh.ID)
	}
	if h, err := c.Health(t.Context()); err != nil || h.Ready != "ready" {
		t.Errorf("Health: %+v err %v, want ready", h, err)
	}
}
