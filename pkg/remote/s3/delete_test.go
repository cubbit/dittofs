package s3

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/remote"
)

// The reply accounting over reply values: each requested key's result comes
// from what the reply states of it, never from its silence.
func TestDeleteResults(t *testing.T) {
	del := func(keys ...string) []types.DeletedObject {
		var out []types.DeletedObject
		for _, k := range keys {
			out = append(out, types.DeletedObject{Key: aws.String(k)})
		}
		return out
	}
	fail := func(key string, code *string) types.Error { return types.Error{Key: aws.String(key), Code: code} }

	for _, tc := range []struct {
		why     string
		deleted []types.DeletedObject
		failed  []types.Error
		want    [3]error // for keys a, b, c
	}{
		{"every key deleted", del("a", "b", "c"), nil, [3]error{}},
		{"a quiet reply, naming nothing", nil, nil, [3]error{remote.ErrTransient, remote.ErrTransient, remote.ErrTransient}},
		{"b left out", del("a", "c"), nil, [3]error{nil, remote.ErrTransient, nil}},
		{"b deleted twice", del("a", "b", "b", "c"), nil, [3]error{nil, remote.ErrTransient, nil}},
		{"b deleted and failed", del("a", "b", "c"), []types.Error{fail("b", aws.String("InternalError"))}, [3]error{nil, remote.ErrTransient, nil}},
		{"b failed twice", del("a", "c"), []types.Error{fail("b", aws.String("AccessDenied")), fail("b", aws.String("AccessDenied"))}, [3]error{nil, remote.ErrTransient, nil}},
		{"b refused", del("a", "c"), []types.Error{fail("b", aws.String("AccessDenied"))}, [3]error{nil, remote.ErrDenied, nil}},
		{"b throttled", del("a", "c"), []types.Error{fail("b", aws.String("SlowDown"))}, [3]error{nil, remote.ErrThrottled, nil}},
		{"b absent", del("a", "c"), []types.Error{fail("b", aws.String("NoSuchKey"))}, [3]error{}},
		{"b failed with an unlisted code", del("a", "c"), []types.Error{fail("b", aws.String("NoSuchCode"))}, [3]error{nil, remote.ErrTransient, nil}},
		{"b failed with no code", del("a", "c"), []types.Error{fail("b", nil)}, [3]error{nil, remote.ErrTransient, nil}},
		{"a key never requested", del("a", "b", "c", "d"), []types.Error{fail("e", aws.String("AccessDenied"))}, [3]error{}},
	} {
		got := deleteResults([]string{"a", "b", "c"}, tc.deleted, tc.failed)
		if len(got) != 3 {
			t.Fatalf("[%s] %d results, want 3", tc.why, len(got))
		}
		for i, want := range tc.want {
			switch {
			case want == nil && got[i] != nil:
				t.Errorf("[%s] key %c: %v, want nil", tc.why, 'a'+i, got[i])
			case want != nil && !errors.Is(got[i], want):
				t.Errorf("[%s] key %c: %v, want %v", tc.why, 'a'+i, got[i], want)
			}
		}
	}
}

// batchRequest is what a DeleteObjects request carried, as the service read it.
type batchRequest struct {
	keys    []string
	quiet   bool
	md5OK   bool     // Content-MD5 is the MD5 of the body received
	headers []string // checksum headers other than Content-MD5
	payload string   // x-amz-content-sha256
}

// batchServer is a TLS service that reads each DeleteObjects request and
// answers it with every key deleted but those in refuse, answered with their
// code. Any other request is answered by other, or 204.
type batchServer struct {
	mu       sync.Mutex
	got      []batchRequest
	inFlight atomic.Int32
	overlap  atomic.Bool // two requests were served at once
	refuse   map[string]string
	other    http.HandlerFunc
}

