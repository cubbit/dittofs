// Package tagtest is a package whose name ends in test, linked into dfs only
// under the weakkeytagged tag, which the release never sets.
package tagtest

func Ready() bool { return true }
