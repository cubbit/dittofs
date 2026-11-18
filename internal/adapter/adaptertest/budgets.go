package adaptertest

import "github.com/cubbit/dittofs/pkg/resources"

// Budgets is a fresh set of every budget a test-built server is handed: the
// four on the request path, wide enough for a loopback test's traffic, and the
// node's connection count, so no test runs a server with no connection limit
// (RFC 24 section 2.5; RFC 20 section 3.4). A test that wants a budget small
// or a limit exact replaces the field on what this returns.
func Budgets() *resources.Budgets {
	return &resources.Budgets{
		Receive:    resources.NewWaitBudget("receive", 8<<20),
		Reply:      resources.NewWaitBudget("reply", 8<<20),
		Connection: resources.NewBudget("connection", 8<<20),
		Hold:       resources.NewBudget("hold", 8<<20),
		Conns:      resources.NewConnLimit(1024),
	}
}
