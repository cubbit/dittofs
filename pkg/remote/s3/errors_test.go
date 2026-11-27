package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/cubbit/dittofs/pkg/remote"
)

var allOps = []opKind{opPut, opPutControl, opGet, opGetControl, opDelete, opList, opDeleteKey}

// sentinels are the closed set, ErrThrottled apart from the ErrTransient it
// also matches.
var sentinels = []error{remote.ErrNotFound, remote.ErrInvalid, remote.ErrDenied, remote.ErrTransient, remote.ErrCorrupt}

// oneSentinel says which sentinel err wraps, or why it does not wrap exactly
// one: ErrThrottled is the one sentinel it is, though it also matches
// ErrTransient, and ErrMalformed is never a store's.
func oneSentinel(err error) (why string, got error) {
	if errors.Is(err, remote.ErrMalformed) {
		return "matches ErrMalformed", nil
	}

	var hit []error
	for _, s := range sentinels {
		if errors.Is(err, s) {
			hit = append(hit, s)
		}
	}

	if len(hit) != 1 {
		return fmt.Sprintf("matches %d sentinels", len(hit)), nil
	}

	if errors.Is(err, remote.ErrThrottled) {
		return "", remote.ErrThrottled
	}

	if errors.Is(err, remote.ErrDenied) {
		var d *remote.DeniedError
		if !errors.As(err, &d) {
			return "an ErrDenied with no DeniedError", nil
		}

		if d.Cause != remote.CauseAccess && d.Cause != remote.CauseQuota && d.Cause != remote.CauseSize {
			return "an ErrDenied of cause " + d.Cause.String(), nil
		}
	}

	return "", hit[0]
}

// The table is total: every call, every status from 100 to 599, a key's
// status 0 and statuses outside the range, with every code the table names
// and one it does not, maps to exactly one sentinel, a denial carrying its
// cause and the reply's code. Only an absent key to a delete is nil.
func TestClassifyIsTotal(t *testing.T) {
	// Every code C.3 names, none and one it does not; a key's status 0 is
	// read through every code of the reference as well.
	codes := []string{"", "NotACodeAnyServiceSends", "XMinioAdminBucketQuotaExceeded", "NoSuchKey", "NoSuchVersion",
		"NoSuchBucket", "InvalidToken", "EntityTooLarge", "SlowDown", "BadDigest", "InvalidDigest",
		"XAmzContentSHA256Mismatch", "ExpiredToken", "RequestTimeout", "OperationAborted", "InvalidRequest"}
	keyCodes := slices.AppendSeq(slices.Clone(codes), maps.Keys(codeStatus))

	statuses := []int{0, -1, 99, 600, 999}
	for s := 100; s <= 599; s++ {
		statuses = append(statuses, s)
	}

	recs := []recorded{{}, {400, "InvalidRequest"}}
	for _, op := range allOps {
		t.Run(op.String(), func(t *testing.T) { totalOver(t, op, statuses, codes, keyCodes, recs) })
	}
}

// totalOver checks every input of op, stopping at its first wrong answer.
func totalOver(t *testing.T, op opKind, statuses []int, codes, keyCodes []string, recs []recorded) {
	t.Helper()

	{
		for _, status := range statuses {
			cs := codes
			if status == 0 {
				cs = keyCodes
			}

			for _, code := range cs {
				for _, rec := range recs {
					err := classify(op, status, code, rec)
					if err == nil {
						absent := code == "NoSuchKey" && (status == 404 || status == 0) && (op == opDelete || op == opDeleteKey)
						if !absent {
							t.Fatalf("classify(%v, %d, %q, %v) = nil, want an error", op, status, code, rec)
						}

						continue
					}

					if why, _ := oneSentinel(err); why != "" {
						t.Fatalf("classify(%v, %d, %q, %v) = %v: %s, want exactly one sentinel", op, status, code, rec, err, why)
					}

					var d *remote.DeniedError
					if errors.As(err, &d) && d.Code != code {
						t.Fatalf("classify(%v, %d, %q, %v): DeniedError.Code = %q, want the reply's code", op, status, code, rec, d.Code)
					}
				}
			}
		}
	}
}

