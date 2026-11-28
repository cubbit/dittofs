package s3fault

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/cubbit/dittofs/pkg/sim"
)

// unreachable is an endpoint nothing listens on: a request that reached the
// wrapped transport would fail to dial rather than be answered.
const unreachable = "https://127.0.0.1:1"

// Each tear or loss the transport cannot honour without reporting a success
// the service never gave panics, naming the op, before anything is sent.
func TestRoundTripRefusesATearOrLossItCannotHonour(t *testing.T) {
	cut := errors.New("cut")
	for _, c := range []struct {
		name, op string
		action   sim.FaultAction
	}{
		{"a tear of a get", "GetObject", sim.FaultAction{TearAfter: 10, Err: cut}},
		{"a tear with no error", "PutObject", sim.FaultAction{TearAfter: 10}},
		{"a loss", "PutObject", sim.FaultAction{TearAfter: sim.Lose}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fault, err := sim.NewSchedule(1, nil, sim.Rule{Op: "*", Key: "*", Action: c.action})
			if err != nil {
				t.Fatal(err)
			}
			tr := New(&http.Transport{}, fault)
			req, err := http.NewRequestWithContext(awsmiddleware.SetOperationName(t.Context(), c.op),
				http.MethodPut, unreachable+"/bucket/key", strings.NewReader("body"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				r := recover()
				if msg, _ := r.(string); !strings.Contains(msg, "s3fault:") || !strings.Contains(msg, c.op) {
					t.Errorf("%s of %s: recovered %v, want a panic naming the op", c.name, c.op, r)
				}
			}()
			resp, err := tr.RoundTrip(req)
			t.Errorf("%s of %s returned (%v, %v), want a panic", c.name, c.op, resp, err)
		})
	}
}

// An Answer reaches the SDK as the status and code given, or, for a body that
// does not parse, as that status with a body the SDK cannot read, and the
// request never reaches the wrapped transport, which would fail to dial.
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
				_, err := rawClient(unreachable, tr).GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("bucket"), Key: aws.String("key")})

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

// Answer refuses a success, which only the service may answer.
func TestAnswerOfASuccessPanics(t *testing.T) {
	for _, status := range []int{200, 204, 299} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("Answer(%d) returned, want a panic", status)
				}
			}()
			Answer(status, "", nil)
		}()
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
