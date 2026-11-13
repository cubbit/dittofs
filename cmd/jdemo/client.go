package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// envelope is the one shape every route answers: {"ok":true,"result":…} or
// {"ok":false,"error":"<the journal's own error text>"}.
type envelope struct {
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// Client talks to the jdemo daemon over its unix socket. It is the only
// thing every CLI subcommand does with the journal: the daemon owns it.
type Client struct {
	socket string
	hc     *http.Client
}

// Dial returns the client for the daemon at socket.
func Dial(socket string) *Client {
	return &Client{
		socket: socket,
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
			Timeout: socketTimeout * 10,
		},
	}
}

// Do sends req (nil for none) as JSON and decodes a successful answer's
// result into resp (nil to drop it). A failed answer comes back with the
// journal's error text verbatim; an unreachable daemon as an error friendly
// names.
func (c *Client) Do(ctx context.Context, method, path string, req any, resp any) error {
	var body io.Reader
	if req != nil {
		b, err := json.Marshal(req)
		if err != nil {
			return fmt.Errorf("jdemo: encoding request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	hreq, err := http.NewRequestWithContext(ctx, method, "http://jdaemon"+path, body)
	if err != nil {
		return fmt.Errorf("jdemo: %s %s: %w", method, path, err)
	}
	if req != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}
	hresp, err := c.hc.Do(hreq)
	if err != nil {
		return &unreachableError{path: c.socket, err: err}
	}
	defer hresp.Body.Close()
	var env envelope
	if err := json.NewDecoder(hresp.Body).Decode(&env); err != nil {
		return fmt.Errorf("jdemo: %s %s: decoding answer: %w", method, path, err)
	}
	if !env.OK {
		return journalError(env.Error)
	}
	if resp != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, resp); err != nil {
			return fmt.Errorf("jdemo: %s %s: decoding result: %w", method, path, err)
		}
	}
	return nil
}

// journalError carries an answer's error text without touching it, so the
// journal's own words — including "not implemented: …" — reach the terminal.
func journalError(text string) error {
	if text == "" {
		return fmt.Errorf("jdemo: daemon answered an empty error")
	}
	return fmt.Errorf("%s", text)
}

// unreachableError is a daemon that answered nothing at all.
type unreachableError struct {
	path string
	err  error
}

func (e *unreachableError) Error() string {
	// The path is the whole diagnosis; the transport error beneath it says
	// nothing a presenter can act on.
	_ = e.err
	return fmt.Sprintf("daemon not reachable at %s", e.path)
}

// versionJSON is a version on the wire.
type versionJSON struct {
	Epoch   uint64 `json:"epoch"`
	Counter uint64 `json:"counter"`
}

func versionToJSON(v journal.Version) versionJSON {
	return versionJSON{Epoch: v.Epoch, Counter: v.Counter}
}

func versionFromJSON(v versionJSON) journal.Version {
	return journal.Version{Epoch: v.Epoch, Counter: v.Counter}
}
