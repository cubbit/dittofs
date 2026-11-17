package apiclient_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/cubbit/dittofs/pkg/sim"
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

// settled runs call and returns its error once it returns, failing the test
// if the client instead waits on clock to send again, or does neither within
// a generous bound.
func settled(t *testing.T, what string, clock *sim.TestClock, call func(context.Context) error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call(t.Context()) }()
	for end := time.Now().Add(10 * time.Second); ; {
		select {
		case err := <-done:
			return err
		default:
		}
		if clock.Pending() > 0 {
			t.Fatalf("%s: the client waits to send again, want it returned", what)
		}
		if time.Now().After(end) {
			t.Fatalf("%s: neither returned nor waited", what)
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

// bodies is the body each request carried.
func bodies(rs []seen) []string {
	var bs []string
	for _, r := range rs {
		bs = append(bs, r.body)
	}
	return bs
}

// to is the requests among rs whose URI starts with prefix.
func to(rs []seen, prefix string) []seen {
	return slices.DeleteFunc(rs, func(r seen) bool { return !strings.HasPrefix(r.uri, prefix) })
}

// checkKeys checks that every request f was sent carried an
// Idempotency-Key exactly when it was a POST or a DELETE.
func checkKeys(t *testing.T, f *front) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.seen {
		want := r.method == http.MethodPost || r.method == http.MethodDelete
		if got := r.header.Get("Idempotency-Key") != ""; got != want {
			t.Errorf("%s %s: carries an Idempotency-Key %v, want %v", r.method, r.uri, got, want)
		}
	}
}

var unavailable = fault{
	status: http.StatusServiceUnavailable,
	header: http.Header{"Content-Type": {apiv1.MediaTypeProblem}, "Retry-After": {"7"}},
	body:   `{"status":503,"code":"unavailable","title":"Service Unavailable","retryable":true,"request_id":"r1"}`,
}

// New takes an https URL or an http URL on a loopback address, each with no
// credentials, path, query or fragment, or a socket's path; it refuses plain
// http to any other host, localhost included, and a bare host taken for a
// path.
func TestNew(t *testing.T) {
	for _, tc := range []struct {
		server string
		ok     bool
	}{
		{"https://dfs.example:8080", true},
		{"https://dfs.example:8080/", true},
		{"http://127.0.0.1:8080", true},
		{"http://127.0.0.2:8080/", true},
		{"http://[::1]:8080", true},
		{"/run/dittofs/api.sock", true},
		{"run/api.sock", true},
		{"http://localhost:8080/", false},
		{"http://dfs.example:8080", false},
		{"http://10.0.0.1:8080", false},
		{"ftp://127.0.0.1", false},
		{"http://127.0.0.1:80@example.com/", false},
		{"http://eli:secret@127.0.0.1:8080", false},
		{"https://eli@dfs.example", false},
		{"http://127.0.0.1:8080/api", false},
		{"http://127.0.0.1:8080/?x=1", false},
		{"http://127.0.0.1:8080?", false},
		{"http://127.0.0.1:8080/#top", false},
		{"node.example.com:8443", false},
		{"https://", false},
		{"https://:443", false},
		{"api.sock", false},
	} {
		if _, err := apiclient.New(tc.server); (err == nil) != tc.ok {
			t.Errorf("New(%q): error %v, want accepted %v", tc.server, err, tc.ok)
		}
	}
}

// Over http on loopback, over the socket and over https with the server's
// CA, a share is created, found by its name through the list filter, read
// with its ETag, and renamed under that ETag; https without the CA is
// refused at once, not sent again.
func TestResourceOverEachTransport(t *testing.T) {
	s := serve(t, admin())
	tlsFront := httptest.NewTLSServer(s.front)
	t.Cleanup(tlsFront.Close)
	pool := x509.NewCertPool()
	pool.AddCert(tlsFront.Certificate())
	clock := newClock()
	err := settled(t, "https without the server's CA", clock, func(ctx context.Context) error {
		_, err := client(t, tlsFront.URL, clock).Shares().Get(ctx, uuid.NewString())
		return err
	})
	if err == nil || errors.As(err, new(*apiv1.Problem)) {
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

// A create or a read whose answer was lost, or which was refused with a
// retryable problem, is sent again once its wait has passed on the client's
// clock, and not before, with the same key and body; the server's replay of
// a create's record answers it, so there is one share.
func TestRetries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		ft     fault
		wait   time.Duration
	}{
		{"a lost create answer", http.MethodPost, fault{drop: true}, time.Second},
		{"a create refused 503 with Retry-After 7", http.MethodPost, unavailable, 7 * time.Second},
		{"a lost read answer", http.MethodGet, fault{drop: true}, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := serve(t, admin())
			clock := newClock()
			c := client(t, s.url, clock)
			ns := s.namespace(t)
			ft := tc.ft
			ft.match = func(r *http.Request) bool {
				return r.Method == tc.method && strings.HasPrefix(r.URL.Path, "/api/v1/shares")
			}
			s.front.add(ft)
			type result struct {
				id  string
				err error
			}
			done := make(chan result, 1)
			go func() {
				made, err := c.CreateShare(t.Context(), newShare("builds", ns))
				if err == nil {
					_, err = c.Shares().Get(t.Context(), made.ID)
				}
				done <- result{made.ID, err}
			}()
			eventually(t, "the client waits after the fault", func() bool { return clock.Pending() == 1 })
			clock.Advance(tc.wait - time.Millisecond)
			if n := clock.Pending(); n != 1 {
				t.Fatalf("a millisecond before the wait of %v: %d waits pending, want the client still waiting", tc.wait, n)
			}
			clock.Advance(time.Millisecond)
			got := <-done
			if got.err != nil {
				t.Fatalf("CreateShare then Get, after the fault: %v", got.err)
			}
			if ids := s.shareIDs(t); !slices.Equal(ids, []string{got.id}) {
				t.Errorf("shares: %q, want only the answered %s", ids, got.id)
			}
			sent := to(s.front.requests(tc.method), "/api/v1/shares")
			ks := keys(sent)
			if len(sent) != 2 || ks[0] != ks[1] || (ks[0] != "") != (tc.method == http.MethodPost) || sent[0].body != sent[1].body {
				t.Errorf("%ss sent: keys %q bodies %q, want two sendings of one call under one key and body, a key only on a POST", tc.method, ks, bodies(sent))
			}
		})
	}
}

