package s3fault

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Rule selects the requests an effect applies to; the zero Rule matches every
// request, those on the bucket itself included.
type Rule struct {
	Op        string // an operation name, as Request.Op has it; "" matches every one
	KeyPrefix string
	Times     int   // how many matching requests, from the next; 0 means every one
	MinBody   int64 // only requests whose Content-Length is at least this
}

// Effect is what the transport does to a request a Rule selects. It is sealed:
// only this package's constructors build one.
type Effect interface {
	// before acts on the request, of op on key, before it is sent, and
	// returns it, or a clone it changed: never req changed in place, which
	// the client still holds. A response it returns answers the request,
	// which is then never sent.
	before(req *http.Request, op, key string) (*http.Request, *http.Response)
	// after acts on the service's reply.
	after(resp *http.Response) (*http.Response, error)
}

// injected is one Inject call: its rule, its effect and how many more
// requests it applies to.
type injected struct {
	rule   Rule
	effect Effect
	left   int // of rule.Times; unused when it is 0
}

// Inject applies e to every request r selects from now on, after the
// scheduled fault, until remove is called. Every injection that selects a
// request applies, in the order they were injected, on the request and then
// on the reply. Once one answers the request, those after it neither act nor
// count it against their Times.
func (t *Transport) Inject(r Rule, e Effect) (remove func()) {
	in := &injected{rule: r, effect: e, left: r.Times}
	t.mu.Lock()
	t.effects = append(t.effects, in)
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.effects = slices.DeleteFunc(t.effects, func(x *injected) bool { return x == in })
	}
}

// selecting returns the injections whose rules select the request, Times
// aside.
func (t *Transport) selecting(req *http.Request, op, key string) []*injected {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*injected
	for _, in := range t.effects {
		r := in.rule
		if (r.Op == "" || r.Op == op) && strings.HasPrefix(key, r.KeyPrefix) && (r.MinBody == 0 || req.ContentLength >= r.MinBody) {
			out = append(out, in)
		}
	}
	return out
}

// claim counts one request against in's Times, and reports false when none
// is left.
func (t *Transport) claim(in *injected) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if in.rule.Times == 0 {
		return true
	}
	if in.left == 0 {
		return false
	}
	in.left--
	return true
}

// Answer answers the request with status and code without sending it. A nil
// body is an S3 error document carrying code; any other body is sent as given,
// so a test can answer one that does not parse.
//
// decision: a 2xx status panics. The transport never answers a request in the
// service's place with a success, since a success it made up would report a
// block stored, or read, that the service never held.
func Answer(status int, code string, body []byte) Effect {
	switch {
	case status >= 200 && status <= 299:
		panic(fmt.Sprintf("s3fault: Answer of the success %d: only the service answers a success", status))
	case status < 100 || status > 599:
		panic(fmt.Sprintf("s3fault: Answer of the status %d, outside 100-599", status))
	}
	if body == nil {
		body, _ = xml.Marshal(struct {
			XMLName   xml.Name `xml:"Error"`
			Code      string
			Message   string
			RequestID string `xml:"RequestId"`
		}{Code: code, Message: "answered by s3fault", RequestID: "s3fault"})
		body = append([]byte(xml.Header), body...)
	}
	return answer{status: status, body: body}
}

type answer struct {
	status int
	body   []byte
}

