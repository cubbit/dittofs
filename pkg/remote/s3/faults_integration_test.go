//go:build integration

package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/s3/s3fault"
)

// faultRig is one store, in batch and in one-name delete modes, over a fresh
// LocalStack bucket and prefix holding one block and the health object, its
// every request passing through one fault transport and one armed fault.
type faultRig struct {
	tr      *s3fault.Transport
	f       *armed
	batch   *store
	oneName *store
	block   remote.Name
}

// newFaultRig watches the test for leaks, then builds the rig.
func newFaultRig(t *testing.T) *faultRig {
	t.Helper()

	// Before the watch, as newLSRig says.
	endpoint, roots := s3fault.LocalStack(t)
	prof.Watch(t, prof.Options{})
	b := s3fault.FreshBucket(t)

	r := &faultRig{f: &armed{}, block: remote.Name{0xfa}}

	c, err := NewClient(ClientConfig{
		Endpoint:        endpoint,
		Bucket:          b.Name,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		TrustRoots:      roots,
		Limit:           4,
	}, Deps{
		Logger:    slog.New(slog.DiscardHandler),
		Recorder:  &calls{},
		Transport: func(rt http.RoundTripper) http.RoundTripper { r.tr = s3fault.New(rt, r.f); return r.tr },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	prefix := fmt.Sprintf("ns-%x/", randomBytes(4))
	if r.batch, err = open(c, prefix, remote.Mutable, choice{proved: true, sum: remote.ChecksumCRC32C, batch: true}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if r.oneName, err = open(c, prefix, remote.Mutable, crc); err != nil {
		t.Fatalf("open: %v", err)
	}

	body := randomBytes(4096)
	if err := r.batch.Put(t.Context(), r.block, bytes.NewReader(body), int64(len(body)), crc32cOf(body)); err != nil {
		t.Fatalf("Put of the rig's block: %v", err)
	}
	// Longer than any cut a row makes, since a reply that ends before
	// TruncateReply's cut never reaches its end.
	if err := r.batch.PutControl(t.Context(), remote.RoleHealth, randomBytes(4096)); err != nil {
		t.Fatalf("PutControl of the rig's health object: %v", err)
	}

	return r
}

// storeCall is one call of remote.Store this package serves: the SDK
// operation its one request is, and a run that makes the call and returns its
// error, a get's from its reader's end or its Close too.
type storeCall struct {
	name string
	op   string
	body bool // the service's reply to it carries a body
	run  func(ctx context.Context, r *faultRig) error
}

var everyCall = []storeCall{
	{"put", "PutObject", false, func(ctx context.Context, r *faultRig) error {
		b := []byte("a block put under a fault")
		return r.batch.Put(ctx, remote.Name{0xb0}, bytes.NewReader(b), int64(len(b)), crc32cOf(b))
	}},
	{"put_control", "PutObject", false, func(ctx context.Context, r *faultRig) error {
		return r.batch.PutControl(ctx, remote.RoleClaim, []byte("a claim put under a fault"))
	}},
	{"get", "GetObject", true, func(ctx context.Context, r *faultRig) error {
		return readAll(r.batch.Get(ctx, r.block, remote.Range{}))
	}},
	{"get_ranged", "GetObject", true, func(ctx context.Context, r *faultRig) error {
		return readAll(r.batch.Get(ctx, r.block, remote.Range{Off: 1, Len: 1000}))
	}},
	{"get_control", "GetObject", true, func(ctx context.Context, r *faultRig) error {
		_, _, err := r.batch.GetControl(ctx, remote.RoleHealth)
		return err
	}},
	{"delete", "DeleteObjects", true, func(ctx context.Context, r *faultRig) error {
		return r.batch.Delete(ctx, []remote.Name{{0xde}})[0]
	}},
	{"delete_one_name", "DeleteObject", false, func(ctx context.Context, r *faultRig) error {
		return r.oneName.Delete(ctx, []remote.Name{{0xde}})[0]
	}},
	{"list", "ListObjectsV2", true, func(ctx context.Context, r *faultRig) error {
		_, _, _, err := r.batch.List(ctx, "", 10)
		return err
	}},
}

// readAll reads a get's reader to its end and closes it, returning the get's,
// the read's or the close's error.
func readAll(rc io.ReadCloser, _ remote.StoredSum, err error) error {
	if err != nil {
		return err
	}

	_, err = io.Copy(io.Discard, rc)
	if cerr := rc.Close(); err == nil {
		err = cerr
	}

	return err
}

// once makes c's call under rule's effect, applied to c's one request, and
// returns how many requests it made and its error.
func (r *faultRig) once(t *testing.T, c storeCall, e s3fault.Effect) (int, error) {
	t.Helper()

	remove := r.tr.Inject(s3fault.Rule{Op: c.op, Times: 1}, e)
	defer remove()

	before := len(r.tr.Requests())
	err := c.run(t.Context(), r)

	return len(r.tr.Requests()) - before, err
}

// failsOnce fails t unless err is a failure wrapping exactly one sentinel,
// reaching no SDK type, made in one request; what names the case.
func failsOnce(t *testing.T, what string, err error, requests int) {
	t.Helper()

	if requests != 1 {
		t.Errorf("%s: %d requests, want 1: one failing call is one attempt", what, requests)
	}

	if why, _ := oneSentinel(err); why != "" {
		t.Errorf("%s = %v: %s, want exactly one sentinel", what, err, why)
	}

	noSDKType(t, what, err)

	if typ := awsType(err); typ != "" {
		t.Errorf("%s = %v unwraps to the SDK's %s", what, err, typ)
	}
}

// unparseable is a reply body that is no S3 error document: a proxy's page.
var unparseable = []byte("<html><body>502 from a proxy in front of the service</body></html>")

// Every status from 100 to 599 but the 2xx, with no code, a code the table
// names, one it does not, and a body that does not parse, answered to each
// call: the call fails with exactly one sentinel and no SDK type, after one
// request. Only an absent key to a one-name delete is the delete done. Each
// call has a rig of its own, so the transport's record of its 2,000 requests
// is released before the next call's leak watch.
func TestEveryReplyWrapsOneSentinel(t *testing.T) {
	type reply struct {
		code string
		body []byte
	}

	replies := []reply{{"", nil}, {"SlowDown", nil}, {"NoSuchKey", nil}, {"NotACodeAnyServiceSends", nil}, {"", unparseable}}

	for _, c := range everyCall {
		t.Run(c.name, func(t *testing.T) {
			r := newFaultRig(t)

			for status := 100; status <= 599; status++ {
				if status >= 200 && status <= 299 {
					continue
				}

				for _, rp := range replies {
					n, err := r.once(t, c, s3fault.Answer(status, rp.code, rp.body))

					what := fmt.Sprintf("%s answered %d %q (unparseable %v)", c.name, status, rp.code, rp.body != nil)
					if err == nil && c.name == "delete_one_name" && status == 404 && rp.code == "NoSuchKey" {
						continue
					}

					failsOnce(t, what, err, n)

					if t.Failed() {
						t.FailNow() // the first wrong answer; the rest would repeat it
					}
				}
			}
		})
	}
}

// The refusals with a cause: each quota reply of the profile's list and 507
// are cause quota, a 403 cause access, to every call; a 413 with and without
// a code and a 400 EntityTooLarge are cause size to a block put and
// ErrInvalid to a control put.
func TestRefusalsCarryTheirCause(t *testing.T) {
	r := newFaultRig(t)

	type refusal struct {
		status int
		code   string
		body   []byte
	}

	var quota []refusal
	for _, code := range quotaCodes {
		quota = append(quota, refusal{400, code, nil}, refusal{403, code, nil})
	}
	quota = append(quota, refusal{507, "", nil}, refusal{507, "", unparseable})

	access := []refusal{{403, "AccessDenied", nil}, {403, "", nil}, {403, "", unparseable}}

	for _, c := range everyCall {
		for _, rf := range quota {
			n, err := r.once(t, c, s3fault.Answer(rf.status, rf.code, rf.body))
			what := fmt.Sprintf("%s answered %d %q", c.name, rf.status, rf.code)
			failsOnce(t, what, err, n)
			checkErr(t, what, err, nil, remote.CauseQuota)
		}

		for _, rf := range access {
			n, err := r.once(t, c, s3fault.Answer(rf.status, rf.code, rf.body))
			what := fmt.Sprintf("%s answered %d %q", c.name, rf.status, rf.code)
			failsOnce(t, what, err, n)
			checkErr(t, what, err, nil, remote.CauseAccess)
		}
	}

	size := []refusal{{413, "EntityTooLarge", nil}, {413, "", nil}, {413, "", unparseable}, {400, "EntityTooLarge", nil}}

	for _, c := range everyCall[:2] { // put, then put_control
		for _, rf := range size {
			n, err := r.once(t, c, s3fault.Answer(rf.status, rf.code, rf.body))
			what := fmt.Sprintf("%s answered %d %q", c.name, rf.status, rf.code)
			failsOnce(t, what, err, n)

			if c.name == "put" {
				checkErr(t, what, err, nil, remote.CauseSize)
			} else {
				checkErr(t, what, err, remote.ErrInvalid, 0)
			}
		}
	}
}

// With every request answered with the throttling reply, every call is
// ErrThrottled, which also matches ErrTransient, after one request.
func TestThrottledOnEveryRequest(t *testing.T) {
	r := newFaultRig(t)

	for _, throttle := range []struct {
		status int
		code   string
	}{{503, "SlowDown"}, {429, ""}} {
		remove := r.tr.Inject(s3fault.Rule{}, s3fault.Answer(throttle.status, throttle.code, nil))

		for _, c := range everyCall {
			before := len(r.tr.Requests())
			err := c.run(t.Context(), r)
			what := fmt.Sprintf("%s under %d %q on every request", c.name, throttle.status, throttle.code)

			failsOnce(t, what, err, len(r.tr.Requests())-before)
			checkErr(t, what, err, remote.ErrThrottled, 0)

			if !errors.Is(err, remote.ErrTransient) {
				t.Errorf("%s = %v, want it to match ErrTransient too", what, err)
			}
		}

		remove()
	}
}

// Failures that carry no reply, or a reply the call cannot trust: a request
// dropped unsent, a reply lost after the service acted, a reply body cut
// short, and a byte flipped in a body the call verifies. Each fails with
// exactly one sentinel and no SDK type, after one request; a response checksum
// mismatch on a get is ErrTransient, never ErrCorrupt.
func TestFaultsWithoutAReplyWrapOneSentinel(t *testing.T) {
	r := newFaultRig(t)

	for _, c := range everyCall {
		r.f.dropNext(c.op)
		before := len(r.tr.Requests())
		err := c.run(t.Context(), r)
		what := c.name + " dropped unsent"
		failsOnce(t, what, err, len(r.tr.Requests())-before)
		checkErr(t, what, err, remote.ErrTransient, 0)

		if errors.Is(err, errDropped) {
			t.Errorf("%s = %v: the fault's own error is reachable", what, err)
		}

		n, err := r.once(t, c, s3fault.LoseReply())
		what = c.name + " with its reply lost"
		failsOnce(t, what, err, n)
		checkErr(t, what, err, remote.ErrTransient, 0)

		if c.body {
			n, err = r.once(t, c, s3fault.TruncateReply(10))
			what = c.name + " with its reply cut after 10 bytes"
			failsOnce(t, what, err, n)
			checkErr(t, what, err, remote.ErrTransient, 0)
		}
	}

	for _, tc := range []struct {
		call storeCall
		want error
	}{
		{everyCall[0], remote.ErrCorrupt},   // put: the service refuses the flipped body
		{everyCall[1], remote.ErrCorrupt},   // put_control
		{everyCall[2], remote.ErrTransient}, // get: the response checksum mismatches
		{everyCall[4], remote.ErrTransient}, // get_control
	} {
		n, err := r.once(t, tc.call, s3fault.FlipByte(7))
		what := tc.call.name + " with a byte of its body flipped"
		failsOnce(t, what, err, n)
		checkErr(t, what, err, tc.want, 0)
	}
}

// A call whose context is cancelled while its request is held back returns
// context.Canceled itself, no sentinel, after one request.
func TestCancelledCallReturnsTheContextsError(t *testing.T) {
	r := newFaultRig(t)

	for _, c := range everyCall {
		reached, release := r.f.stallNext(c.op)

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		before := len(r.tr.Requests())

		go func() { done <- c.run(ctx, r) }()

		<-reached
		cancel()

		err := <-done
		close(release) // the held At, which the transport abandoned, returns

		if n := len(r.tr.Requests()) - before; n != 1 {
			t.Errorf("%s cancelled while held: %d requests, want 1", c.name, n)
		}

		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s cancelled while held = %v, want context.Canceled", c.name, err)
		}

		if why, _ := oneSentinel(err); why != "matches 0 sentinels" {
			t.Errorf("%s cancelled while held = %v: %s, want no sentinel", c.name, err, why)
		}
	}
}

// errDropped is the error a dropped request fails with.
var errDropped = errors.New("faults test: the request was dropped unsent")

// armed is a fault that passes every request until armed for an op, then
// drops the next request of it unsent, or holds it until released.
type armed struct {
	mu      sync.Mutex
	op      string
	drop    bool
	reached chan struct{}
	release chan struct{}
}

// dropNext drops the next request of op unsent.
func (f *armed) dropNext(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.op, f.drop = op, true
}

// stallNext holds the next request of op until release is closed, and
// returns a channel closed once that request is held.
func (f *armed) stallNext(op string) (reached <-chan struct{}, release chan<- struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.op, f.drop = op, false
	f.reached, f.release = make(chan struct{}), make(chan struct{})

	return f.reached, f.release
}

func (f *armed) At(op string, _ []byte) (time.Duration, int, error) {
	f.mu.Lock()
	if f.op == "" || op != f.op {
		f.mu.Unlock()
		return 0, 0, nil
	}

	drop, reached, release := f.drop, f.reached, f.release
	f.op, f.reached, f.release = "", nil, nil
	f.mu.Unlock()

	if drop {
		return 0, 0, errDropped
	}

	close(reached)
	<-release

	return 0, 0, nil
}