// A call is sent at most 5 times: after the fifth lost answer the error is
// returned.
func TestAtMostFiveSends(t *testing.T) {
	s := serve(t, admin())
	clock := newClock()
	c := client(t, s.url, clock)
	id := uuid.NewString()
	for range 5 {
		s.front.add(fault{match: on(http.MethodGet, "/api/v1/shares/"+id), drop: true})
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.Shares().Get(t.Context(), id)
		done <- err
	}()
	for n := 1; n < 5; n++ {
		eventually(t, fmt.Sprintf("the client waits after lost answer %d", n), func() bool { return clock.Pending() == 1 })
		clock.Advance(time.Second)
	}
	err := settled(t, "Get after the fifth lost answer", clock, func(context.Context) error { return <-done })
	if err == nil || errors.As(err, new(*apiv1.Problem)) {
		t.Errorf("Get with five answers lost: error %v, want the lost answer", err)
	}
	if n := len(to(s.front.requests(http.MethodGet), "/api/v1/shares/")); n != 5 {
		t.Errorf("GETs of the share sent: %d, want 5", n)
	}
}

// A refusal whose body is no problem, such as a proxy's page, is a problem
// holding its status, never retryable, and is not sent again.
func TestNonProblemBodyIsFinal(t *testing.T) {
	s := serve(t, admin())
	clock := newClock()
	c := client(t, s.url, clock)
	id := uuid.NewString()
	s.front.add(fault{match: on(http.MethodGet, "/api/v1/shares/"+id), status: http.StatusServiceUnavailable,
		header: http.Header{"Content-Type": {"text/html"}}, body: "<html>busy</html>"})
	err := settled(t, "Get answered a 503 page", clock, func(ctx context.Context) error {
		_, err := c.Shares().Get(ctx, id)
		return err
	})
	if p := problem(t, "Get answered a 503 page", err); p.Status != http.StatusServiceUnavailable || p.Retryable {
		t.Errorf("Get answered a 503 page: status %d retryable %v, want 503 and not retryable", p.Status, p.Retryable)
	}
}

