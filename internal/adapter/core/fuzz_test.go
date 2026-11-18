package core_test

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
)

// FuzzConnection feeds a connection a peer's arbitrary bytes: no panic, and
// nothing left of the connection once it ends - no reservation of a record,
// whatever the bytes cut as records, and every budget at zero (RFC 20
// sections 5.2 and 6.8; RFC 24 section 9's R3 and R6).
func FuzzConnection(f *testing.F) {
	f.Add([]byte{0, 0, 0, 4, 0, 0, 0, 0})
	f.Add([]byte{0x80, 0x00, 0x00, 0x04})
	f.Add([]byte{})
	f.Add(testproto.Append(nil, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("hi")}))
	f.Add(testproto.Append(nil, testproto.Msg{Op: testproto.OpPanic, Version: 1, ID: 1}))
	f.Fuzz(func(t *testing.T, body []byte) {
		b := budgets()
		adaptertest.CheckLeaks(t, b)
		srv, l := testServer(t, adapter.Deps{Budgets: b}, settings.DefaultNFS())
		conn, client := servedConn(t, srv, &l)
		_ = client.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, _ = client.Write(body)
		_ = client.Close()
		select {
		case <-core.ConnDone(conn):
		case <-time.After(2 * time.Second):
			t.Fatal("the connection's teardown did not end")
		}
	})
}
