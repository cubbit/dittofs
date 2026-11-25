// Package armtest is a production package whose name ends in test, linked
// only into the arm64 build of dfsctl, which an amd64 host build does not see.
package armtest

func Ready() bool { return true }
