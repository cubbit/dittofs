package s3fault

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
)

// HoldNewKeys holds each key put through the transport out of the first
// listings ListObjectsV2 replies that list it, as a service whose listings
// lag its writes would. Each key is counted on its own, from its first
// successful put; every other key stays in the reply, in its place.
//
// decision: the effect learns a key only from a put its rule selects, so it
// is injected under the zero Rule, which selects the puts and the listings
// alike. Under Rule{Op: "ListObjectsV2"} it holds nothing, and a KeyPrefix
// selects no listing, whose key is "". A hold of the first keys a listing
// shows, needing no puts, is the upgrade if a row must scope it.
func HoldNewKeys(listings int) Effect {
	return &holdNewKeys{listings: listings, left: map[string]int{}}
}

type holdNewKeys struct {
	listings int

	mu   sync.Mutex
	left map[string]int // each key put, and how many more listings hold it out
}

func (*holdNewKeys) before(req *http.Request, _, _ string) (*http.Request, *http.Response) {
	return req, nil
}

func (h *holdNewKeys) after(resp *http.Response) (*http.Response, error) {
	req := resp.Request
	switch op := awsmiddleware.GetOperationName(req.Context()); {
	case op == "PutObject" && resp.StatusCode/100 == 2:
		key := keyOf(req.URL.Path)
		h.mu.Lock()
		if _, put := h.left[key]; !put {
			h.left[key] = h.listings
		}
		h.mu.Unlock()
		return resp, nil
	case op == "ListObjectsV2" && resp.StatusCode == http.StatusOK:
		// ponytail: the reply's KeyCount still counts the keys held out,
		// which the store never reads. Re-encode the reply through
		// encoding/xml once a consumer reads KeyCount.
		return rewriteBody(resp, func(body []byte) ([]byte, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return dropElements(body, "Contents", func(key string) bool {
				if h.left[key] == 0 {
					return false
				}
				h.left[key]--
				return true
			})
		})
	}
	return resp, nil
}

// FailBatchKey refuses key inside a DeleteObjects batch with code, and lets the
// service delete every other key in it. A batch that does not hold key passes
// as it is, though it counts against the rule's Times. A batch of key alone
// panics: there is no batch left to send, and a single refusal is an Answer.
// It matches by key alone, so it fits an unversioned batch that names each
// key once.
//
// decision: the key is refused for real. It is taken out of the request,
// whose checksum is recomputed as a Content-MD5, so the service never deletes
// it, and an Error entry for it is added to the service's reply. Rewriting
// the reply alone would report refused a key the service deleted; dropping
// the key without an entry would send a reply no service sends with Quiet
// false. The request's signature no longer matches, which holds only for a
// service that checks none, as LocalStack does not.
func FailBatchKey(key, code string) Effect { return &failBatchKey{key: key, code: code} }

type failBatchKey struct{ key, code string }

func (f *failBatchKey) before(req *http.Request, op, _ string) (*http.Request, *http.Response) {
	if op != "DeleteObjects" || req.Body == nil {
		return req, nil
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	var rest []byte
	keys, refused := 0, 0
	if err == nil {
		rest, err = dropElements(body, "Object", func(k string) bool {
			keys++
			if k != f.key {
				return false
			}
			refused++
			return true
		})
	}
	switch {
	case err != nil:
		panic(fmt.Sprintf("s3fault: FailBatchKey cannot read the DeleteObjects request: %v", err))
	case refused == 0:
		return withBody(req.Context(), req, body), nil
	case refused == keys:
		panic(fmt.Sprintf("s3fault: FailBatchKey of %q, the batch's only key: refuse a lone key with Answer", f.key))
	}
	// The service checks the checksum the SDK sent, of whichever algorithm,
	// against the body it gets; one Content-MD5 of the new body replaces it.
	sent := withBody(context.WithValue(req.Context(), f, true), req, rest)
	for name := range sent.Header {
		if strings.HasPrefix(name, "X-Amz-Checksum-") {
			sent.Header.Del(name)
		}
	}
	sum := md5.Sum(rest)
	sent.Header.Set("Content-Md5", base64.StdEncoding.EncodeToString(sum[:]))
	return sent, nil
}

func (f *failBatchKey) after(resp *http.Response) (*http.Response, error) {
	if resp.Request.Context().Value(f) == nil || resp.StatusCode != http.StatusOK {
		return resp, nil
	}
	return rewriteBody(resp, func(body []byte) ([]byte, error) {
		end := bytes.LastIndex(body, []byte("</DeleteResult>"))
		if end < 0 {
			return nil, fmt.Errorf("s3fault: FailBatchKey: the DeleteObjects reply holds no DeleteResult: %.200q", body)
		}
		entry, err := xml.Marshal(struct {
			XMLName            xml.Name `xml:"Error"`
			Key, Code, Message string
		}{Key: f.key, Code: f.code, Message: "refused by s3fault"})
		return slices.Concat(body[:end], entry, body[end:]), err
	})
}

// dropElements returns body less each element named name directly under its
// root whose Key drop reports true for, with every other byte as it was.
func dropElements(body []byte, name string, drop func(key string) bool) (out []byte, err error) {
	d := xml.NewDecoder(bytes.NewReader(body))
	var last int64
	depth := 0
	for {
		start := d.InputOffset()
		tok, err := d.Token()
		if err == io.EOF {
			return append(out, body[last:]...), nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth != 1 || t.Name.Local != name {
				depth++
				continue
			}
			var el struct{ Key string }
			if err := d.DecodeElement(&el, &t); err != nil {
				return nil, err
			}
			if drop(el.Key) {
				out = append(out, body[last:start]...)
				last = d.InputOffset()
			}
		case xml.EndElement:
			depth--
		}
	}
}

// rewriteBody replaces resp's body with what edit makes of it, and its length
// to match.
func rewriteBody(resp *http.Response, edit func([]byte) ([]byte, error)) (*http.Response, error) {
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err == nil {
		body, err = edit(body)
	}
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return resp, nil
}

// withBody is a clone of req under ctx that sends body.
func withBody(ctx context.Context, req *http.Request, body []byte) *http.Request {
	out := req.Clone(ctx)
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	out.ContentLength = int64(len(body))
	return out
}
