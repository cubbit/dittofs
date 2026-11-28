//go:build integration

package s3fault

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/sim"
)

// rig is a fresh bucket on LocalStack, a client that reaches it through a
// fault transport and an admin client that bypasses it, to read back what
// the service holds.
type rig struct {
	tr       *Transport
	endpoint string
	client   *s3.Client
	admin    *s3.Client
	bucket   string
}

// newRig watches the test for leaked goroutines and descriptors, then builds
// a rig whose transport consults fault. The bucket and every object in it are
// deleted at cleanup.
func newRig(t *testing.T, fault Fault) *rig {
	t.Helper()
	prof.Watch(t, prof.Options{})
	endpoint, roots := LocalStack(t)
	https := func() *http.Transport {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots},
			// HTTP/1.1 only, as the store's client is.
			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		}
		t.Cleanup(tr.CloseIdleConnections)
		return tr
	}
	r := &rig{tr: New(https(), fault), endpoint: endpoint, admin: rawClient(endpoint, https())}
	r.client = rawClient(endpoint, r.tr)

	var name [6]byte
	_, _ = rand.Read(name[:])
	r.bucket = "s3fault-" + hex.EncodeToString(name[:])
	if _, err := r.admin.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: &r.bucket}); err != nil {
		t.Fatalf("CreateBucket %s: %v", r.bucket, err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		list, err := r.admin.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &r.bucket})
		if err != nil {
			t.Errorf("listing %s to delete it: %v", r.bucket, err)
			return
		}
		for _, o := range list.Contents {
			if _, err := r.admin.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &r.bucket, Key: o.Key}); err != nil {
				t.Errorf("DeleteObject %s: %v", *o.Key, err)
			}
		}
		if _, err := r.admin.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &r.bucket}); err != nil {
			t.Errorf("DeleteBucket %s: %v", r.bucket, err)
		}
	})
	return r
}

func (r *rig) put(ctx context.Context, key string, body []byte) error {
	_, err := r.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &r.bucket, Key: &key, Body: bytes.NewReader(body)})
	return err
}

// stored is what the service holds at key, read through the admin client;
// ok is false when it holds nothing there.
func (r *rig) stored(t *testing.T, key string) (body []byte, ok bool) {
	t.Helper()
	out, err := r.admin.GetObject(t.Context(), &s3.GetObjectInput{Bucket: &r.bucket, Key: &key})
	var missing *types.NoSuchKey
	if errors.As(err, &missing) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("admin GetObject %s: %v", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	body, err = io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("admin reading %s: %v", key, err)
	}
	return body, true
}

// Each fault acts on its own call and on the side of it it names: a dropped
// or answered put stores nothing, a cut put stores nothing, a lost reply's
// put is stored although the client got an error, and a truncated get ends
// early under the service's full Content-Range. The next put of each key,
// which no fault selects, is stored, so no fault acts on the call after it.
func TestEachFaultActsOnItsOwnCall(t *testing.T) {
	errDrop, errCut := errors.New("dropped"), errors.New("cut")
	fault, err := sim.NewSchedule(1, nil,
		sim.Rule{Op: "PutObject", Key: "dropped", Count: 1, Action: sim.FaultAction{Err: errDrop}},
		sim.Rule{Op: "PutObject", Key: "cut", Count: 1, Action: sim.FaultAction{TearAfter: 10, Err: errCut}},
	)
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, fault)
	r.tr.Inject(Rule{Op: "PutObject", KeyPrefix: "lost", Times: 1}, LoseReply())
	r.tr.Inject(Rule{Op: "PutObject", KeyPrefix: "slow", Times: 1}, Answer(http.StatusServiceUnavailable, "SlowDown", nil))
	body := bytes.Repeat([]byte("0123456789"), 10)

	for _, c := range []struct {
		key       string
		wantErr   func(error) bool
		wantStore bool
	}{
		{"dropped", func(err error) bool { return errors.Is(err, errDrop) }, false},
		{"cut", func(err error) bool { return errors.Is(err, errCut) }, false},
		{"lost", func(err error) bool { return errors.Is(err, errReplyLost) }, true},
		{"slow", func(err error) bool {
			var api smithy.APIError
			var re *awshttp.ResponseError
			return errors.As(err, &api) && api.ErrorCode() == "SlowDown" && errors.As(err, &re) && re.HTTPStatusCode() == 503
		}, false},
	} {
		if err := r.put(t.Context(), c.key, body); !c.wantErr(err) {
			t.Errorf("put %s under its fault: %v, want the fault's error", c.key, err)
		}
		if got, ok := r.stored(t, c.key); ok != c.wantStore || (ok && !bytes.Equal(got, body)) {
			t.Errorf("after the faulted put %s the service holds %d bytes (stored %v), want stored %v with the body sent", c.key, len(got), ok, c.wantStore)
		}
		next := []byte("next put of " + c.key)
		if err := r.put(t.Context(), c.key, next); err != nil {
			t.Errorf("the put of %s after its faulted one: %v, want it to pass", c.key, err)
		}
		if got, _ := r.stored(t, c.key); !bytes.Equal(got, next) {
			t.Errorf("after the second put of %s the service holds %q, want %q", c.key, got, next)
		}
	}

	key := "truncated"
	if _, err := r.admin.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &r.bucket, Key: &key, Body: bytes.NewReader(body)}); err != nil {
		t.Fatal(err)
	}
	r.tr.Inject(Rule{Op: "GetObject", KeyPrefix: key, Times: 1}, TruncateReply(10))
	out, err := r.client.GetObject(t.Context(), &s3.GetObjectInput{Bucket: &r.bucket, Key: &key, Range: aws.String("bytes=0-99")})
	if err != nil {
		t.Fatalf("GetObject %s under TruncateReply(10): %v", key, err)
	}
	got, err := io.ReadAll(out.Body)
	_ = out.Body.Close()
	if err != nil || !bytes.Equal(got, body[:10]) || aws.ToString(out.ContentRange) != "bytes 0-99/100" {
		t.Errorf("GetObject %s under TruncateReply(10): %q, %v under Content-Range %q, want its first 10 bytes and io.EOF under bytes 0-99/100",
			key, got, err, aws.ToString(out.ContentRange))
	}
}