// An answer longer than 16 MiB is an error, and the call is not sent again.
func TestAnswerTooLong(t *testing.T) {
	s := serve(t, admin())
	clock := newClock()
	c := client(t, s.url, clock)
	id := uuid.NewString()
	s.front.add(fault{match: on(http.MethodGet, "/api/v1/shares/"+id), status: http.StatusInternalServerError,
		header: http.Header{"Content-Type": {"text/plain"}}, body: strings.Repeat("x", 16<<20+1)})
	err := settled(t, "Get answered 16 MiB and a byte", clock, func(ctx context.Context) error {
		_, err := c.Shares().Get(ctx, id)
		return err
	})
	if err == nil || errors.As(err, new(*apiv1.Problem)) || !strings.Contains(err.Error(), "16 MiB") {
		t.Errorf("Get answered 16 MiB and a byte: error %.200v, want the answer refused as too long", err)
	}
}

// A redirect is not followed: the call is refused with the redirect's
// status, and its token never reaches the redirect's target. The version
// check is passed through, so the call itself meets the redirect.
func TestRedirectNotFollowed(t *testing.T) {
	s := serve(t, admin())
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			s.front.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, s.url+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)
	ts := tokens{"secret", func(string) (string, error) { return "", errors.New("refreshed, want no refresh") }}
	_, err := client(t, redirect.URL, newClock(), apiclient.WithTokenSource(ts)).Health(t.Context())
	for _, r := range s.front.requests(http.MethodGet) {
		if a := r.header.Get("Authorization"); a != "" {
			t.Errorf("the redirect's target was sent GET %s with Authorization %q, want none", r.uri, a)
		}
	}
	if p := problem(t, "a call answered 307", err); p.Status != http.StatusTemporaryRedirect || p.Retryable {
		t.Errorf("a call answered 307: status %d retryable %v, want 307 and not retryable", p.Status, p.Retryable)
	}
}

// A call waiting on another call's version check stops when its context
// ends.
func TestVersionWaitEndsWithContext(t *testing.T) {
	s := serve(t, admin())
	hold := make(chan struct{})
	s.front.add(fault{match: on(http.MethodGet, "/api/version"), hold: hold})
	c := client(t, s.url, newClock())
	first := make(chan error, 1)
	go func() {
		_, err := c.Health(t.Context())
		first <- err
	}()
	eventually(t, "the first call's version check reaches the server", func() bool { return len(s.front.requests(http.MethodGet)) == 1 })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	second := make(chan error, 1)
	go func() {
		_, err := c.Health(ctx)
		second <- err
	}()
	// A bounded wait is the assertion: a second call that ignored its
	// context would wait for the first's check, which the front holds.
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the second call, its context ended: error %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Errorf("the second call, its context ended, still waits on the first's version check")
	}
	close(hold)
	if err := <-first; err != nil {
		t.Errorf("the first call, its version check released: %v", err)
	}
}

// A server serving no v1 is refused at first contact, with an error naming
// both versions, and no call under /api/v1/ is sent to it.
func TestVersionRefused(t *testing.T) {
	s := serve(t, admin())
	s.front.add(fault{match: on(http.MethodGet, "/api/version"), status: http.StatusOK,
		header: http.Header{"Content-Type": {"application/json"}}, body: `{"majors":["v2"]}`})
	_, err := client(t, s.url, newClock()).Health(t.Context())
	if err == nil || !strings.Contains(err.Error(), "v1") || !strings.Contains(err.Error(), "v2") {
		t.Errorf("Health against a server serving only v2: error %v, want one naming v1 and v2", err)
	}
	if rs := to(s.front.requests(http.MethodGet), "/api/v1/"); len(rs) != 0 {
		t.Errorf("GETs under /api/v1/ sent: %+v, want none", rs)
	}
}

