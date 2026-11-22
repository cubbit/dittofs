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

// putUser writes a user through the store, its uid and SID allocated as the
// API's create allocates them, and returns it with its ETag.
func putUser(t *testing.T, h *nodetest.Harness, name string) (entity.Principal, string) {
	t.Helper()
	p := entity.NewUser()
	gen, err := h.Store().ControlPlane().PutUser(t.Context(), entity.User{Principal: p, Name: name}, 0, entity.Alloc{UID: true, RID: true, Range: entity.DefaultIDRange})
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
	awaited := make(chan struct{}, 4)
	h.Node().OnAwaited(func() { awaited <- struct{}{} })
	sh := addShare(t, h)
	started(t, h)
	// Registered after started's, so it runs first: Stop finds no pass held.
	t.Cleanup(func() { holding.Store(false); close(release) })
	addr, _ := h.Node().Addrs()
	p, tag := putUser(t, h, "alice")
	holding.Store(true)
	ch := send(t.Context(), addr, http.MethodPatch, "/api/v1/users/"+uuid.UUID(p.ID).String(), `{"spec":{"disabled":true}}`, "If-Match", tag)
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("no reconcile pass read the counters within 10 s of the write")
	}
	select {
	case <-awaited: // the writer's second is armed on the node's clock
	case <-time.After(10 * time.Second):
		t.Fatal("the write's wait for the node never started within 10 s")
	}
	// No event marks an answer that is never sent, so a bounded wait is the
	// assertion here. It outlasts a wall-clock second, so a wait timed on
	// the wall clock rather than the node's answers inside it.
	select {
	case r := <-ch:
		t.Fatalf("PATCH the user answered %d %s with the node's clock short of the writer's second", r.status, r.body)
	case <-time.After(1500 * time.Millisecond):
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

// With reconcile held after it read the counters, each write raising the
// principals generation, a user's, a group's or a membership's, answers
// only once the node's clock passes the writer's second, and each write
// raising neither, a grant's, a quota's or a netgroup's, answers with the
// clock untouched.
func TestWhichWritesWaitForTheNode(t *testing.T) {
	signing := func(context.Context) ([]byte, error) { return []byte("installation"), nil }
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewSigning(admin, signing)))
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
	awaited := make(chan struct{}, 16)
	h.Node().OnAwaited(func() { awaited <- struct{}{} })
	sh := addShare(t, h)
	started(t, h)
	t.Cleanup(func() { holding.Store(false); close(release) })
	addr, _ := h.Node().Addrs()
	holding.Store(true)

	keys := 0
	write := func(what string, raises bool, method, path, body, tag string) sent {
		t.Helper()
		keys++
		hdr := []string{"Idempotency-Key", "k" + strconv.Itoa(keys)}
		switch tag {
		case "":
		case "*":
			hdr = append(hdr, "If-None-Match", "*")
		default:
			hdr = append(hdr, "If-Match", tag)
		}
		ch := send(t.Context(), addr, method, path, body, hdr...)
		if !raises {
			return answeredIn(t, what+" with the node's clock untouched", ch)
		}
		select {
		case <-awaited:
		case r := <-ch:
			t.Fatalf("%s answered %d %s without waiting for the node", what, r.status, r.body)
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: its wait for the node never started within 10 s", what)
		}
		// No event marks an answer that is never sent, so a bounded wait is
		// the assertion here: an answer not held arrives well inside it.
		select {
		case r := <-ch:
			t.Fatalf("%s answered %d %s with the node's clock short of the writer's second", what, r.status, r.body)
		case <-time.After(200 * time.Millisecond):
		}
		h.Clock().Advance(time.Second)
		return answeredIn(t, what+" once a second passed", ch)
	}
	want := func(what string, r sent, status int) {
		t.Helper()
		if r.status != status {
			t.Fatalf("%s: answered %d %s, want %d", what, r.status, r.body, status)
		}
	}
	idOf := func(r sent) string {
		t.Helper()
		var v struct {
			ID         string `json:"id"`
			Generation uint64 `json:"generation"`
		}
		if err := json.Unmarshal(r.body, &v); err != nil {
			t.Fatalf("decode %s: %v", r.body, err)
		}
		return v.ID
	}
	tagOf := func(r sent) string {
		t.Helper()
		var v struct {
			Generation uint64 `json:"generation"`
		}
		if err := json.Unmarshal(r.body, &v); err != nil {
			t.Fatalf("decode %s: %v", r.body, err)
		}
		return `"` + strconv.FormatUint(v.Generation, 10) + `"`
	}
	r := write("POST /users", true, http.MethodPost, "/api/v1/users", `{"spec":{"name":"alice"}}`, "")
	want("POST /users", r, http.StatusCreated)
	select {
	case <-held: // the pass the create woke is held, so no later write is applied
	case <-time.After(10 * time.Second):
		t.Fatal("no reconcile pass read the counters within 10 s of the create")
	}
	user := "/api/v1/users/" + idOf(r)
	r = write("PATCH /users/{id}", true, http.MethodPatch, user, `{"spec":{"disabled":true}}`, tagOf(r))
	want("PATCH /users/{id}", r, http.StatusOK)
	userTag, uid := tagOf(r), idOf(r)
	r = write("POST /groups", true, http.MethodPost, "/api/v1/groups", `{"spec":{"name":"staff"}}`, "")
	want("POST /groups", r, http.StatusCreated)
	group := "/api/v1/groups/" + idOf(r)
	var created apiv1.Group
	if err := json.Unmarshal(r.body, &created); err != nil || created.Spec.GID == nil {
		t.Fatalf("POST /groups: body %s (%v), want the group with its gid", r.body, err)
	}
	r = write("PUT /groups/{id}", true, http.MethodPut, group, `{"spec":{"name":"crew","gid":`+strconv.FormatUint(uint64(*created.Spec.GID), 10)+`}}`, tagOf(r))
	want("PUT /groups/{id}", r, http.StatusOK)
	groupTag := tagOf(r)
	r = write("PUT /groups/{id}/members/{sub}", true, http.MethodPut, group+"/members/"+uid, `{"spec":{}}`, "*")
	want("PUT /groups/{id}/members/{sub}", r, http.StatusOK)
	r = write("DELETE /groups/{id}/members/{sub}", true, http.MethodDelete, group+"/members/"+uid, "", `"1"`)
	want("DELETE /groups/{id}/members/{sub}", r, http.StatusOK)

	shares := "/api/v1/shares/" + sh.ID.String()
	r = write("PUT /shares/{id}/grants/{sub}", false, http.MethodPut, shares+"/grants/"+uid, `{"spec":{"access":"read"}}`, "*")
	want("PUT /shares/{id}/grants/{sub}", r, http.StatusOK)
	r = write("DELETE /shares/{id}/grants/{sub}", false, http.MethodDelete, shares+"/grants/"+uid, "", tagOf(r))
	want("DELETE /shares/{id}/grants/{sub}", r, http.StatusOK)
	r = write("PUT /shares/{id}/quotas/{sub}", false, http.MethodPut, shares+"/quotas/"+uid,
		`{"spec":{"hard":{"bytes":1,"files":0},"soft":{"bytes":0,"files":0},"grace_seconds":0,"advisory":{"bytes":0,"files":0}}}`, "*")
	want("PUT /shares/{id}/quotas/{sub}", r, http.StatusOK)
	r = write("PUT /netgroups/{sub}", false, http.MethodPut, "/api/v1/netgroups/labs", `{"spec":{"members":["10.0.0.0/8"]}}`, "*")
	want("PUT /netgroups/{sub}", r, http.StatusOK)
	r = write("DELETE /netgroups/{sub}", false, http.MethodDelete, "/api/v1/netgroups/labs", "", tagOf(r))
	want("DELETE /netgroups/{sub}", r, http.StatusOK)

	r = write("DELETE /users/{id}", true, http.MethodDelete, user, "", userTag)
	want("DELETE /users/{id}", r, http.StatusAccepted)
	r = write("DELETE /groups/{id}", true, http.MethodDelete, group, "", groupTag)
	want("DELETE /groups/{id}", r, http.StatusAccepted)
}
