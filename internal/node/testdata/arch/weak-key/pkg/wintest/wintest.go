// Package wintest is a production package whose name ends in test, linked
// only into the windows build of dfs, which the host build does not see.
package wintest

func Ready() bool { return true }