// A delayed request waits until the test moves the fault's clock, and one
// whose context is cancelled while it waits returns context.Canceled at once,
// with the clock where it was and nothing sent.
func TestADelayedRequestWaitsOnTheFaultsClock(t *testing.T) {
	clock := sim.NewTestClock(time.Unix(0, 0))
	fault, err := sim.NewSchedule(1, clock, sim.Rule{Op: "PutObject", Key: "delayed-*", Action: sim.FaultAction{Delay: time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, fault)
	// backstop bounds a wait on an event that should already have come.
	backstop := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s did not happen", what)
		}
	}
	done := make(chan error, 1)
	returned := func(what string) error {
		t.Helper()
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			t.Fatalf("%s did not return", what)
			return nil
		}
	}

	armed := clock.Armed(time.Minute)
	go func() { done <- r.put(t.Context(), "delayed-sent", []byte("sent")) }()
	backstop(armed, "the delayed put's wait")
	if _, ok := r.stored(t, "delayed-sent"); ok {
		t.Error("delayed-sent is stored before the clock moved, want the put held")
	}
	clock.Advance(time.Minute)
	if err := returned("the delayed put"); err != nil {
		t.Fatalf("the delayed put after the clock moved: %v", err)
	}
	if got, _ := r.stored(t, "delayed-sent"); string(got) != "sent" {
		t.Errorf("after the delay the service holds %q, want sent", got)
	}

	ctx, cancel := context.WithCancel(t.Context())
	armed = clock.Armed(time.Minute)
	go func() { done <- r.put(ctx, "delayed-cancelled", []byte("cancelled")) }()
	backstop(armed, "the cancelled put's wait")
	cancel()
	if err := returned("the cancelled put"); !errors.Is(err, context.Canceled) {
		t.Errorf("the put cancelled while delayed: %v, want context.Canceled", err)
	}
	if got := clock.Elapsed(); got != time.Minute {
		t.Errorf("the clock read %v when the cancelled put returned, want 1m0s: it returned without the clock", got)
	}
	// Releases the abandoned wait, so the leak check finds its goroutine gone.
	clock.Advance(time.Minute)
	if _, ok := r.stored(t, "delayed-cancelled"); ok {
		t.Error("delayed-cancelled is stored, want the cancelled put never sent")
	}
}