func (b *batchServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if b.inFlight.Add(1) > 1 {
		b.overlap.Store(true)
	}
	defer b.inFlight.Add(-1)

	if !r.URL.Query().Has("delete") {
		if b.other != nil {
			b.other(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	body, _ := io.ReadAll(r.Body)
	var in struct {
		Object []struct{ Key string }
		Quiet  bool
	}
	if err := xml.Unmarshal(body, &in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	sum := md5.Sum(body)
	req := batchRequest{
		quiet:   in.Quiet,
		md5OK:   r.Header.Get("Content-Md5") == base64.StdEncoding.EncodeToString(sum[:]),
		payload: r.Header.Get("X-Amz-Content-Sha256"),
	}
	for k := range r.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-amz-checksum") || lk == "x-amz-sdk-checksum-algorithm" || lk == "x-amz-trailer" {
			req.headers = append(req.headers, k)
		}
	}

	var reply strings.Builder
	reply.WriteString(`<?xml version="1.0" encoding="UTF-8"?><DeleteResult>`)
	for _, o := range in.Object {
		req.keys = append(req.keys, o.Key)
		if code, ok := b.refuse[o.Key]; ok {
			fmt.Fprintf(&reply, "<Error><Key>%s</Key><Code>%s</Code><Message>m</Message></Error>", o.Key, code)
			continue
		}
		fmt.Fprintf(&reply, "<Deleted><Key>%s</Key></Deleted>", o.Key)
	}
	reply.WriteString(`</DeleteResult>`)

	b.mu.Lock()
	b.got = append(b.got, req)
	b.mu.Unlock()

	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, reply.String())
}

func (b *batchServer) requests() []batchRequest {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]batchRequest(nil), b.got...)
}

// batched is the choice of a service with a batch delete.
var batched = choice{batch: true}

// names returns n distinct names.
func names(n int) []remote.Name {
	out := make([]remote.Name, n)
	for i := range out {
		out[i][0], out[i][1], out[i][2] = byte(i>>16), byte(i>>8), byte(i)
	}

	return out
}

// Each DeleteObjects the service reads carries a Content-MD5 of the body it
// received, no other checksum header, the unsigned payload and Quiet false.
// LocalStack checks none of this, so the service here is what consumes it.
func TestDeleteSendsContentMD5AndNoOtherChecksum(t *testing.T) {
	prof.Watch(t, prof.Options{})
	b := &batchServer{}
	s, _ := storeOn(t, tlsServer(t, b.ServeHTTP, false, nil), batched, nil)

	for i, err := range s.Delete(t.Context(), names(3)) {
		if err != nil {
			t.Errorf("Delete name %d: %v, want nil", i, err)
		}
	}

	got := b.requests()
	if len(got) != 1 {
		t.Fatalf("the service read %d DeleteObjects, want 1", len(got))
	}
	r := got[0]
	if !r.md5OK {
		t.Error("Content-MD5 is missing or is not the MD5 of the body the service received")
	}
	if len(r.headers) != 0 {
		t.Errorf("the request carries %v, want no checksum header but Content-MD5", r.headers)
	}
	if r.payload != "UNSIGNED-PAYLOAD" {
		t.Errorf("x-amz-content-sha256 %q, want UNSIGNED-PAYLOAD", r.payload)
	}
	if r.quiet {
		t.Error("the request asks for quiet mode, want Quiet false")
	}
}

// 2,500 names go in three requests of at most 1,000 keys, one after another,
// in the order given; each name's result is its key's.
func TestDeleteSplitsTheBatch(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ns := names(2500)
	b := &batchServer{}
	s, rec := storeOn(t, tlsServer(t, b.ServeHTTP, false, nil), batched, nil)
	b.refuse = map[string]string{s.blockKey(ns[1500]): "AccessDenied"}

	errs := s.Delete(t.Context(), ns)
	for i, err := range errs {
		switch {
		case i == 1500 && !errors.Is(err, remote.ErrDenied):
			t.Errorf("Delete name %d, refused: %v, want ErrDenied", i, err)
		case i != 1500 && err != nil:
			t.Errorf("Delete name %d: %v, want nil", i, err)
		}
	}

	got := b.requests()
	if len(got) != 3 {
		t.Fatalf("the service read %d DeleteObjects, want 3", len(got))
	}
	var sent []string
	for i, r := range got {
		if want := []int{1000, 1000, 500}[i]; len(r.keys) != want {
			t.Errorf("request %d names %d keys, want %d", i, len(r.keys), want)
		}
		sent = append(sent, r.keys...)
	}
	for i, k := range sent {
		if i < len(ns) && k != s.blockKey(ns[i]) {
			t.Fatalf("key %d sent is %s, want %s: the names went out of order", i, k, s.blockKey(ns[i]))
		}
	}
	if b.overlap.Load() {
		t.Error("two requests of one Delete were served at once, want one after another")
	}

	if calls := rec.all(); len(calls) != 1 || calls[0] != (recordedCall{"delete", "denied", "access", 0}) {
		t.Errorf("Delete recorded %+v, want one delete, denied of cause access", calls)
	}
}

