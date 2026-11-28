//go:build integration

package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/s3/s3fault"
)

// faultRig is one namespace's store in batch and in one-name delete modes,
// over a fresh LocalStack bucket holding one block and the health object, its
// every request passing through one fault transport and one armed fault.
// Every request it sends is checked for its signing when the test ends.
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

	r := &faultRig{f: &armed{}, block: remote.Name{0xfa}}
	r.batch, r.tr = newGetRig(t).openFaulted(t, deleting, r.f)
	t.Cleanup(func() { checkSigned(t, []*s3fault.Transport{r.tr}) })

	var err error
	if r.oneName, err = open(r.batch.c, r.batch.prefix, remote.Mutable, crc); err != nil {
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

// putBody is the body of the put calls: longer than a tear's cut.
var putBody = []byte("a block put under a fault, cut part-way when torn")

var everyCall = []storeCall{
	{"put", "PutObject", false, func(ctx context.Context, r *faultRig) error {
		return r.batch.Put(ctx, remote.Name{0xb0}, bytes.NewReader(putBody), int64(len(putBody)), crc32cOf(putBody))
	}},
	{"put_control", "PutObject", false, func(ctx context.Context, r *faultRig) error {
		return r.batch.PutControl(ctx, remote.RoleClaim, putBody)
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

// callNamed is everyCall's call of that name, failing t when there is none.
func callNamed(t *testing.T, name string) storeCall {
	t.Helper()

	for _, c := range everyCall {
		if c.name == name {
			return c
		}
	}

	t.Fatalf("no call named %s", name)

	return storeCall{}
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

// requests counts the requests the rig's transport has seen.
func (r *faultRig) requests() int { return len(r.tr.Requests()) }

// once makes c's call with e applied to its one request, and returns how many
// requests it made and its error.
func (r *faultRig) once(t *testing.T, c storeCall, e s3fault.Effect) (int, error) {
	t.Helper()

	remove := r.tr.Inject(s3fault.Rule{Op: c.op, Times: 1}, e)
	defer remove()

	before := r.requests()
	err := c.run(t.Context(), r)

	return r.requests() - before, err
}

// fails fails t unless err, made in one request, wraps want alone (or, with
// want nil, is a denial of cause) and reaches no SDK or network type; what
// names the case.
func fails(t *testing.T, what string, requests int, err, want error, cause remote.Cause) {
	t.Helper()

	checkErr(t, what, err, want, cause)
	oneAttempt(t, what, requests, err)
}

// oneAttempt fails t unless err was made in one request and reaches no SDK or
// network type.
func oneAttempt(t *testing.T, what string, requests int, err error) {
	t.Helper()

	if requests != 1 {
		t.Errorf("%s: %d requests, want 1: one failing call is one attempt", what, requests)
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
// request; but an absent key to a one-name delete, which is the delete done.
// Each call has a rig of its own, so the transport's record of its 2,000
// requests is released before the next call's leak watch.
func TestEveryReplyWrapsOneSentinel(t *testing.T) {
	replies := []struct {
		code string
		body []byte
	}{{"", nil}, {"SlowDown", nil}, {"NoSuchKey", nil}, {"NotACodeAnyServiceSends", nil}, {"", unparseable}}

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

					if c.name == "delete_one_name" && status == 404 && rp.code == "NoSuchKey" {
						if err != nil || n != 1 {
							t.Errorf("%s = %v after %d requests, want nil after 1: an absent key is the delete done", what, err, n)
						}

						continue
					}

					if why, _ := oneSentinel(err); why != "" {
						t.Errorf("%s = %v: %s, want exactly one sentinel", what, err, why)
					}

					oneAttempt(t, what, n, err)

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
		for _, cause := range []struct {
			refusals []refusal
			cause    remote.Cause
		}{{quota, remote.CauseQuota}, {access, remote.CauseAccess}} {
			for _, rf := range cause.refusals {
				n, err := r.once(t, c, s3fault.Answer(rf.status, rf.code, rf.body))
				fails(t, fmt.Sprintf("%s answered %d %q", c.name, rf.status, rf.code), n, err, nil, cause.cause)
			}
		}
	}

	size := []refusal{{413, "EntityTooLarge", nil}, {413, "", nil}, {413, "", unparseable}, {400, "EntityTooLarge", nil}}

	for _, rf := range size {
		n, err := r.once(t, callNamed(t, "put"), s3fault.Answer(rf.status, rf.code, rf.body))
		fails(t, fmt.Sprintf("put answered %d %q", rf.status, rf.code), n, err, nil, remote.CauseSize)

		n, err = r.once(t, callNamed(t, "put_control"), s3fault.Answer(rf.status, rf.code, rf.body))
		fails(t, fmt.Sprintf("put_control answered %d %q", rf.status, rf.code), n, err, remote.ErrInvalid, 0)
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
		func() {
			defer r.tr.Inject(s3fault.Rule{}, s3fault.Answer(throttle.status, throttle.code, nil))()

			for _, c := range everyCall {
				before := r.requests()
				err := c.run(t.Context(), r)
				what := fmt.Sprintf("%s under %d %q on every request", c.name, throttle.status, throttle.code)

				fails(t, what, r.requests()-before, err, remote.ErrThrottled, 0)

				if !errors.Is(err, remote.ErrTransient) {
					t.Errorf("%s = %v, want it to match ErrTransient too", what, err)
				}
			}
		}()
	}
}

// Failures that carry no reply, or a reply the call cannot trust: a request
// dropped unsent, a put torn part-way, a reply lost after the service acted, a
// reply body cut short, and a byte flipped in a body the call verifies. Each
// fails with exactly one sentinel and no SDK type, after one request; a
// response checksum mismatch on a get is ErrTransient, never ErrCorrupt.
func TestFaultsWithoutAReplyWrapOneSentinel(t *testing.T) {
	r := newFaultRig(t)

	for _, c := range everyCall {
		modes := []mode{modeDrop}
		if c.op == "PutObject" {
			modes = append(modes, modeTear)
		}

		for _, m := range modes {
			r.f.arm(c.op, m)
			before := r.requests()
			err := c.run(t.Context(), r)
			what := fmt.Sprintf("%s %s", c.name, m)
			fails(t, what, r.requests()-before, err, remote.ErrTransient, 0)

			if errors.Is(err, errInjected) {
				t.Errorf("%s = %v: the fault's own error is reachable", what, err)
			}

			if r.f.armedFor() != "" {
				t.Errorf("%s: the fault was never reached, want the call's one request %s", what, c.op)
			}
		}

		n, err := r.once(t, c, s3fault.LoseReply())
		fails(t, c.name+" with its reply lost", n, err, remote.ErrTransient, 0)

		if c.body {
			n, err = r.once(t, c, s3fault.TruncateReply(10))
			fails(t, c.name+" with its reply cut after 10 bytes", n, err, remote.ErrTransient, 0)
		}
	}

	for name, want := range map[string]error{
		"put":         remote.ErrCorrupt, // the service refuses the flipped body
		"put_control": remote.ErrCorrupt,
		"get":         remote.ErrTransient, // the response checksum mismatches
		"get_control": remote.ErrTransient,
	} {
		n, err := r.once(t, callNamed(t, name), s3fault.FlipByte(7))
		fails(t, name+" with a byte of its body flipped", n, err, want, 0)
	}
}

// backstop bounds each wait of the cancel row on the call under test; nothing
// here takes a clock, so the bound is the assertion.
const backstop = 30 * time.Second

// A call whose context is cancelled while its request is held back, and a put
// cancelled while its body streams, each return context.Canceled, no sentinel
// and no SDK type.
func TestCancelledCallReturnsTheContextsError(t *testing.T) {
	r := newFaultRig(t)

	for _, c := range everyCall {
		ctx, cancel := context.WithCancel(t.Context())
		reached, release := r.f.stall(c.op)
		done := make(chan error, 1)
		before := r.requests()

		go func() { done <- c.run(ctx, r) }()

		select {
		case <-reached:
		case err := <-done:
			t.Fatalf("%s returned %v before its request %s was held", c.name, err, c.op)
		case <-time.After(backstop):
			t.Fatalf("%s's request %s was not held within %v", c.name, c.op, backstop)
		}

		cancel()

		var err error
		select {
		case err = <-done:
		case <-time.After(backstop):
			t.Fatalf("%s, cancelled while held, did not return within %v", c.name, backstop)
		}

		close(release) // the held At, which the transport abandoned, returns

		if n := r.requests() - before; n != 1 {
			t.Errorf("%s cancelled while held: %d requests, want 1", c.name, n)
		}

		cancelled(t, c.name+" cancelled while held", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	body := bytes.Repeat([]byte("x"), 64<<10)
	err := r.batch.Put(ctx, remote.Name{0xca}, &cancelsOnRead{r: bytes.NewReader(body), cancel: cancel}, int64(len(body)), crc32cOf(body))
	cancelled(t, "put cancelled while its body streams", err)
}

// cancelled fails t unless err is context.Canceled with no sentinel and no SDK
// type; what names the case.
func cancelled(t *testing.T, what string, err error) {
	t.Helper()

	if !errors.Is(err, context.Canceled) {
		t.Errorf("%s = %v, want context.Canceled", what, err)
	}

	if why, _ := oneSentinel(err); why != "matches 0 sentinels" {
		t.Errorf("%s = %v: %s, want no sentinel", what, err, why)
	}

	noSDKType(t, what, err)

	if typ := awsType(err); typ != "" {
		t.Errorf("%s = %v unwraps to the SDK's %s", what, err, typ)
	}
}

// cancelsOnRead is a put's body that cancels its call's context at its first
// read, then serves r.
type cancelsOnRead struct {
	r      io.Reader
	cancel context.CancelFunc
}

func (b *cancelsOnRead) Read(p []byte) (int, error) {
	b.cancel()
	return b.r.Read(p)
}

// errInjected is the error a dropped or torn request fails with.
var errInjected = errors.New("faults test: the request was dropped or cut")

// mode is what an armed fault does to the next request of its op.
type mode int

const (
	modeDrop  mode = iota + 1 // dropped unsent
	modeTear                  // a PutObject cut after tearAt bytes of its body
	modeStall                 // held until released
)

// tearAt is where a torn put's body is cut, inside putBody.
const tearAt = 5

func (m mode) String() string {
	return map[mode]string{modeDrop: "dropped unsent", modeTear: "torn part-way", modeStall: "held"}[m]
}

// armed is a fault that passes every request until armed for an op, then acts
// on the next request of it in its mode, once.
type armed struct {
	mu      sync.Mutex
	op      string
	mode    mode
	reached chan struct{}
	release chan struct{}
}

// arm sets the next request of op to be dropped or torn.
func (f *armed) arm(op string, m mode) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.op, f.mode = op, m
}

// stall holds the next request of op until release is closed, and returns a
// channel closed once that request is held.
func (f *armed) stall(op string) (reached <-chan struct{}, release chan<- struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.op, f.mode = op, modeStall
	f.reached, f.release = make(chan struct{}), make(chan struct{})

	return f.reached, f.release
}

// armedFor is the op the fault is armed for, "" once it has acted.
func (f *armed) armedFor() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.op
}

func (f *armed) At(op string, _ []byte) (time.Duration, int, error) {
	f.mu.Lock()
	if f.op == "" || op != f.op {
		f.mu.Unlock()
		return 0, 0, nil
	}

	m, reached, release := f.mode, f.reached, f.release
	f.op, f.reached, f.release = "", nil, nil
	f.mu.Unlock()

	switch m {
	case modeDrop:
		return 0, 0, errInjected
	case modeTear:
		return 0, tearAt, errInjected
	}

	close(reached)
	<-release

	return 0, 0, nil
}
