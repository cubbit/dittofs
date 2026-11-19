package node_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/google/uuid"
)

// sent is a request's answer, or the error sending it failed with.
type sent struct {
	status int
	body   []byte
	err    error
}

// send sends method on addr's path with body and the headers hdr names in
// pairs, on a connection of its own, and delivers what it was answered.
func send(ctx context.Context, addr, method, path, body string, hdr ...string) <-chan sent {
	ch := make(chan sent, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, strings.NewReader(body))
		if err != nil {
			ch <- sent{err: err}
			return
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err != nil {
			ch <- sent{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		ch <- sent{resp.StatusCode, b, err}
	}()
	return ch
}

func answeredIn(t *testing.T, what string, ch <-chan sent) sent {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("%s: %v", what, r.err)
		}
		return r
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: no answer within 10 s", what)
		return sent{}
	}
}

// putUser writes a user through the store and returns it with its ETag.
func putUser(t *testing.T, h *nodetest.Harness, name string) (entity.Principal, string) {
	t.Helper()
	p := entity.NewUser()
	gen, err := h.Store().ControlPlane().PutUser(t.Context(), entity.User{Principal: p, Name: name}, 0, entity.Alloc{})
	if err != nil {
		t.Fatalf("put user %s: %v", name, err)
	}
	return p, `"` + strconv.FormatUint(gen, 10) + `"`
}

// A user written through the API answers only once the node has
// reconciled through the principals generation its commit raised, read back
// after the answer: the node's clock never moves, so no pass runs unless
// the write woke one.
func TestUserWriteAnswersOnceApplied(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(admin)))
	started(t, h)
	addr, _ := h.Node().Addrs()
	p, tag := putUser(t, h, "alice")
	r := answeredIn(t, "PATCH the user", send(t.Context(), addr, http.MethodPatch, "/api/v1/users/"+uuid.UUID(p.ID).String(), `{"spec":{"disabled":true}}`, "If-Match", tag))
	pos := h.Node().Positions()
	if r.status != http.StatusOK {
		t.Fatalf("PATCH the user: answered %d %s, want 200", r.status, r.body)
	}
	if c := counters(t, h); pos.Principals < c.Principals {
		t.Errorf("node's principals position %d when the PATCH answered, want at least the generation %d it raised", pos.Principals, c.Principals)
	}
}

// With reconcile held after it read the counters, a user's write waits on
// the API's clock and, once a second has passed on it, answers its own
// commit, the node still short of applying it; a grant, which raises
// neither generation, answers at its commit with the clock untouched.
func TestUserWriteWaitsASecondThenAnswersItsCommit(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(admin)))
	var holding atomic.Bool
	held, release := make(chan struct{}, 1), make(chan struct{})
	h.Node().OnReconcileRead(func(kvstore.Counters) {
		if !holding.Load() {
			return
		}
		select {
		case held <- struct{}{}:
		default:
		}
		<-release
	})
	sh := addShare(t, h)
	started(t, h)
	// Registered after started's, so it runs first: Stop finds no pass held.
	t.Cleanup(func() { holding.Store(false); close(release) })
	addr, _ := h.Node().Addrs()
	p, tag := putUser(t, h, "alice")
	base := h.Clock().Pending()
	holding.Store(true)
	ch := send(t.Context(), addr, http.MethodPatch, "/api/v1/users/"+uuid.UUID(p.ID).String(), `{"spec":{"disabled":true}}`, "If-Match", tag)
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("no reconcile pass read the counters within 10 s of the write")
	}
	wait(t, "the writer's wait on the node's clock", func() bool { return h.Clock().Pending() > base })
	select {
	case r := <-ch:
		t.Fatalf("PATCH the user answered %d %s with the node's clock short of the writer's second", r.status, r.body)
	default:
	}
	h.Clock().Advance(time.Second)
	r := answeredIn(t, "PATCH the user once a second passed", ch)
	var u apiv1.User
	if err := json.Unmarshal(r.body, &u); err != nil || r.status != http.StatusOK || !u.Spec.Disabled {
		t.Errorf("PATCH the user once a second passed: answered %d %s (%v), want the commit's 200 with alice disabled", r.status, r.body, err)
	}
	if rec, ok, err := h.Store().ControlPlane().User(t.Context(), p); err != nil || !ok || !rec.Disabled {
		t.Errorf("store's alice: %+v, %v, %v; want her committed disabled", rec, ok, err)
	}
	if pos, c := h.Node().Positions(), counters(t, h); pos.Principals >= c.Principals {
		t.Errorf("node's principals position %d, want it still below %d with reconcile held", pos.Principals, c.Principals)
	}

	g := answeredIn(t, "PUT a grant with reconcile held", send(t.Context(), addr, http.MethodPut,
		"/api/v1/shares/"+sh.ID.String()+"/grants/"+uuid.UUID(p.ID).String(), `{"spec":{"access":"read"}}`, "If-None-Match", "*"))
	if g.status != http.StatusOK {
		t.Errorf("PUT a grant with reconcile held: answered %d %s, want 200 at its commit", g.status, g.body)
	}
}
