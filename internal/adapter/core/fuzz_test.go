package core_test

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
)

// FuzzConnection feeds a connection arbitrary bytes: no panic, no reservation
// left once the connection ends, and no record past the cap charged (RFC 20
// sections 5.2 and 6.8).
func FuzzConnection(f *testing.F) {
	f.Add([]byte{0, 0, 0, 4, 0, 0, 0, 0})
	f.Add([]byte{0x80, 0x00, 0x00, 0x04})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, body []byte) {
		b := budgets()
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
		if r := b.Receive.Reserved(); r != 0 {
			t.Fatalf("%d receive bytes are still reserved after the connection ended", r)
		}
		if r := b.Reply.Reserved(); r != 0 {
			t.Fatalf("%d reply bytes are still reserved after the connection ended", r)
		}
	})
}
