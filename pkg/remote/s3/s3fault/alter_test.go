package s3fault

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"testing/iotest"
)

// A flip inverts the one byte at its offset, wherever the reads that carry it
// fall, and changes no length; an offset past the end leaves the body whole.
func TestFlipChangesOneByteAndNoLength(t *testing.T) {
	body := []byte("0123456789")
	for _, at := range []int64{0, 5, 9, 10, 100} {
		for _, r := range []struct {
			name string
			wrap func(io.Reader) io.Reader
		}{
			{"whole", func(r io.Reader) io.Reader { return r }},
			{"a byte a read", iotest.OneByteReader},
			{"half reads", iotest.HalfReader},
		} {
			got, err := io.ReadAll(&flip{ReadCloser: io.NopCloser(r.wrap(bytes.NewReader(body))), at: at})
			if err != nil {
				t.Fatalf("FlipByte(%d), %s: %v", at, r.name, err)
			}
			want := bytes.Clone(body)
			if at < int64(len(want)) {
				want[at] ^= 0xff
			}
			if !bytes.Equal(got, want) {
				t.Errorf("FlipByte(%d), %s: %q, want %q", at, r.name, got, want)
			}
		}
	}
}

// errorReply is a service's reply with status and body.
func errorReply(status int, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        strconv.Itoa(status) + " " + http.StatusText(status),
		Header:        http.Header{"Content-Length": {strconv.Itoa(len(body))}},
		Body:          io.NopCloser(bytes.NewReader([]byte(body))),
		ContentLength: int64(len(body)),
	}
}

// RecodeError rewrites only an error reply whose code is from: its status,
// its code and its length follow, and every other reply keeps its status and
// its body byte for byte.
func TestRecodeErrorTouchesOnlyItsCode(t *testing.T) {
	const (
		badDigest = `<?xml version='1.0' encoding='utf-8'?><Error><Code>BadDigest</Code><Message>m</Message></Error>`
		recoded   = `<?xml version='1.0' encoding='utf-8'?><Error><Code>InvalidRequest</Code><Message>m</Message></Error>`
	)
	for _, c := range []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantBody   string
	}{
		{"the code from", 400, badDigest, 403, recoded},
		{"another code", 404, `<Error><Code>NoSuchKey</Code></Error>`, 404, `<Error><Code>NoSuchKey</Code></Error>`},
		{"a success naming the code", 200, badDigest, 200, badDigest},
		{"no error document", 400, "", 400, ""},
		{"a body that does not parse", 400, "<Error><Code>BadDigest</Code>", 400, "<Error><Code>BadDigest</Code>"},
	} {
		resp, err := RecodeError("BadDigest", 403, "InvalidRequest").after(errorReply(c.status, c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != c.wantStatus || string(got) != c.wantBody {
			t.Errorf("%s: recoded to %d %q, want %d %q", c.name, resp.StatusCode, got, c.wantStatus, c.wantBody)
		}
		if resp.ContentLength != int64(len(got)) || resp.Header.Get("Content-Length") != strconv.Itoa(len(got)) {
			t.Errorf("%s: Content-Length %d, header %q, want %d for both", c.name, resp.ContentLength, resp.Header.Get("Content-Length"), len(got))
		}
	}
}

func TestRecodeErrorRefusesASuccess(t *testing.T) {
	for _, status := range []int{100, 200, 299, 600} {
		wantPanic(t, "RecodeError to "+strconv.Itoa(status), func() { RecodeError("BadDigest", status, "X") }, strconv.Itoa(status))
	}
	wantPanic(t, "FlipByte(-1)", func() { FlipByte(-1) }, "-1")
	for _, status := range []int{300, 599} {
		RecodeError("BadDigest", status, "X") // an error status: no panic
	}
}

// A flipped request is flipped on a resend through GetBody too, and its reply
// is left whole: the flip acts on one side of the call.
func TestFlipByteActsOnTheRequestWhenItHasABody(t *testing.T) {
	body := []byte("0123456789")
	want := bytes.Clone(body)
	want[5] ^= 0xff
	req := httptest.NewRequest(http.MethodPut, "/b/k", bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	sent, _ := FlipByte(5).before(req, "PutObject", "k")
	first, _ := io.ReadAll(sent.Body)
	resent, err := sent.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	again, _ := io.ReadAll(resent)
	if !bytes.Equal(first, want) || !bytes.Equal(again, want) {
		t.Errorf("FlipByte(5) sent %q and resent %q, want %q both times", first, again, want)
	}
	if b, _ := io.ReadAll(req.Body); !bytes.Equal(b, body[len(first):]) {
		t.Errorf("FlipByte(5) changed the caller's request: it reads %q after the send", b)
	}

	reply := errorReply(http.StatusOK, string(body))
	reply.Request = sent
	resp, _ := FlipByte(5).after(reply)
	if got, _ := io.ReadAll(resp.Body); !bytes.Equal(got, body) {
		t.Errorf("FlipByte(5) on a request with a body read its reply as %q, want it whole, %q", got, body)
	}
}