// A name passed twice in one batch is sent once, and both its results are
// that key's.
func TestDeleteSendsARepeatedNameOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})
	b := &batchServer{}
	s, _ := storeOn(t, tlsServer(t, b.ServeHTTP, false, nil), batched, nil)
	n := names(2)

	for i, err := range s.Delete(t.Context(), []remote.Name{n[0], n[1], n[0]}) {
		if err != nil {
			t.Errorf("Delete name %d: %v, want nil", i, err)
		}
	}

	if got := b.requests(); len(got) != 1 || len(got[0].keys) != 2 {
		t.Fatalf("the service read %+v, want one request of 2 keys", got)
	}
}

// A refusal of the whole request fails every name of it with that refusal;
// a 404 NoSuchKey to the request states nothing of any key.
func TestDeleteAppliesABatchRefusalToEveryName(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, tc := range []struct {
		status int
		code   string
		want   error
	}{
		{403, "AccessDenied", remote.ErrDenied},
		{503, "SlowDown", remote.ErrThrottled},
		{404, "NoSuchKey", remote.ErrTransient},
	} {
		srv := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, xmlError(tc.code))
		}, false, nil)
		s, _ := storeOn(t, srv, batched, nil)

		for i, err := range s.Delete(t.Context(), names(3)) {
			if !errors.Is(err, tc.want) {
				t.Errorf("Delete name %d, batch answered %d %s: %v, want %v", i, tc.status, tc.code, err, tc.want)
			}
		}
	}
}

// On a service without batch delete, each name is one DeleteObject, in
// order, and an absent key answered 404 NoSuchKey is the name deleted.
func TestDeleteOneNamePerRequest(t *testing.T) {
	prof.Watch(t, prof.Options{})
	var (
		mu   sync.Mutex
		seen []string
	)
	ns := names(3)
	var absent string
	b := &batchServer{other: func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, absent) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, xmlError("NoSuchKey"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}}
	s, _ := storeOn(t, tlsServer(t, b.ServeHTTP, false, nil), choice{}, nil)
	absent = s.blockKey(ns[1])

	for i, err := range s.Delete(t.Context(), ns) {
		if err != nil {
			t.Errorf("Delete name %d: %v, want nil", i, err)
		}
	}

	if n := len(b.requests()); n != 0 {
		t.Errorf("the service read %d DeleteObjects, want none", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(ns) {
		t.Fatalf("the service saw %v, want %d requests", seen, len(ns))
	}
	for i, got := range seen {
		if want := "DELETE /b/" + s.blockKey(ns[i]); got != want {
			t.Errorf("request %d is %q, want %q", i, got, want)
		}
	}
	if b.overlap.Load() {
		t.Error("two requests of one Delete were served at once, want one after another")
	}
}

// A closed store and an ended context fail every name and send nothing.
func TestDeleteRefusesBeforeAnyRequest(t *testing.T) {
	prof.Watch(t, prof.Options{})
	srv, n := counting(t, md5ETag)

	for _, ch := range []choice{batched, {}} {
		closed, rec := storeOn(t, srv, ch, nil)
		_ = closed.Close()
		for i, err := range closed.Delete(t.Context(), names(2)) {
			if !errors.Is(err, remote.ErrInvalid) {
				t.Errorf("Delete name %d after Close, batch %v: %v, want ErrInvalid", i, ch.batch, err)
			}
		}
		if got := rec.all(); len(got) != 1 || got[0] != (recordedCall{"delete", "invalid", "", 0}) {
			t.Errorf("Delete after Close recorded %+v, want one delete, invalid", got)
		}

		s, rec := storeOn(t, srv, ch, nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		for i, err := range s.Delete(ctx, names(2)) {
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Delete name %d cancelled, batch %v: %v, want context.Canceled", i, ch.batch, err)
			}
		}
		if got := rec.all(); len(got) != 1 || got[0] != (recordedCall{"delete", "local", "", 0}) {
			t.Errorf("a cancelled Delete recorded %+v, want one delete, local", got)
		}
	}

	if got := n.Load(); got != 0 {
		t.Errorf("the service received %d requests, want 0", got)
	}
}