// Every operation the client sends reaches the record under its SDK name and
// its key, with the headers the client built and the protocol of the
// service's reply; an answered request records no reply. Each operation
// passes through unchanged: the get returns what the put stored.
func TestTheRecordNamesEachOperation(t *testing.T) {
	r := newRig(t, nil)
	if err := r.put(t.Context(), "op/a", []byte("a")); err != nil {
		t.Fatal(err)
	}
	out, err := r.client.GetObject(t.Context(), &s3.GetObjectInput{Bucket: &r.bucket, Key: aws.String("op/a")})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(out.Body)
	_ = out.Body.Close()
	if string(got) != "a" {
		t.Errorf("GetObject op/a through the transport: %q, want a", got)
	}
	if _, err := r.client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: &r.bucket}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.DeleteObjects(t.Context(), &s3.DeleteObjectsInput{Bucket: &r.bucket,
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: aws.String("op/a")}}}}); err != nil {
		t.Fatal(err)
	}
	r.tr.Inject(Rule{Times: 1}, Answer(http.StatusNotFound, "NoSuchKey", nil))
	_, _ = r.client.GetObject(t.Context(), &s3.GetObjectInput{Bucket: &r.bucket, Key: aws.String("op/answered")})

	want := []Request{
		{Op: "PutObject", Key: "op/a", ProtoMajor: 1},
		{Op: "GetObject", Key: "op/a", ProtoMajor: 1},
		{Op: "ListObjectsV2", ProtoMajor: 1},
		{Op: "DeleteObjects", ProtoMajor: 1},
		{Op: "GetObject", Key: "op/answered"},
	}
	reqs := r.tr.Requests()
	if len(reqs) != len(want) {
		t.Fatalf("the record holds %d requests, want %d: %+v", len(reqs), len(want), reqs)
	}
	for i, w := range want {
		g := reqs[i]
		if g.Op != w.Op || g.Key != w.Key || g.ProtoMajor != w.ProtoMajor {
			t.Errorf("request %d recorded as (%s, %q, HTTP/%d), want (%s, %q, HTTP/%d)", i, g.Op, g.Key, g.ProtoMajor, w.Op, w.Key, w.ProtoMajor)
		}
		if sha := g.Header.Get("X-Amz-Content-Sha256"); sha != "UNSIGNED-PAYLOAD" {
			t.Errorf("request %d (%s) recorded x-amz-content-sha256 %q, want UNSIGNED-PAYLOAD", i, g.Op, sha)
		}
	}
}

// Thirty-two concurrent puts under one Answer of Times 1: exactly one is
// refused, and the service holds the other 31.
func TestConcurrentPutsShareOneAnswer(t *testing.T) {
	r := newRig(t, nil)
	r.tr.Inject(Rule{Times: 1}, Answer(http.StatusServiceUnavailable, "SlowDown", nil))
	const n = 32
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[i] = r.put(t.Context(), fmt.Sprintf("c/%02d", i), []byte("c")) })
	}
	wg.Wait()
	refused := slices.IndexFunc(errs, func(err error) bool { return err != nil })
	if refused < 0 || slices.ContainsFunc(errs[refused+1:], func(err error) bool { return err != nil }) {
		t.Errorf("puts refused: %v, want exactly one", errs)
	}
	list, err := r.admin.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: &r.bucket, Prefix: aws.String("c/")})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Contents) != n-1 {
		t.Errorf("the service holds %d objects after the puts, want %d", len(list.Contents), n-1)
	}
}

// The transport allocates nothing per byte of a put: what it adds to a put's
// allocation is the same at 16 MiB as at 1 MiB, so it buffers no body.
func TestTheTransportAllocatesNothingPerByte(t *testing.T) {
	r := newRig(t, nil)
	direct := rawClient(r.endpoint, r.tr.next)
	// allocated is the least the process allocates over one put, over a few
	// runs, so a collection or another goroutine's work in one run is ignored.
	allocated := func(c *s3.Client, body []byte) uint64 {
		least := ^uint64(0)
		for range 5 {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			if _, err := c.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &r.bucket, Key: aws.String("alloc"), Body: bytes.NewReader(body)}); err != nil {
				t.Fatal(err)
			}
			runtime.ReadMemStats(&after)
			least = min(least, after.TotalAlloc-before.TotalAlloc)
		}
		return least
	}
	added := func(size int) int64 {
		body := make([]byte, size)
		return int64(allocated(r.client, body)) - int64(allocated(direct, body))
	}
	small, large := added(1<<20), added(16<<20)
	t.Logf("the transport adds %d bytes to a 1 MiB put and %d to a 16 MiB one", small, large)
	if growth := large - small; growth > 1<<20 {
		t.Errorf("the transport adds %d bytes to a 1 MiB put and %d to a 16 MiB one: it grows with the body", small, large)
	}
}