func (a answer) before(req *http.Request, _, _ string) (*http.Request, *http.Response) {
	body := a.body
	if req.Method == http.MethodHead {
		body = nil // as the service answers a HEAD: the status alone
	}
	return req, &http.Response{
		Status:        strconv.Itoa(a.status) + " " + http.StatusText(a.status),
		StatusCode:    a.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/xml"}, "Content-Length": {strconv.Itoa(len(body))}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func (answer) after(resp *http.Response) (*http.Response, error) { return resp, nil }

// errReplyLost is what a request under LoseReply returns.
var errReplyLost = errors.New("s3fault: the service's reply was lost")

// LoseReply sends the request, lets the service apply it, then discards the
// reply and returns an error, as a connection lost after the service acted
// would.
func LoseReply() Effect { return loseReply{} }

type loseReply struct{}

func (loseReply) before(req *http.Request, _, _ string) (*http.Request, *http.Response) {
	return req, nil
}

func (loseReply) after(resp *http.Response) (*http.Response, error) {
	_ = resp.Body.Close()
	return nil, errReplyLost
}

// TruncateReply ends the reply's body after n bytes, leaving its headers,
// Content-Length and Content-Range as the service sent them.
//
// decision: the body ends with io.EOF, not io.ErrUnexpectedEOF, so only a
// reader that compares the bytes it got with Content-Range sees it short.
func TruncateReply(n int64) Effect { return truncateReply{n} }

type truncateReply struct{ n int64 }

func (truncateReply) before(req *http.Request, _, _ string) (*http.Request, *http.Response) {
	return req, nil
}

func (e truncateReply) after(resp *http.Response) (*http.Response, error) {
	resp.Body = &cut{ReadCloser: resp.Body, left: e.n, end: io.EOF}
	return resp, nil
}

// FlipByte inverts the byte at offset at of the request's body when it has
// one, else of the reply's, changing no length. A body shorter than at+1 is
// left whole.
func FlipByte(at int64) Effect {
	if at < 0 {
		panic(fmt.Sprintf("s3fault: FlipByte at %d, before the body", at))
	}
	return flipByte{at}
}

type flipByte struct{ at int64 }

func (e flipByte) before(req *http.Request, _, _ string) (*http.Request, *http.Response) {
	if !hasBody(req) {
		return req, nil
	}
	req = req.Clone(req.Context())
	req.Body = &flip{ReadCloser: req.Body, at: e.at}
	if get := req.GetBody; get != nil {
		// net/http resends a body through GetBody; the resent one is flipped too.
		req.GetBody = func() (io.ReadCloser, error) {
			b, err := get()
			if err != nil {
				return nil, err
			}
			return &flip{ReadCloser: b, at: e.at}, nil
		}
	}
	return req, nil
}

// after tells the sides apart by resp.Request, which net/http sets to the
// request sent.
func (e flipByte) after(resp *http.Response) (*http.Response, error) {
	if resp.Request != nil && hasBody(resp.Request) {
		return resp, nil // the request's body was the one flipped
	}
	resp.Body = &flip{ReadCloser: resp.Body, at: e.at}
	return resp, nil
}

func hasBody(req *http.Request) bool { return req.Body != nil && req.Body != http.NoBody }

// flip inverts the byte at offset at of the body it reads.
type flip struct {
	io.ReadCloser
	at, off int64
}

func (f *flip) Read(p []byte) (int, error) {
	n, err := f.ReadCloser.Read(p)
	if i := f.at - f.off; i >= 0 && i < int64(n) {
		p[i] ^= 0xff
	}
	f.off += int64(n)
	return n, err
}

// StripHeaders removes the named headers from the request before it is sent,
// leaving the record with them as the client built them. With Rule.MinBody it
// strips only from requests at or above that size. Host, Content-Length and
// Transfer-Encoding are written from the request's fields, not its header,
// so they cannot be stripped this way.
//
// decision: the headers are removed after the client signed them, so a
// service that validates the signature refuses the request. LocalStack checks
// no signature, which a test pins; against a service that does, a strip needs
// the request signed again.
func StripHeaders(names ...string) Effect { return stripHeaders(names) }

type stripHeaders []string

func (e stripHeaders) before(req *http.Request, _, _ string) (*http.Request, *http.Response) {
	req = req.Clone(req.Context())
	for _, name := range e {
		req.Header.Del(name)
	}
	return req, nil
}

func (stripHeaders) after(resp *http.Response) (*http.Response, error) { return resp, nil }

// RecodeError rewrites a service's error reply whose S3 error code is from to
// status and code, as a service that refuses the same request under another
// code would. Every other reply passes as the service gave it: a success, an
// error under another code, and one with no error document, as a HEAD's.
// A status outside 300-599 panics: only an error replaces an error.
func RecodeError(from string, status int, code string) Effect {
	if status < 300 || status > 599 {
		panic(fmt.Sprintf("s3fault: RecodeError to the status %d: only an error status, 300-599, replaces one", status))
	}
	return recodeError{from: from, status: status, code: code}
}

type recodeError struct {
	from, code string
	status     int
}

func (recodeError) before(req *http.Request, _, _ string) (*http.Request, *http.Response) {
	return req, nil
}

func (e recodeError) after(resp *http.Response) (*http.Response, error) {
	if resp.StatusCode < 300 {
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body) // an error document is small
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var doc struct{ Code string }
	from := []byte("<Code>" + e.from + "</Code>")
	if xml.Unmarshal(body, &doc) == nil && doc.Code == e.from && bytes.Contains(body, from) {
		body = bytes.Replace(body, from, []byte("<Code>"+e.code+"</Code>"), 1)
		resp.StatusCode = e.status
		resp.Status = strconv.Itoa(e.status) + " " + http.StatusText(e.status)
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}