// A version check whose answer was lost is sent again once its wait has
// passed on the client's clock, and the call then goes through.
func TestVersionResentAfterLostAnswer(t *testing.T) {
	s := serve(t, admin())
	s.front.add(fault{match: on(http.MethodGet, "/api/version"), drop: true})
	clock := newClock()
	c := client(t, s.url, clock)
	done := make(chan error, 1)
	go func() {
		_, err := c.Health(t.Context())
		done <- err
	}()
	eventually(t, "the client waits after the lost version answer", func() bool {
		select {
		case err := <-done:
			t.Fatalf("Health, its version answer lost: returned %v before waiting, want it to wait and send the check again", err)
		default:
		}
		return clock.Pending() == 1
	})
	clock.Advance(time.Second)
	if err := <-done; err != nil {
		t.Errorf("Health after the lost version answer was sent again: %v", err)
	}
	if n := len(to(s.front.requests(http.MethodGet), "/api/version")); n != 2 {
		t.Errorf("version checks sent: %d, want 2, the lost one and its resending", n)
	}
}

// A write under a stale tag is refused 412 and not sent again.
func TestStaleTagStops(t *testing.T) {
	s := serve(t, admin())
	clock := newClock()
	c := client(t, s.url, clock)
	sh, err := c.CreateShare(t.Context(), newShare("builds", s.namespace(t)))
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	if _, err := c.Shares().Patch(t.Context(), sh.ID, map[string]any{"spec": map[string]any{"name": "ci"}}, sh.ETag); err != nil {
		t.Fatalf("Patch under the current tag: %v", err)
	}
	err = settled(t, "Patch under the stale tag", clock, func(ctx context.Context) error {
		_, err := c.Shares().Patch(ctx, sh.ID, map[string]any{"spec": map[string]any{"name": "qa"}}, sh.ETag)
		return err
	})
	if p := problem(t, "Patch under the stale tag", err); p.Status != http.StatusPreconditionFailed || p.Code != apiv1.CodeGenerationMismatch {
		t.Errorf("Patch under the stale tag: %d %q, want 412 generation_mismatch", p.Status, p.Code)
	}
	if n := len(s.front.requests(http.MethodPatch)); n != 2 {
		t.Errorf("PATCHes sent: %d, want 2, the refused one once", n)
	}
}

// A PATCH or a PUT whose answer was lost is returned at once, not sent
// again: it carries no key, and its second sending under the same tag would
// be refused 412 for a write that applied.
func TestLostWriteAnswerNotResent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   string
		write  func(context.Context, *apiclient.Client, apiclient.Tagged[apiv1.ShareSpec, apiv1.ShareStatus]) error
	}{
		{"Patch of a share", http.MethodPatch, "", func(ctx context.Context, c *apiclient.Client, sh apiclient.Tagged[apiv1.ShareSpec, apiv1.ShareStatus]) error {
			_, err := c.Shares().Patch(ctx, sh.ID, map[string]any{"spec": map[string]any{"name": "ci"}}, sh.ETag)
			return err
		}},
		{"Put of an export policy", http.MethodPut, "/export-policy", func(ctx context.Context, c *apiclient.Client, sh apiclient.Tagged[apiv1.ShareSpec, apiv1.ShareStatus]) error {
			pol, err := c.ExportPolicies().Get(ctx, sh.ID)
			if err != nil {
				return fmt.Errorf("get the policy: %w", err)
			}
			_, err = c.ExportPolicies().Put(ctx, sh.ID, policy, pol.ETag)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := serve(t, admin())
			clock := newClock()
			c := client(t, s.url, clock)
			sh, err := c.CreateShare(t.Context(), newShare("builds", s.namespace(t)))
			if err != nil {
				t.Fatalf("CreateShare: %v", err)
			}
			target := "/api/v1/shares/" + sh.ID + tc.path
			s.front.add(fault{match: on(tc.method, target), drop: true})
			err = settled(t, tc.name+", its answer lost", clock, func(ctx context.Context) error { return tc.write(ctx, c, sh) })
			if err == nil || errors.As(err, new(*apiv1.Problem)) {
				t.Errorf("%s, its answer lost: error %v, want the lost answer", tc.name, err)
			}
			if n := len(to(s.front.requests(tc.method), target)); n != 1 {
				t.Errorf("%ss of %s sent: %d, want 1", tc.method, target, n)
			}
		})
	}
}

