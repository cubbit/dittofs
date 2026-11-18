//go:build !linux

package core

import (
	"net"
	"time"
)

// userTimeout sets nothing on a platform without TCP_USER_TIMEOUT
// (RFC 20 section 6.7: "where the platform has it").
func userTimeout(net.Conn, time.Duration) {}