// Each row of RFC 4 Appendix C.3, at the call it names.
func TestClassifyRows(t *testing.T) {
	throttled, notFound, invalid, transient, corrupt := remote.ErrThrottled, remote.ErrNotFound, remote.ErrInvalid, remote.ErrTransient, remote.ErrCorrupt
	access, quota, size := remote.CauseAccess, remote.CauseQuota, remote.CauseSize

	for _, tc := range []struct {
		op     opKind
		status int
		code   string
		want   error        // the sentinel, nil for a denial or for success
		cause  remote.Cause // the denial's cause; 0 when want decides
	}{
		// not found
		{opGet, 404, "NoSuchKey", notFound, 0},
		{opGet, 404, "NoSuchVersion", notFound, 0},
		{opGetControl, 404, "NoSuchVersion", notFound, 0},
		{opList, 404, "NoSuchVersion", invalid, 0},
		{opDelete, 404, "NoSuchKey", nil, 0},
		{opDeleteKey, 0, "NoSuchKey", nil, 0},
		// access: a missing bucket is the store's failure, not a block's
		{opGet, 404, "NoSuchBucket", nil, access},
		{opGet, 401, "", nil, access},
		{opPut, 403, "AccessDenied", nil, access},
		{opList, 400, "InvalidToken", nil, access},
		{opGet, 301, "PermanentRedirect", nil, access},
		{opGet, 307, "", nil, access},
		{opDeleteKey, 0, "AccessDenied", nil, access},
		// quota first, with any status and any code
		{opPut, 507, "", nil, quota},
		{opGet, 507, "InternalError", nil, quota},
		{opPut, 400, "XMinioAdminBucketQuotaExceeded", nil, quota},
		{opPut, 413, "XMinioAdminBucketQuotaExceeded", nil, quota},
		{opPutControl, 403, "XMinioAdminBucketQuotaExceeded", nil, quota},
		{opDeleteKey, 0, "XMinioAdminBucketQuotaExceeded", nil, quota},
		// size next, before the class rows, on a block put only
		{opPut, 413, "", nil, size},
		{opPut, 413, "AccessDenied", nil, size},
		{opPut, 400, "EntityTooLarge", nil, size},
		{opPut, 403, "EntityTooLarge", nil, size},
		{opPutControl, 413, "", invalid, 0},
		{opPutControl, 403, "EntityTooLarge", invalid, 0},
		{opGet, 413, "", invalid, 0},
		// throttled
		{opGet, 429, "", throttled, 0},
		{opPut, 503, "SlowDown", throttled, 0},
		{opDeleteKey, 0, "SlowDown", throttled, 0},
		// corrupt
		{opPut, 400, "BadDigest", corrupt, 0},
		{opPut, 400, "InvalidDigest", corrupt, 0},
		{opPut, 400, "XAmzContentSHA256Mismatch", corrupt, 0},
		// transient
		{opPut, 400, "ExpiredToken", transient, 0},
		{opPut, 400, "RequestTimeout", transient, 0},
		{opGet, 408, "", transient, 0},
		{opDelete, 409, "OperationAborted", transient, 0},
		{opGet, 500, "InternalError", transient, 0},
		{opGet, 502, "", transient, 0},
		{opGet, 503, "", transient, 0},
		{opGet, 503, "ServiceUnavailable", transient, 0},
		{opGet, 504, "", transient, 0},
		{opGet, 599, "", transient, 0},
		{opDeleteKey, 0, "InternalError", transient, 0},
		{opDeleteKey, 0, "NotACodeAnyServiceSends", transient, 0},
		{opGet, 100, "", transient, 0},
		{opGet, 200, "", transient, 0},
		{opGet, 600, "", transient, 0},
		{opGet, -1, "", transient, 0},
		// invalid
		{opGet, 416, "InvalidRange", invalid, 0},
		{opGet, 501, "NotImplemented", invalid, 0},
		{opPut, 400, "InvalidRequest", invalid, 0},
		{opPut, 400, "", invalid, 0},
		{opGet, 404, "", invalid, 0},
		{opGet, 404, "NoSuchUpload", invalid, 0},
		{opPut, 409, "", invalid, 0},
		{opDeleteKey, 0, "MalformedXML", invalid, 0},
	} {
		err := classify(tc.op, tc.status, tc.code, recorded{})
		checkErr(t, "classify("+tc.op.String()+", "+strconv.Itoa(tc.status)+", "+tc.code+")", err, tc.want, tc.cause)
	}
}

