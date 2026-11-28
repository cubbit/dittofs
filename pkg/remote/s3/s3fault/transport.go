package s3fault

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
)

// Fault decides each request's fault; *sim.Schedule satisfies it as it is.
// It is called once per request, with the request's op and key, before the
// request is sent. At waits out any delay itself, on its own clock, and the
// transport only reads the delay it returns. Zero results pass; err alone drops the request unsent and
// returns err; tearAfter > 0 with err set, on a PutObject only, sends that
// many bytes of a body longer than that, then cuts it and returns err. Every
// other tear, and a loss (tearAfter < 0), panics, naming the op. A panic
// raised on a goroutine the client started ends the test binary.
type Fault interface {
	At(op string, key []byte) (delay time.Duration, tearAfter int, err error)
}

// Other is the op of a request that carries no SDK operation name.
const Other = "Other"

// Transport is a fault wrapper around a real http.RoundTripper: it consults a
// Fault and the effects injected into it before each request, and keeps a
// record of every request that reached it, those it dropped or answered
// included. It never answers a request with a
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
	out := slices.Clone(t.requests)
	for i := range out {
		out[i].Header = out[i].Header.Clone()
	}
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
			panic(fmt.Sprintf("s3fault: a tear of the op %s with error %v: only a PutObject tears, and only failing", op, err))
		case tearAfter > 0 && (req.Body == nil || int64(tearAfter) >= req.ContentLength):
			// A cut at or past the body's end would send it whole, and the
			// service would store the put the client was told failed.
			panic(fmt.Sprintf("s3fault: a tear of the op %s after %d bytes of a %d-byte body: a tear cuts part-way", op, tearAfter, req.ContentLength))
		case tearAfter > 0:
			req = req.Clone(req.Context())
			req.Body = &cut{ReadCloser: req.Body, left: int64(tearAfter), end: err}
			req.GetBody = nil // so net/http cannot resend the body whole
			if resp, sendErr := t.next.RoundTrip(req); sendErr == nil {
				t.replied(n, resp)
				_ = resp.Body.Close()
			}
			return nil, err
		case err != nil:
			closeBody(req)
			return nil, err
		}
	}

	var effects []Effect
	for _, in := range t.selecting(req, op, key) {
		if !t.claim(in) {
			continue
		}
		effects = append(effects, in.effect)
		var resp *http.Response
		if req, resp = in.effect.before(req, op, key); resp != nil {
			closeBody(req)
			return resp, nil
		}
	}

	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	t.replied(n, resp)

	for _, e := range effects {
		if resp, err = e.after(resp); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// replied records the protocol of the reply to request n.
func (t *Transport) replied(n int, resp *http.Response) {
	t.mu.Lock()
	t.requests[n].ProtoMajor = resp.ProtoMajor
	t.mu.Unlock()
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
	if err == io.EOF && b.left > 0 {
		err = nil // a body that ends before the cut still ends with end
	}
	return n, err
}
