// Command dfs links pkg/attest, a production package whose name ends in test.
// Only what dfs and dfsctl link must resolve: internal/dot, internal/dotvalue
// and internal/install import a keyprovider this module does not require, so
// no main may link them.
package main

import "example.com/weakkey/pkg/attest"

func main() { _, _ = attest.Key() }
