// Command dfs links pkg/attest, a production package whose name ends in test.
package main

import "example.com/weakkey/pkg/attest"

func main() { _, _ = attest.Key() }
