package s3fault

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/cubbit/dittofs/pkg/sim"
)

// service is a real listener on 127.0.0.1:0 standing for the service: it
// fails the test on any request that reaches it, so a request the transport
// should have kept back is caught where it lands.
func service(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("%s %s reached the service, want it kept back", r.Method, r.URL)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// faultOf is a schedule firing action on every call.
func faultOf(t *testing.T, action sim.FaultAction) *sim.Schedule {
	t.Helper()
	fault, err := sim.NewSchedule(1, nil, sim.Rule{Op: "*", Key: "*", Action: action})
	if err != nil {
		t.Fatal(err)
	}
	return fault
}

// wantPanic fails the test unless f panics with an s3fault message holding
// each of want.
func wantPanic(t *testing.T, what string, f func(), want ...string) {
	t.Helper()
	defer func() {
		t.Helper()
		msg, _ := recover().(string)
		if !strings.HasPrefix(msg, "s3fault:") {
			t.Errorf("%s: recovered %q, want an s3fault panic", what, msg)
		}
		for _, w := range want {
			if !strings.Contains(msg, w) {
				t.Errorf("%s: panic %q, want it to name %q", what, msg, w)
			}
		}
	}()
	f()
	t.Errorf("%s returned, want a panic", what)
}

// Each tear or loss the transport cannot honour without reporting a success
// the service never gave panics, naming the op, before anything is sent.
func TestRoundTripRefusesATearOrLossItCannotHonour(t *testing.T) {
	cut := errors.New("cut")
	for _, c := range []struct {
		name, op string
		action   sim.FaultAction
	}{
		{"a tear of a get", "GetObject", sim.FaultAction{TearAfter: 2, Err: cut}},
		{"a tear with no error", "PutObject", sim.FaultAction{TearAfter: 2}},
		{"a tear at the body's end", "PutObject", sim.FaultAction{TearAfter: 4, Err: cut}},
		{"a tear past the body's end", "PutObject", sim.FaultAction{TearAfter: 5, Err: cut}},
		{"a loss", "PutObject", sim.FaultAction{TearAfter: sim.Lose}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := New(http.DefaultTransport, faultOf(t, c.action))
			req, err := http.NewRequestWithContext(awsmiddleware.SetOperationName(t.Context(), c.op),
				http.MethodPut, service(t)+"/bucket/key", strings.NewReader("body"))
			if err != nil {
				t.Fatal(err)
			}
			wantPanic(t, c.name+" of "+c.op, func() { _, _ = tr.RoundTrip(req) }, c.op)
		})
	}
}

// A torn put sends the bytes before the cut and no more, then fails with the
// fault's error, so the service reads a body cut short.
func TestATornPutSendsOnlyTheBytesBeforeTheCut(t *testing.T) {
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- b
	}))
	t.Cleanup(srv.Close)
	errCut := errors.New("cut")
	body := bytes.Repeat([]byte("0123456789"), 10)
	tr := New(http.DefaultTransport, faultOf(t, sim.FaultAction{TearAfter: 10, Err: errCut}))
	_, err := rawClient(srv.URL, tr).PutObject(t.Context(), &s3.PutObjectInput{Bucket: aws.String("bucket"), Key: aws.String("key"), Body: bytes.NewReader(body)})
	if !errors.Is(err, errCut) {
		t.Errorf("PutObject torn after 10 bytes: %v, want the fault's error", err)
	}
	select {
	case b := <-got:
		if !bytes.Equal(b, body[:10]) {
			t.Errorf("the service read %q of the torn put, want its first 10 bytes", b)
		}
	case <-time.After(30 * time.Second): // a backstop: the handler has run once PutObject returned
		t.Fatal("the torn put never reached the service")
	}
}

// A reply body shorter than TruncateReply's cut reads whole and ends with
// io.EOF, rather than reading nothing forever once the body has ended.
func TestATruncatedReplyShorterThanTheCutEnds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("short"))
	}))
	t.Cleanup(srv.Close)
	tr := New(http.DefaultTransport, nil)
	tr.Inject(Rule{Op: "GetObject"}, TruncateReply(10))
	out, err := rawClient(srv.URL, tr).GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("bucket"), Key: aws.String("key")})
	if err != nil {
		t.Fatalf("GetObject under TruncateReply(10): %v", err)
	}
	t.Cleanup(func() { _ = out.Body.Close() })
	type read struct {
		b   []byte
		err error
	}
	done := make(chan read, 1)
	go func() {
		b, err := io.ReadAll(out.Body)
		done <- read{b, err}
	}()
	select {
	case r := <-done:
		if string(r.b) != "short" || r.err != nil {
			t.Errorf("reading a 5-byte body under TruncateReply(10): %q, %v, want \"short\" ending in io.EOF", r.b, r.err)
		}
	case <-time.After(30 * time.Second): // a backstop: the read ends once the 5-byte body has
		t.Fatal("reading a 5-byte body under TruncateReply(10) never ended")
	}
}