// The check's recorded rejection is ErrCorrupt only on a put that carried the
// chosen checksum, which is the put that passes it.
func TestClassifyRecordedRejection(t *testing.T) {
	rec := recorded{400, "InvalidRequest"}
	for _, tc := range []struct {
		op   opKind
		rec  recorded
		want error
	}{
		{opPut, rec, remote.ErrCorrupt},
		{opPutControl, rec, remote.ErrCorrupt},
		{opPut, recorded{}, remote.ErrInvalid},
		{opGet, rec, remote.ErrInvalid},
		{opList, rec, remote.ErrInvalid},
	} {
		if _, got := oneSentinel(classify(tc.op, 400, "InvalidRequest", tc.rec)); got != tc.want {
			t.Errorf("classify(%v, 400, InvalidRequest, %v) is %v, want %v", tc.op, tc.rec, got, tc.want)
		}
	}

	// The status and the code must both match: the same code at another status is the class row.
	if _, got := oneSentinel(classify(opPut, 403, "InvalidRequest", rec)); got != remote.ErrDenied {
		t.Errorf("classify(put, 403, InvalidRequest, %v) is %v, want ErrDenied", rec, got)
	}
}

// answer is a representative service answer, modelled on S3's error body
// and an nginx proxy's page: its status, its content type and its body.
type answer struct {
	status int
	ctype  string
	body   string
}

