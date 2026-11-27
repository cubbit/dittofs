//go:build ad_dc

package ad_dc_test

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/internal/identity/netlogon"
)

// secureChannelRefusesZeroReply sets up netlogon's secure channel as DITTOFS$
// with machineAccountPass on the controller at dcIP, through a connection
// that zeroes the server challenge or the server credential in the
// controller's reply: the client refuses either, and an all-zero challenge
// before it sends NetrServerAuthenticate3 (MS-NRPC §3.1.4.1).
func secureChannelRefusesZeroReply(ctx context.Context, t *testing.T, dcIP string) {
	for _, tc := range []struct {
		name    string
		stubLen int    // the reply whose stub is this long has its first 8 bytes zeroed; 0 for none
		errText string // empty for a channel that sets up
		auth3   bool   // whether NetrServerAuthenticate3 reaches the controller
	}{
		{"unchanged", 0, "", true},
		// NetrServerReqChallenge's reply: the server challenge and the status.
		{"all-zero server challenge", 12, "server challenge", false},
		// NetrServerAuthenticate3's reply: the server credential, the
		// negotiated flags, the account RID and the status. The credential
		// the client computes is not all zero here, so this row checks end to
		// end that a zeroed credential is refused; the unit rows guard the
		// all-zero check itself.
		{"all-zero server credential, end to end", 20, "server credential", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &opRecord{}
			dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				if _, port, _ := net.SplitHostPort(addr); port == "135" {
					return c, nil
				}
				return &zeroReplyConn{Conn: c, stubLen: tc.stubLen, rec: rec}, nil
			}
			dc, err := netlogon.NewDialer(dial).Dial(ctx, fixtureRecord(), dcIP)
			if err != nil {
				t.Fatalf("dial the DC: %v", err)
			}
			defer func() { _ = dc.Close() }()
			ch, err := dc.Channel(ctx, "dittofs", []byte(machineAccountPass))
			if tc.errText == "" {
				if err != nil {
					t.Fatalf("Channel = %v, want a channel", err)
				}
				_ = ch.Close()
			} else {
				if err == nil {
					_ = ch.Close()
					t.Fatalf("Channel = a channel, want an error naming the %s", tc.errText)
				}
				if !strings.Contains(err.Error(), tc.errText) {
					t.Errorf("Channel = %v, want an error naming the %s", err, tc.errText)
				}
			}
			if got := rec.sent(opAuthenticate3); got != tc.auth3 {
				t.Errorf("NetrServerAuthenticate3 sent = %v, want %v", got, tc.auth3)
			}
		})
	}
}

// opRecord is the operation numbers of the requests written on a channel's
// connections.
type opRecord struct {
	mu  sync.Mutex
	ops []uint16
}

func (r *opRecord) sent(op uint16) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.ops, op)
}

// zeroReplyConn passes a connection through, recording each request PDU's
// operation number, and zeroes the first 8 stub bytes of every unauthenticated
// response PDU whose stub is stubLen bytes long, none when stubLen is under 8.
// Every PDU is little-endian, as go-msrpc and Samba send them.
type zeroReplyConn struct {
	net.Conn
	stubLen int
	rec     *opRecord
	buf     []byte
}

// The connection-oriented PDU header (C706 §12.6.3): ptype at 2, frag_len at
// 8, auth_len at 10; a request's opnum at 22; a response's stub at 24.
const (
	pduRequest      = 0
	pduResponse     = 2
	pduCommonHeader = 16
	pduStub         = 24
)

func (c *zeroReplyConn) Write(b []byte) (int, error) {
	for p := b; len(p) >= pduStub; {
		if p[2] == pduRequest {
			c.rec.mu.Lock()
			c.rec.ops = append(c.rec.ops, binary.LittleEndian.Uint16(p[22:]))
			c.rec.mu.Unlock()
		}
		n := int(binary.LittleEndian.Uint16(p[8:]))
		if n < pduStub || n > len(p) {
			break
		}
		p = p[n:]
	}
	return c.Conn.Write(b)
}

func (c *zeroReplyConn) Read(b []byte) (int, error) {
	if len(c.buf) == 0 {
		pdu := make([]byte, pduCommonHeader)
		if _, err := io.ReadFull(c.Conn, pdu); err != nil {
			return 0, err
		}
		n := int(binary.LittleEndian.Uint16(pdu[8:]))
		if n < pduCommonHeader {
			return 0, io.ErrUnexpectedEOF
		}
		pdu = append(pdu, make([]byte, n-pduCommonHeader)...)
		if _, err := io.ReadFull(c.Conn, pdu[pduCommonHeader:]); err != nil {
			return 0, err
		}
		if c.stubLen >= 8 && pdu[2] == pduResponse && binary.LittleEndian.Uint16(pdu[10:]) == 0 && n == pduStub+c.stubLen {
			clear(pdu[pduStub : pduStub+8])
		}
		c.buf = pdu
	}
	n := copy(b, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}