// A share's export policy is read, replaced and merged at its own path, each
// under the tag the last answered; a PUT with "*" sends If-None-Match: *,
// refused 412 on a policy that exists. Only a POST or a DELETE carries a
// key.
func TestExportPolicy(t *testing.T) {
	s := serve(t, admin())
	clock := newClock()
	c := client(t, s.url, clock)
	sh, err := c.CreateShare(t.Context(), newShare("builds", s.namespace(t)))
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	pol, err := c.ExportPolicies().Get(t.Context(), sh.ID)
	if err != nil || pol.ETag == "" || len(pol.Spec.Clients) != 1 || pol.Spec.Clients[0].CIDR != "10.0.0.0/8" {
		t.Fatalf("Get the export policy: %+v err %v, want the share's policy for 10.0.0.0/8 with an ETag", pol, err)
	}
	spec := policy
	spec.ReadOnly = true
	put, err := c.ExportPolicies().Put(t.Context(), sh.ID, spec, pol.ETag)
	if err != nil || !put.Spec.ReadOnly || put.ETag == pol.ETag {
		t.Fatalf("Put a read-only policy under its tag: %+v err %v, want it read-only under a new tag", put, err)
	}
	patched, err := c.ExportPolicies().Patch(t.Context(), sh.ID, map[string]any{"spec": map[string]any{"read_only": false}}, put.ETag)
	if err != nil || patched.Spec.ReadOnly || patched.ETag == put.ETag {
		t.Errorf("Patch read_only off under the Put's tag: %+v err %v, want it writable under a new tag", patched, err)
	}
	err = settled(t, "Put * on an existing policy", clock, func(ctx context.Context) error {
		_, err := c.ExportPolicies().Put(ctx, sh.ID, policy, "*")
		return err
	})
	if p := problem(t, "Put * on an existing policy", err); p.Status != http.StatusPreconditionFailed {
		t.Errorf("Put * on an existing policy: %d %q, want 412", p.Status, p.Code)
	}
	puts := s.front.requests(http.MethodPut)
	if len(puts) != 2 {
		t.Fatalf("PUTs sent: %d, want 2", len(puts))
	}
	if h := puts[1].header; h.Get("If-None-Match") != "*" || h.Get("If-Match") != "" {
		t.Errorf("Put *: If-None-Match %q If-Match %q, want * and none", h.Get("If-None-Match"), h.Get("If-Match"))
	}
	checkKeys(t, s.front)
}

