// Package transport turns an endpoint URL into a connection.
//
// The seam (design.md section 12.1): the endpoint is a URL, not a path, so nothing in the
// gate or the daemon assumes a scheme. v1 speaks unix://; a tier-two broker would speak
// tcp:// with TLS behind the same interface.
package transport

import (
	"context"
	"net"
)

// Dialer opens a connection to an endpoint URL such as
// "unix:///Users/you/.claude/governor/sock".
//
// The gate is on the hot path and calls this under a 100 ms deadline (section 4.4): an
// implementation MUST honour the context and MUST NOT resolve DNS for a unix endpoint.
type Dialer interface {
	Dial(ctx context.Context, endpoint string) (net.Conn, error)
}

// Listener binds an endpoint URL for the daemon to accept on.
type Listener interface {
	Listen(ctx context.Context, endpoint string) (net.Listener, error)
}
