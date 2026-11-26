package nfsclient

import "net"

// Over is a client on conn, so a test can wrap the connection it reads.
func Over(conn net.Conn) *Client {
	return &Client{conn: conn}
}
