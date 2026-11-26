// Package amdtest is a production package whose name ends in test, linked
// only into the amd64 build of dfs, which an arm64 host build does not see.
package amdtest

func Ready() bool { return true }