// An action answering the resource it changed returns it, with no
// operation. A create or an action without an idempotency key sends none,
// and is not sent again when its answer is lost or it is refused with a
// retryable problem.
func TestWithoutIdempotencyKey(t *testing.T) {
	s := serve(t, admin())
	clock := newClock()
	c := client(t, s.url, clock)
	sh, err := c.CreateShare(t.Context(), newShare("builds", s.namespace(t)))
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	res, err := c.Shares().Action(t.Context(), sh.ID, "quiesce", nil, "")
	if err != nil || res.Operation != nil || res.Resource == nil || res.Resource.Status.State != "quiesced" || res.Resource.ETag == "" {
		t.Errorf(":quiesce: %+v err %v, want the share quiesced with its ETag and no operation", res, err)
	}
	_, err = c.Shares().Create(t.Context(), apiv1.ShareSpec{Name: "ci"}, apiclient.WithoutIdempotencyKey())
	if p := problem(t, "Create without a key", err); p.Code != apiv1.CodePreconditionRequired {
		t.Errorf("Create without a key: %q, want precondition_required", p.Code)
	}
	_, err = c.Shares().Action(t.Context(), sh.ID, "resume", nil, "", apiclient.WithoutIdempotencyKey())
	if p := problem(t, "Action without a key", err); p.Code != apiv1.CodePreconditionRequired {
		t.Errorf("Action without a key: %q, want precondition_required", p.Code)
	}
	if ks := keys(s.front.requests(http.MethodPost)); len(ks) != 4 || ks[0] == "" || ks[1] == "" || ks[2] != "" || ks[3] != "" {
		t.Errorf("POST keys: %q, want the create's and the quiesce's, then none twice", ks)
	}

	s.front.add(fault{match: on(http.MethodPost, "/api/v1/shares/"+sh.ID+":resume"), drop: true})
	err = settled(t, "Action without a key, its answer lost", clock, func(ctx context.Context) error {
		_, err := c.Shares().Action(ctx, sh.ID, "resume", nil, "", apiclient.WithoutIdempotencyKey())
		return err
	})
	if err == nil || errors.As(err, new(*apiv1.Problem)) {
		t.Errorf("Action without a key, its answer lost: error %v, want the lost answer", err)
	}
	if n := len(to(s.front.requests(http.MethodPost), "/api/v1/shares/"+sh.ID+":resume")); n != 2 {
		t.Errorf(":resume sent %d times, want twice, the lost one once", n)
	}

	ft := unavailable
	ft.match = on(http.MethodPost, "/api/v1/shares")
	s.front.add(ft)
	err = settled(t, "Create without a key, refused 503", clock, func(ctx context.Context) error {
		_, err := c.Shares().Create(ctx, apiv1.ShareSpec{Name: "qa"}, apiclient.WithoutIdempotencyKey())
		return err
	})
	if p := problem(t, "Create without a key, refused 503", err); p.Status != http.StatusServiceUnavailable {
		t.Errorf("Create without a key, refused 503: %d, want 503", p.Status)
	}
}

// An action answered 202 returns the operation it started, and no resource.
func TestActionAccepted(t *testing.T) {
	s := serve(t, admin())
	id := uuid.NewString()
	s.front.add(fault{match: on(http.MethodPost, "/api/v1/shares/"+id+":quiesce"), status: http.StatusAccepted,
		header: http.Header{"Content-Type": {"application/json"}}, body: `{"id":"op1","state":"pending"}`})
	res, err := client(t, s.url, newClock()).Shares().Action(t.Context(), id, "quiesce", nil, "")
	if err != nil || res.Operation == nil || res.Operation.ID != "op1" || res.Resource != nil {
		t.Errorf(":quiesce answered 202: operation %+v resource %+v err %v, want operation op1 and no resource", res.Operation, res.Resource, err)
	}
}

