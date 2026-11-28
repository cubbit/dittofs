package s3fault

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
)

// testdata reads a body LocalStack 4.13.1 wrote, kept under testdata/.
func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sdkRequest is a request as the SDK hands it to the transport: its operation
// name in its context, path-style on bucket b.
func sdkRequest(t *testing.T, op, method, target string, body []byte) *http.Request {
	t.Helper()
	ctx := awsmiddleware.SetOperationName(context.Background(), op)
	req, err := http.NewRequestWithContext(ctx, method, "https://localhost.localstack.cloud:4566/b"+target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// reply is the service's reply to req, as net/http hands it back.
func reply(req *http.Request, status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Header:        http.Header{"Content-Length": {strconv.Itoa(len(body))}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// without is body less the one element that starts at from and ends at the
// first to after it.
func without(t *testing.T, body []byte, from, to string) []byte {
	t.Helper()
	i := bytes.Index(body, []byte(from))
	if i < 0 {
		t.Fatalf("testdata holds no %s", from)
	}
	j := bytes.Index(body[i:], []byte(to))
	if j < 0 {
		t.Fatalf("testdata holds no %s after %s", to, from)
	}
	return slices.Concat(body[:i], body[i+j+len(to):])
}

// readAll reads a rewritten body, and checks its Content-Length matches it.
func readAll(t *testing.T, what string, resp *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: reading the rewritten body: %v", what, err)
	}
	if resp.ContentLength != int64(len(body)) || resp.Header.Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Errorf("%s: Content-Length %d, header %q, for a %d-byte body", what, resp.ContentLength, resp.Header.Get("Content-Length"), len(body))
	}
	return body
}

// A held key's Contents element, between two others in a LocalStack listing,
// is removed alone and the reply still parses; the next listing, past the
// hold, passes whole. The keys never put through it stay, in their order.
func TestHoldNewKeysRemovesOnlyTheHeldKeysElement(t *testing.T) {
	list := testdata(t, "list-objects-v2.xml")
	hold := HoldNewKeys(1)
	put := sdkRequest(t, "PutObject", http.MethodPut, "/b&<c", []byte("x"))
	if _, err := hold.after(reply(put, http.StatusOK, nil)); err != nil {
		t.Fatal(err)
	}
	listing := func() []byte {
		req := sdkRequest(t, "ListObjectsV2", http.MethodGet, "?list-type=2", nil)
		resp, err := hold.after(reply(req, http.StatusOK, list))
		if err != nil {
			t.Fatalf("rewriting the listing: %v", err)
		}
		return readAll(t, "the listing", resp)
	}

	got := listing()
	if want := without(t, list, "<Contents><Key>b&amp;&lt;c</Key>", "</Contents>"); !bytes.Equal(got, want) {
		t.Errorf("the first listing after the put of b&<c:\n%s\nwant LocalStack's with that key's element alone removed:\n%s", got, want)
	}
	if got := listing(); !bytes.Equal(got, list) {
		t.Errorf("the second listing, past a hold of 1:\n%s\nwant LocalStack's whole", got)
	}
}

// A refused key is taken out of a LocalStack DeleteObjects request, which
// carries a Content-MD5 of what is left and no other checksum, and an Error
// entry with the code given is added to the service's reply to what is left;
// a batch without the key, and its reply, pass as they are.
func TestFailBatchKeyRewritesTheRequestAndTheReply(t *testing.T) {
	batch, result := testdata(t, "delete-objects-request.xml"), testdata(t, "delete-objects-reply.xml")
	fail := FailBatchKey("b&<c", "AccessDenied")
	req := sdkRequest(t, "DeleteObjects", http.MethodPost, "?delete=", batch)
	req.Header.Set("X-Amz-Checksum-Crc32", "LWIabQ==")

	sent, answered := fail.before(req, "DeleteObjects", "")
	if answered != nil {
		t.Fatal("FailBatchKey answered the batch, want it sent")
	}
	body, err := io.ReadAll(sent.Body)
	if err != nil {
		t.Fatal(err)
	}
	if want := without(t, batch, "<Object><Key>b&amp;&lt;c</Key>", "</Object>"); !bytes.Equal(body, want) {
		t.Errorf("the batch sent:\n%s\nwant LocalStack's request less the refused key's Object:\n%s", body, want)
	}
	sum := md5.Sum(body)
	if got, want := sent.Header.Get("Content-Md5"), base64.StdEncoding.EncodeToString(sum[:]); got != want {
		t.Errorf("the batch sent carries Content-MD5 %q, want %q, of the body sent", got, want)
	}
	if got := sent.Header.Get("X-Amz-Checksum-Crc32"); got != "" {
		t.Errorf("the batch sent still carries the client's x-amz-checksum-crc32 %q, of the body before the rewrite", got)
	}
	if sent.ContentLength != int64(len(body)) {
		t.Errorf("the batch sent has ContentLength %d, want %d", sent.ContentLength, len(body))
	}
	if sent.GetBody == nil {
		t.Error("the batch sent has no GetBody, want one replaying the body sent")
	} else if again, err := sent.GetBody(); err != nil {
		t.Errorf("GetBody of the batch sent: %v", err)
	} else if b, _ := io.ReadAll(again); !bytes.Equal(b, body) {
		t.Errorf("GetBody of the batch sent: %q, want the body sent", b)
	}

	// LocalStack's reply to the batch sent, which lists the two keys left.
	left := without(t, result, "<Deleted><Key>b&amp;&lt;c</Key>", "</Deleted>")
	resp, err := fail.after(reply(sent, http.StatusOK, left))
	if err != nil {
		t.Fatalf("rewriting the reply: %v", err)
	}
	var parsed struct {
		Deleted []struct{ Key string }
		Error   []struct{ Key, Code string }
	}
	if err := xml.Unmarshal(readAll(t, "the reply", resp), &parsed); err != nil {
		t.Fatalf("the rewritten reply does not parse: %v", err)
	}
	if len(parsed.Error) != 1 || parsed.Error[0].Key != "b&<c" || parsed.Error[0].Code != "AccessDenied" {
		t.Errorf("the rewritten reply's errors: %+v, want b&<c refused AccessDenied", parsed.Error)
	}
	if len(parsed.Deleted) != 2 || parsed.Deleted[0].Key != "a" || parsed.Deleted[1].Key != "d" {
		t.Errorf("the rewritten reply's Deleted: %+v, want the service's a and d", parsed.Deleted)
	}

	other := FailBatchKey("absent", "AccessDenied")
	req = sdkRequest(t, "DeleteObjects", http.MethodPost, "?delete=", batch)
	sent, answered = other.before(req, "DeleteObjects", "")
	if answered != nil {
		t.Fatal("FailBatchKey answered a batch without its key, want it sent")
	}
	if body, _ := io.ReadAll(sent.Body); !bytes.Equal(body, batch) {
		t.Errorf("a batch without the refused key was sent as:\n%s\nwant it whole", body)
	}
	if resp, err = other.after(reply(sent, http.StatusOK, result)); err != nil {
		t.Fatalf("the reply to a batch without the refused key: %v", err)
	}
	if got, _ := io.ReadAll(resp.Body); !bytes.Equal(got, result) {
		t.Errorf("the reply to a batch without the refused key:\n%s\nwant it whole", got)
	}
}

// Refusing a batch's only key panics: nothing would be left to send.
func TestFailBatchKeyOfTheOnlyKeyPanics(t *testing.T) {
	body := []byte(`<Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Object><Key>a</Key></Object></Delete>`)
	req := sdkRequest(t, "DeleteObjects", http.MethodPost, "?delete=", body)
	defer func() {
		if msg, _ := recover().(string); !strings.Contains(msg, "only key") {
			t.Errorf("FailBatchKey of a batch's only key recovered %q, want a panic naming the only key", msg)
		}
	}()
	FailBatchKey("a", "AccessDenied").before(req, "DeleteObjects", "")
}
