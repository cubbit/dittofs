package client

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// Retry-After is read as seconds, at most a minute; a negative, absent or
// unreadable one is the default wait.
func TestRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"7", 7 * time.Second},
		{"", retryWait},
		{"soon", retryWait},
		{"-3", retryWait},
		{"60", time.Minute},
		{"61", time.Minute},
		{"9999999999", time.Minute},
		{"99999999999999999999", retryWait},
	} {
		if got := retryAfter(http.Header{"Retry-After": {tc.header}}); got != tc.want {
			t.Errorf("retryAfter(Retry-After: %q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

// A version naming v1 is accepted; one naming no v1 is refused naming both;
// a body that is no version is refused quoting only a short prefix of it.
func TestCheckVersion(t *testing.T) {
	if err := checkVersion([]byte(`{"majors":["v1","v2"],"later":1}`)); err != nil {
		t.Errorf("checkVersion(v1 and v2): %v, want accepted", err)
	}
	if err := checkVersion([]byte(`{"majors":["v2"]}`)); err == nil || !strings.Contains(err.Error(), "v1") || !strings.Contains(err.Error(), "v2") {
		t.Errorf("checkVersion(v2 only): error %v, want one naming v1 and v2", err)
	}
	err := checkVersion([]byte(strings.Repeat("x", 4096)))
	if err == nil || len(err.Error()) > 256 {
		t.Errorf("checkVersion(4 KiB of junk): error of %d bytes %.300v, want one quoting a short prefix", len(err.Error()), err)
	}
}

// A field of an answer the client does not know is ignored.
func TestTaggedIgnoresUnknownFields(t *testing.T) {
	body := `{"later":{"x":1},"id":"s1","spec":{"later":true,"name":"builds"},"status":{"later":[]}}`
	got, err := tagged[apiv1.ShareSpec, apiv1.ShareStatus](reply{status: http.StatusOK, header: http.Header{"Etag": {`"3"`}}, body: []byte(body)}, nil)
	if err != nil || got.ID != "s1" || got.Spec.Name != "builds" || got.ETag != `"3"` {
		t.Errorf("tagged(a body with fields the client does not know): %+v err %v, want s1 named builds with ETag \"3\"", got, err)
	}
}

// Only a 202 carries an operation.
func TestAccepted(t *testing.T) {
	op, err := accepted(reply{status: http.StatusAccepted, body: []byte(`{"id":"op1","state":"pending"}`)})
	if err != nil || op == nil || op.ID != "op1" {
		t.Errorf("accepted(202): %+v err %v, want operation op1", op, err)
	}
	for _, status := range []int{http.StatusOK, http.StatusNoContent} {
		if op, err := accepted(reply{status: status}); op != nil || err != nil {
			t.Errorf("accepted(%d with no body): %+v err %v, want no operation and no error", status, op, err)
		}
	}
}

// A program's http.DefaultTransport that is no *http.Transport, as a tracing
// wrapper is, gives a transport that still reads the proxy environment and
// bounds the dial and the TLS handshake; an *http.Transport is cloned.
func TestTransport(t *testing.T) {
	tr := transport(wrapped{http.DefaultTransport})
	if tr.Proxy == nil || tr.DialContext == nil || tr.TLSHandshakeTimeout <= 0 {
		t.Errorf("transport(a wrapper): Proxy set %v, DialContext set %v, TLS handshake timeout %v, want both set and a timeout",
			tr.Proxy != nil, tr.DialContext != nil, tr.TLSHandshakeTimeout)
	}
	base := &http.Transport{TLSHandshakeTimeout: 7 * time.Second}
	if tr := transport(base); tr == base || tr.TLSHandshakeTimeout != 7*time.Second {
		t.Errorf("transport(an *http.Transport): %p with handshake timeout %v, want a clone of %p with 7s", tr, tr.TLSHandshakeTimeout, base)
	}
}

// wrapped is a RoundTripper wrapping another, as a tracing setup's is.
type wrapped struct{ http.RoundTripper }

// An operation has ended once it succeeded, failed or was cancelled; a
// state the client does not know is refused.
func TestEnded(t *testing.T) {
	for _, tc := range []struct {
		state   apiv1.OpState
		ended   bool
		refused bool
	}{
		{apiv1.OpSucceeded, true, false},
		{apiv1.OpFailed, true, false},
		{apiv1.OpCancelled, true, false},
		{apiv1.OpPending, false, false},
		{apiv1.OpRunning, false, false},
		{apiv1.OpCancelling, false, false},
		{"paused", false, true},
		{"", false, true},
	} {
		if got, err := ended(tc.state); got != tc.ended || (err != nil) != tc.refused {
			t.Errorf("ended(%q) = %v, error %v, want %v, refused %v", tc.state, got, err, tc.ended, tc.refused)
		}
	}
}