func xmlError(code string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><Error><Code>` + code + `</Code><Message>m</Message><RequestId>r</RequestId></Error>`
}

const proxyHTML413 = "<html>\r\n<head><title>413 Request Entity Too Large</title></head>\r\n<body>\r\n<center><h1>413 Request Entity Too Large</h1></center>\r\n<hr><center>nginx</center>\r\n</body>\r\n</html>\r\n"

const proxyHTML403 = "<html>\r\n<head><title>403 Forbidden</title></head>\r\n<body>\r\n<center><h1>403 Forbidden</h1></center>\r\n<hr><center>nginx</center>\r\n</body>\r\n</html>\r\n"

// serve answers every request with r from a listener on 127.0.0.1:0.
func serve(t *testing.T, r answer) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if r.ctype != "" {
			w.Header().Set("Content-Type", r.ctype)
		}
		w.WriteHeader(r.status)
		_, _ = io.WriteString(w, r.body)
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

// sdkClient is the SDK's client over endpoint, one attempt per call, over hc
// when it is not nil.
func sdkClient(endpoint string, hc *http.Client) *s3.Client {
	o := s3.Options{
		Region:                     "us-east-1",
		BaseEndpoint:               aws.String(endpoint),
		UsePathStyle:               true,
		Credentials:                aws.AnonymousCredentials{},
		RetryMaxAttempts:           1,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if hc != nil {
		o.HTTPClient = hc
	}

	return s3.New(o)
}

// call runs op through the SDK against c and returns the SDK's error.
func call(ctx context.Context, c *s3.Client, op opKind) error {
	b, k := aws.String("b"), aws.String("blocks/00")
	switch op {
	case opPut, opPutControl:
		_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: b, Key: k, Body: strings.NewReader("x"), ContentLength: aws.Int64(1)})
		return err
	case opGet, opGetControl:
		out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: b, Key: k})
		if err == nil {
			_, err = io.Copy(io.Discard, out.Body)
			_ = out.Body.Close()
		}

		return err
	case opDelete:
		_, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: b, Key: k})
		return err
	case opList:
		_, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: b, Prefix: aws.String("blocks/")})
		return err
	}

	panic("no SDK call for " + op.String())
}

// fromSDK over the errors the SDK's own deserializer builds from representative
// replies: the class, the cause and the code survive, and no SDK or network
// type is reachable by unwrapping.
func TestFromSDKOverRecordedReplies(t *testing.T) {
	for _, tc := range []struct {
		name  string
		op    opKind
		reply answer
		want  error
		cause remote.Cause
		code  string // the DeniedError's code
	}{
		{"an XML AccessDenied", opPut, answer{403, "application/xml", xmlError("AccessDenied")}, nil, remote.CauseAccess, "AccessDenied"},
		{"a proxy's HTML 413 to a block put", opPut, answer{413, "text/html", proxyHTML413}, nil, remote.CauseSize, ""},
		{"a proxy's HTML 413 to a control put", opPutControl, answer{413, "text/html", proxyHTML413}, remote.ErrInvalid, 0, ""},
		{"a proxy's HTML 403", opGet, answer{403, "text/html", proxyHTML403}, nil, remote.CauseAccess, ""},
		{"a 403 with no body", opList, answer{403, "", ""}, nil, remote.CauseAccess, ""},
		{"an XML body with no code", opPut, answer{403, "application/xml", `<Error><Message>m</Message></Error>`}, nil, remote.CauseAccess, ""},
		{"a missing bucket", opGet, answer{404, "application/xml", xmlError("NoSuchBucket")}, nil, remote.CauseAccess, "NoSuchBucket"},
		{"a quota code at 400", opPut, answer{400, "application/xml", xmlError("XMinioAdminBucketQuotaExceeded")}, nil, remote.CauseQuota, "XMinioAdminBucketQuotaExceeded"},
		{"an absent key", opGet, answer{404, "application/xml", xmlError("NoSuchKey")}, remote.ErrNotFound, 0, ""},
		{"a slow-down", opPut, answer{503, "application/xml", xmlError("SlowDown")}, remote.ErrThrottled, 0, ""},
		{"a bad digest", opPut, answer{400, "application/xml", xmlError("BadDigest")}, remote.ErrCorrupt, 0, ""},
		{"an invalid range", opGet, answer{416, "application/xml", xmlError("InvalidRange")}, remote.ErrInvalid, 0, ""},
		{"a 500 with no body", opDelete, answer{500, "", ""}, remote.ErrTransient, 0, ""},
		{"an absent key to a one-name delete", opDelete, answer{404, "application/xml", xmlError("NoSuchKey")}, nil, 0, ""},
		{"a control get of an absent object", opGetControl, answer{404, "application/xml", xmlError("NoSuchKey")}, remote.ErrNotFound, 0, ""},
		{"a 2xx list that does not parse", opList, answer{200, "application/xml", "<ListBucketResult><Contents>"}, remote.ErrTransient, 0, ""},
	} {
		err := fromSDK(t.Context(), tc.op, recorded{}, nil, call(t.Context(), sdkClient(serve(t, tc.reply), nil), tc.op))
		checkErr(t, tc.name, err, tc.want, tc.cause)
		noSDKType(t, tc.name, err)

		var d *remote.DeniedError
		if errors.As(err, &d) && d.Code != tc.code {
			t.Errorf("%s: DeniedError.Code = %q, want %q", tc.name, d.Code, tc.code)
		}
	}
}

// Failures that carry no reply: a refused connection, a certificate the client
// cannot verify and a timeout under a live context are ErrTransient, though a
// timeout matches context.DeadlineExceeded; so is the SDK's checksum
// validation reading a get's body, and none keeps a network or SDK type.
func TestFromSDKWithoutAReply(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + l.Addr().String()
	_ = l.Close()

	untrusted := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(untrusted.Close)

	badSum := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-amz-checksum-crc32c", "AAAAAA==")
		_, _ = io.WriteString(w, "block bytes")
	}))
	t.Cleanup(badSum.Close)

	// A listener that never answers. net/http's timeouts take no clock, so the
	// short real timeouts below are the condition, not a wait for one.
	silent := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(silent.Close)

	headerTimeout := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 50 * time.Millisecond}}
	clientTimeout := &http.Client{Timeout: 50 * time.Millisecond}

	for _, tc := range []struct {
		name string
		err  func() error
	}{
		{"a response header timeout", func() error { return call(t.Context(), sdkClient(silent.URL, headerTimeout), opGet) }},
		{"a client timeout", func() error { return call(t.Context(), sdkClient(silent.URL, clientTimeout), opGet) }},
		{"a refused connection", func() error { return call(t.Context(), sdkClient(closed, nil), opGet) }},
		{"an untrusted certificate", func() error { return call(t.Context(), sdkClient(untrusted.URL, nil), opGet) }},
		{"a mismatching response checksum", func() error {
			out, err := sdkClient(badSum.URL, nil).GetObject(t.Context(), &s3.GetObjectInput{
				Bucket: aws.String("b"), Key: aws.String("blocks/00"), ChecksumMode: types.ChecksumModeEnabled,
			})
			if err != nil {
				return err
			}
			defer func() { _ = out.Body.Close() }()

			_, err = io.Copy(io.Discard, out.Body)
			if err == nil {
				t.Fatal("the SDK read a body whose CRC32C mismatched without an error")
			}

			return err
		}},
	} {
		sdkErr := tc.err()
		if sdkErr == nil {
			t.Fatalf("%s: the SDK call succeeded", tc.name)
		}

		err := fromSDK(t.Context(), opGet, recorded{}, nil, sdkErr)
		checkErr(t, tc.name, err, remote.ErrTransient, 0)
		noSDKType(t, tc.name, err)
	}
}

// A local failure comes back as itself: the context ending, and what a put's
// stream guard recorded, which wins over the SDK's error.
func TestFromSDKReturnsLocalFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	sdkErr := call(ctx, sdkClient(serve(t, answer{200, "", ""}), nil), opPut)
	if got := fromSDK(ctx, opPut, recorded{}, nil, sdkErr); got != context.Canceled {
		t.Errorf("fromSDK(put) over a cancelled call = %v, want context.Canceled itself", got)
	}

	expired, stop := context.WithTimeout(t.Context(), 0)
	defer stop()

	sdkErr = call(expired, sdkClient(serve(t, answer{200, "", ""}), nil), opGet)
	if got := fromSDK(expired, opGet, recorded{}, nil, sdkErr); got != context.DeadlineExceeded {
		t.Errorf("fromSDK(get) over an expired call = %v, want context.DeadlineExceeded itself", got)
	}

	readerErr := errors.New("the body's reader failed")
	sdkErr = call(t.Context(), sdkClient(serve(t, answer{500, "", ""}), nil), opPut)
	if got := fromSDK(t.Context(), opPut, recorded{}, readerErr, sdkErr); got != readerErr {
		t.Errorf("fromSDK(put) with the guard's record = %v, want the record itself", got)
	}

	if got := fromSDK(t.Context(), opGet, recorded{}, nil, nil); got != nil {
		t.Errorf("fromSDK(get, nil) = %v, want nil", got)
	}
}

// checkErr asserts err wraps want, or, with want nil, is a denial of cause
// (nil when cause is 0 too).
func checkErr(t *testing.T, name string, err, want error, cause remote.Cause) {
	t.Helper()

	if want == nil && cause == 0 {
		if err != nil {
			t.Errorf("%s = %v, want nil", name, err)
		}

		return
	}

	why, got := oneSentinel(err)
	if why != "" {
		t.Errorf("%s = %v: %s, want exactly one sentinel", name, err, why)
		return
	}

	if cause != 0 {
		var d *remote.DeniedError
		if !errors.As(err, &d) || d.Cause != cause {
			t.Errorf("%s = %v, want ErrDenied of cause %v", name, err, cause)
		}

		return
	}

	if got != want {
		t.Errorf("%s = %v (%v), want %v", name, err, got, want)
	}
}

// noSDKType asserts no SDK or network error is reachable from err.
func noSDKType(t *testing.T, name string, err error) {
	t.Helper()

	var (
		op  *smithy.OperationError
		api smithy.APIError
		re  *smithyhttp.ResponseError
		ne  net.Error
		ue  *url.Error
	)
	for what, found := range map[string]bool{
		"*smithy.OperationError":    errors.As(err, &op),
		"smithy.APIError":           errors.As(err, &api),
		"*smithyhttp.ResponseError": errors.As(err, &re),
		"net.Error":                 errors.As(err, &ne),
		"*url.Error":                errors.As(err, &ue),
	} {
		if found {
			t.Errorf("%s: %v unwraps to a %s", name, err, what)
		}
	}
}