// Thirty-two concurrent puts under one Answer of Times 1: exactly one is
// answered, and the other 31 reach the service.
func TestConcurrentRequestsShareOneAnswer(t *testing.T) {
	var reached atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	t.Cleanup(srv.Close)
	tr := New(http.DefaultTransport, nil)
	tr.Inject(Rule{Times: 1}, Answer(http.StatusServiceUnavailable, "SlowDown", nil))
	client := rawClient(srv.URL, tr)
	const n = 32
	var answered atomic.Int32
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, err := client.PutObject(t.Context(), &s3.PutObjectInput{Bucket: aws.String("bucket"), Key: aws.String(fmt.Sprint(i)), Body: strings.NewReader("c")})
			if code(err) == "SlowDown" {
				answered.Add(1)
			} else if err != nil {
				t.Errorf("put %d: %v, want it answered SlowDown or passed", i, err)
			}
		})
	}
	wg.Wait()
	if answered.Load() != 1 || reached.Load() != n-1 {
		t.Errorf("%d puts answered and %d reached the service, want 1 and %d", answered.Load(), reached.Load(), n-1)
	}
	if got := len(tr.Requests()); got != n {
		t.Errorf("the record holds %d requests, want %d", got, n)
	}
}

// An effect after one that answers the request neither acts on it nor
// counts it, so its one request is still the next one it selects.
func TestAnAnsweredRequestSpendsNoLaterEffect(t *testing.T) {
	tr := New(http.DefaultTransport, nil)
	removeFirst := tr.Inject(Rule{Times: 1}, Answer(http.StatusServiceUnavailable, "SlowDown", nil))
	tr.Inject(Rule{Times: 1}, Answer(http.StatusNotFound, "NoSuchKey", nil))
	client := rawClient(service(t), tr)
	get := func() string {
		_, err := client.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("bucket"), Key: aws.String("key")})
		return code(err)
	}
	if got := get(); got != "SlowDown" {
		t.Errorf("first get answered %q, want SlowDown", got)
	}
	removeFirst()
	if got := get(); got != "NoSuchKey" {
		t.Errorf("second get answered %q, want NoSuchKey: the answered first get spent it", got)
	}
}

// A HEAD answered with an error carries no body, as the service sends it, so
// the SDK reads the status alone.
func TestAnAnsweredHeadCarriesNoBody(t *testing.T) {
	tr := New(http.DefaultTransport, nil)
	tr.Inject(Rule{}, Answer(http.StatusNotFound, "NoSuchKey", nil))
	_, err := rawClient(service(t), tr).HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String("bucket"), Key: aws.String("key")})
	if got := code(err); got != "NotFound" {
		t.Errorf("HeadObject under Answer(404, NoSuchKey): code %q (%v), want NotFound, the SDK's reading of a bare 404", got, err)
	}
}

// code is the S3 error code err carries, or "" for none.
func code(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

// An Answer reaches the SDK as the status and code given, or, for a body that
// does not parse, as that status with a body the SDK cannot read, and the
// request never reaches the service.
func TestAnswerReachesTheSDKAsTheStatusAndCodeGiven(t *testing.T) {
	for _, status := range []int{100, 404, 599} {
		for _, c := range []struct {
			code string
			body []byte
		}{
			{"NoSuchKey", nil},
			{"S3FaultUnknownCode", nil},
			{"", []byte("<html>not an error document")},
		} {
			t.Run(fmt.Sprintf("%d %s", status, c.code), func(t *testing.T) {
				tr := New(&http.Transport{}, nil)
				tr.Inject(Rule{}, Answer(status, c.code, c.body))
				_, err := rawClient(service(t), tr).GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("bucket"), Key: aws.String("key")})

				var re *awshttp.ResponseError
				if !errors.As(err, &re) || re.HTTPStatusCode() != status {
					t.Fatalf("GetObject under Answer(%d, %q): %v, want a response error of status %d", status, c.code, err, status)
				}
				var api smithy.APIError
				var syntax *xml.SyntaxError
				switch {
				case c.body == nil && (!errors.As(err, &api) || api.ErrorCode() != c.code):
					t.Errorf("GetObject under Answer(%d, %q): %v, want the error code %q", status, c.code, err, c.code)
				case c.body != nil && (errors.As(err, &api) || !errors.As(err, &syntax)):
					t.Errorf("GetObject under Answer(%d) with an unparseable body: %v, want an XML syntax error and no error code", status, err)
				}
			})
		}
	}
}

// Answer refuses a success, which only the service may answer, and a status
// outside 100-599.
func TestAnswerRefusesASuccessOrAStatusOutOfRange(t *testing.T) {
	for _, status := range []int{200, 204, 299, 99, 600} {
		wantPanic(t, fmt.Sprintf("Answer(%d)", status), func() { Answer(status, "", nil) }, fmt.Sprint(status))
	}
}

// keyOf takes the bucket off a path-style path, leaving the key whole.
func TestKeyOf(t *testing.T) {
	for path, want := range map[string]string{"/b": "", "/b/": "", "/b/k": "k", "/b/a/b/c": "a/b/c"} {
		if got := keyOf(path); got != want {
			t.Errorf("keyOf(%q) = %q, want %q", path, got, want)
		}
	}
}

// rawClient is an aws-sdk-go-v2 client set up as Appendix C.3 says, through
// rt: retries off, WhenRequired checksums, UNSIGNED-PAYLOAD, path-style.
func rawClient(endpoint string, rt http.RoundTripper) *s3.Client {
	return s3.New(s3.Options{
		Region:                     "us-east-1",
		BaseEndpoint:               aws.String(endpoint),
		UsePathStyle:               true,
		Credentials:                credentials.NewStaticCredentialsProvider("test", "test", ""),
		Retryer:                    aws.NopRetryer{},
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		HTTPClient:                 &http.Client{Transport: rt},
	}, s3.WithAPIOptions(v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware))
}