// A delete answered 202 returns its operation, which Wait follows to its
// end, and which the operation reads and listing answer; a cancel is
// refused not_cancellable. A query option reaches the route. Only a POST or
// a DELETE carries a key.
func TestDeleteAndWait(t *testing.T) {
	s := serve(t, admin())
	c := client(t, s.url, newClock())
	sh, err := c.CreateShare(t.Context(), newShare("builds", s.namespace(t)))
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	_, err = c.Shares().Delete(t.Context(), sh.ID, sh.ETag, apiclient.WithQuery(url.Values{"snapshots": {"keep"}}))
	if p := problem(t, "Delete ?snapshots=keep", err); p.Status != http.StatusBadRequest || p.Field != "snapshots" {
		t.Errorf("Delete ?snapshots=keep: %d field %q, want 400 on snapshots", p.Status, p.Field)
	}
	op, err := c.Shares().Delete(t.Context(), sh.ID, sh.ETag, apiclient.WithQuery(url.Values{"snapshots": {"delete"}}))
	if err != nil || op == nil || op.Kind != apiv1.OpShareDelete || op.Target.ID != sh.ID {
		t.Fatalf("Delete ?snapshots=delete: %+v err %v, want a share.delete on %s", op, err, sh.ID)
	}
	if dels := s.front.requests(http.MethodDelete); len(dels) != 2 || dels[1].uri != "/api/v1/shares/"+sh.ID+"?snapshots=delete" {
		t.Errorf("DELETEs sent: %+v, want the second at ?snapshots=delete", dels)
	}
	if got, err := c.Operation(t.Context(), op.ID); err != nil || got.ID != op.ID || got.State != op.State {
		t.Errorf("Operation(%s): %+v err %v, want it as Delete answered", op.ID, got, err)
	}
	var listed []string
	for o, err := range c.Operations(t.Context(), nil) {
		if err != nil {
			t.Fatalf("Operations: %v", err)
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
		t.Fatalf("read the operation %s: %v", op.ID, err)
	}
	rec.State = entity.OpSucceeded
	if _, err := s.store.PutOperation(t.Context(), rec, rec.Generation); err != nil {
		t.Fatalf("end the operation: %v", err)
	}
	s.clock.Advance(time.Second)
	if w := <-done; w.err != nil || w.op.State != apiv1.OpSucceeded {
		t.Errorf("Wait: %s err %v, want succeeded", w.op.State, w.err)
	}
	checkKeys(t, s.front)
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

// fresh refreshes "stale" to "fresh".
func fresh(failed string) (string, error) {
	if failed != "stale" {
		return "", errors.New("refreshed " + failed + ", want stale")
	}
	return "fresh", nil
}

// A call refused 401 is refreshed once and sent again with its body and its
// key; a second 401 is returned, not refreshed again; when the refresh
// fails, the 401's problem is returned joined with the refresh's error and
// nothing is sent again.
func TestUnauthorizedRefreshesOnce(t *testing.T) {
	t.Run("refreshed", func(t *testing.T) {
		s := serve(t, admin())
		s.front.add(fault{match: func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer stale" },
			status: http.StatusUnauthorized, header: http.Header{"Content-Type": {apiv1.MediaTypeProblem}},
			body: `{"status":401,"code":"unauthenticated","title":"Unauthorized","retryable":false,"request_id":"r1"}`,
		})
		c := client(t, s.url, newClock(), apiclient.WithTokenSource(tokens{"stale", fresh}))
		sh, err := c.CreateShare(t.Context(), newShare("builds", s.namespace(t)))
		if err != nil {
			t.Fatalf("CreateShare refused 401 once: %v", err)
		}
		if ids := s.shareIDs(t); !slices.Equal(ids, []string{sh.ID}) {
			t.Errorf("shares: %q, want [%s]", ids, sh.ID)
		}
		posts := s.front.requests(http.MethodPost)
		if ks := keys(posts); len(posts) != 2 || posts[0].header.Get("Authorization") != "Bearer stale" || posts[1].header.Get("Authorization") != "Bearer fresh" ||
			ks[0] == "" || ks[0] != ks[1] || posts[0].body != posts[1].body {
			t.Errorf("POSTs sent: %+v, want the stale one and then the fresh one, under one key and body", posts)
		}
	})
	t.Run("refused again", func(t *testing.T) {
		s := serve(t, nil)
		always := tokens{"stale", func(string) (string, error) { return "fresh", nil }}
		_, err := client(t, s.url, newClock(), apiclient.WithTokenSource(always)).Shares().Get(t.Context(), uuid.NewString())
		if p := problem(t, "Get refused 401 twice", err); p.Status != http.StatusUnauthorized {
			t.Errorf("Get refused 401 twice: %d, want 401", p.Status)
		}
		if gets := s.front.requests(http.MethodGet); len(gets) != 3 {
			t.Errorf("GETs sent: %d, want the version, the call and its refreshed sending", len(gets))
		}
	})
	t.Run("refresh fails", func(t *testing.T) {
		s := serve(t, nil)
		gone := errors.New("the session is gone")
		ts := tokens{"stale", func(string) (string, error) { return "", gone }}
		_, err := client(t, s.url, newClock(), apiclient.WithTokenSource(ts)).Shares().Get(t.Context(), uuid.NewString())
		if p := problem(t, "Get refused 401", err); p.Status != http.StatusUnauthorized || p.Code != apiv1.CodeUnauthenticated {
			t.Errorf("Get refused 401 with the refresh failing: %d %q, want 401 unauthenticated", p.Status, p.Code)
		}
		if !errors.Is(err, gone) {
			t.Errorf("Get refused 401 with the refresh failing: error %v, want it to hold the refresh's error", err)
		}
		if gets := s.front.requests(http.MethodGet); len(gets) != 2 {
			t.Errorf("GETs sent: %d, want the version and the call once", len(gets))
		}
	})
}

// A listing follows every page, and a filter admitting none yields none.
func TestListPages(t *testing.T) {
	s := serve(t, admin())
	c := client(t, s.url, newClock())
	ns := s.namespace(t)
	var want []string
	for _, name := range []string{"a", "b", "c"} {
		sh, err := c.CreateShare(t.Context(), newShare(name, ns))
		if err != nil {
			t.Fatalf("CreateShare %s: %v", name, err)
		}
		want = append(want, sh.ID)
	}
	for _, tc := range []struct {
		filter url.Values
		want   []string
	}{
		{url.Values{"limit": {"1"}}, want},
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

// A listing whose page names a cursor it has already followed ends with an
// error: the one the page was read after, or one followed a page earlier,
// an A, B, A cycle.
func TestListRefusesARepeatedCursor(t *testing.T) {
	for _, tc := range []struct {
		name string
		// at is the page, counted among those read after a cursor, whose
		// next is set to the first cursor followed.
		at int
	}{
		{"the cursor it was read after", 1},
		{"a cursor followed a page earlier", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := serve(t, admin())
			c := client(t, s.url, newClock())
			ns := s.namespace(t)
			for _, name := range []string{"a", "b", "c"} {
				if _, err := c.CreateShare(t.Context(), newShare(name, ns)); err != nil {
					t.Fatalf("CreateShare %s: %v", name, err)
				}
			}
			// followed is written by match, which the front calls under its
			// lock, and read by edit after the last match.
			var followed []string
			s.front.add(fault{
				match: func(r *http.Request) bool {
					after := r.URL.Query().Get("after")
					if r.Method != http.MethodGet || r.URL.Path != "/api/v1/shares" || after == "" {
						return false
					}
					followed = append(followed, after)
					return len(followed) == tc.at
				},
				edit: func(_ *http.Request, b []byte) []byte {
					var page map[string]any
					if err := json.Unmarshal(b, &page); err != nil {
						return b
					}
					page["next"] = followed[0]
					out, _ := json.Marshal(page)
					return out
				},
			})
			var err error
			for _, e := range c.Shares().List(t.Context(), url.Values{"limit": {"1"}}) {
				if e != nil {
					err = e
					break
				}
			}
			if err == nil || !strings.Contains(err.Error(), "repeated its cursor") {
				t.Errorf("List with page %d naming the first cursor again: error %v, want the repeat refused", tc.at, err)
			}
		})
	}
}

// Nodes, the audit and the health read through the client, which checks
// the server's version once.
func TestReads(t *testing.T) {
	s := serve(t, admin())
	c := client(t, s.url, newClock())
	node, err := s.nodes.RegisterNode(t.Context(), entity.Node{ID: entity.NodeID(uuid.New()), Address: "10.0.0.7:12049", Roles: entity.NodeRoleProtocol | entity.NodeRoleStorage, Binary: "v9"})
	if err != nil {
		t.Fatalf("register a node: %v", err)
	}
	nodeID := uuid.UUID(node.ID).String()
	if got, err := c.Nodes().Get(t.Context(), nodeID); err != nil || got.Status.Binary != "v9" {
		t.Errorf("Nodes().Get(%s): %+v err %v, want binary v9", nodeID, got, err)
	}
	sh, err := c.CreateShare(t.Context(), newShare("builds", s.namespace(t)))
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	var targets []string
	for a, err := range c.Audit(t.Context(), nil) {
		if err != nil {
			t.Fatalf("Audit: %v", err)
		}
		targets = append(targets, a.Target.ID)
	}
	if !slices.Contains(targets, sh.ID) {
		t.Errorf("Audit targets: %q, want the create of %s", targets, sh.ID)
	}
	if h, err := c.Health(t.Context()); err != nil || h.Ready != "ready" {
		t.Errorf("Health: %+v err %v, want ready", h, err)
	}
	if n := len(to(s.front.requests(http.MethodGet), "/api/version")); n != 1 {
		t.Errorf("version checks sent: %d, want 1 for all the client's calls", n)
	}
}
