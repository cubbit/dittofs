package core

import (
	"net"
	"time"
)

// listenCfg is the config every listener binds with (RFC 20 section 6.7):
// keepalive on, 15 s idle with 3 probes 5 s apart, so a peer that vanished
// without closing is found within NFSv4's 90 s lease and its closed
// connection tells the protocol's own lease what to do with its state. No
// send or receive buffer size is set: they are the kernel's autotuning's.
// TCP_NODELAY stays on, which is Go's own default for every TCP connection
// ("The default is true", TCPConn.SetNoDelay's words), so a reply is never
// held for a client's delayed acknowledgement. The backlog and address
// reuse are Go's defaults too.
var listenCfg = net.ListenConfig{
	KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: 15 * time.Second, Interval: 5 * time.Second, Count: 3},
}
