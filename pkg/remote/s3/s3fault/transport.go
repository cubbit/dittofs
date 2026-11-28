package s3fault

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
)

// Fault decides each request's fault; *sim.Schedule satisfies it as it is.
// It is called once per request, with the request's op and key, before the
// request is sent. Zero results pass; err alone drops the request unsent and
// returns err; tearAfter > 0 with err set, on a PutObject only, sends that
// many bytes of the body, then cuts it and returns err. Every other tear, and
// a loss (tearAfter < 0), panics, naming the op.
type Fault interface {
	At(op string, key []byte) (delay time.Duration, tearAfter int, err error)
}

// Other is the op of a request that carries no SDK operation name.
const Other = "Other"

// Transport is a fault wrapper around a real http.RoundTripper: it consults a
// Fault and the effects injected into it before each request, and keeps a
// record of every request that reached it. It never answers a request with a
// success the service did not give. It is safe for concurrent use.
type Transport struct {
	next  http.RoundTripper
	fault Fault

	mu       sync.Mutex
	effects  []*injected
	requests []Request
}

// Request is one request as it reached the transport.
type Request struct {
	Op, Key    string
	Header     http.Header // as the client built it, before any effect
	ProtoMajor int         // of the service's reply; 0 when none came
}

// New wraps next, the client's own configured transport, with fault; a nil
// fault schedules none.
func New(next http.RoundTripper, fault Fault) *Transport {
	return &Transport{next: next, fault: fault}
}

// Requests returns every request that reached the transport, in the order
// they did.
func (t *Transport) Requests() []Request {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Request, len(t.requests))
	copy(out, t.requests)
	return out
}

// RoundTrip applies the scheduled fault, then the effects whose rules select
// the request, and sends it to the wrapped transport unless one of them
// answers it first.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	op := awsmiddleware.GetOperationName(req.Context())
	if op == "" {
		op = Other
	}
	key := keyOf(req.URL.Path)

	t.mu.Lock()
	n := len(t.requests)
	t.requests = append(t.requests, Request{Op: op, Key: key, Header: req.Header.Clone()})
	t.mu.Unlock()

	if t.fault != nil {
		tearAfter, err := t.at(req, op, key)
		switch {
		case tearAfter < 0:
			// decision: a loss would report a request done that the service
			// never applied, and a tear with no error would report a put
			// whole that the service never stored: either answers a success
			// in the service's place, which the fault transport must never do.
			panic("s3fault: a loss of the op " + op + ": the service would never apply what it reports done")
		case tearAfter > 0 && (err == nil || op != "PutObject"):
			panic("s3fault: a tear of the op " + op + " with error " + errString(err) + ": only a PutObject tears, and only failing")
		case tearAfter > 0:
			req = req.Clone(req.Context())
			req.Body = &cut{ReadCloser: req.Body, left: int64(tearAfter), end: err}
			req.GetBody = nil // so net/http cannot resend the body whole
			if resp, sendErr := t.next.RoundTrip(req); sendErr == nil {
				_ = resp.Body.Close()
			}
			return nil, err
		case err != nil:
			closeBody(req)
			return nil, err
		}
	}

	effects := t.match(req, op, key)
	for _, e := range effects {
		if resp := e.before(req); resp != nil {
			closeBody(req)
			return resp, nil
		}
	}

	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.requests[n].ProtoMajor = resp.ProtoMajor
	t.mu.Unlock()

	for _, e := range effects {
		if resp, err = e.after(resp); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// at consults the fault from a goroutine of its own, so a request whose
// context ends while the fault waits out a delay returns the context's error
// at once.
//
// decision: the abandoned At keeps waiting on the fault's clock, and its
// goroutine returns only when the test moves that clock past the delay; the
// transport has no clock of its own to cancel it with.
func (t *Transport) at(req *http.Request, op, key string) (tearAfter int, err error) {
	type result struct {
		tearAfter int
		err       error
	}
	done := make(chan result, 1)
	go func() {
		_, tearAfter, err := t.fault.At(op, []byte(key))
		done <- result{tearAfter, err}
	}()
	select {
	case r := <-done:
		return r.tearAfter, r.err
	case <-req.Context().Done():
		return 0, req.Context().Err()
	}
}

// keyOf is a path-style request's object key: its path less the bucket.
func keyOf(path string) string {
	_, key, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	return key
}

func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

// cut passes the first left bytes of a body, then ends it with end.
type cut struct {
	io.ReadCloser
	left int64
	end  error
}

func (b *cut) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, b.end
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, err
}
