//go:build integration

package s3

import "io"

// setCheckSources sets what the client's capability check waits on and draws
// its namespace from; a nil one keeps the client's own.
func (c *Client) setCheckSources(clk clock, rand io.Reader) {
	if clk != nil {
		c.clock = clk
	}

	if rand != nil {
		c.rand = rand
	}
}
