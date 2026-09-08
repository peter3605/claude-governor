// Package transport turns an endpoint URL into a connection.
//
// The seam (design.md section 12.1): the endpoint is a URL, not a path, so nothing in the
// gate or the daemon assumes a scheme. v1 speaks unix://; a tier-two broker would speak
// tcp:// with TLS behind the same interface.
package transport

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
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

// EndpointEnv overrides the endpoint URL outright. Tests use it, and so does anyone
// running a second governor beside the first.
const EndpointEnv = "GOVERNOR_ENDPOINT"

// StateDirEnv overrides the directory holding the socket, per design.md section 13.3.
//
// It is spelled the same as the Phase 0 trace log's override on purpose: one directory
// holds all of the governor's state, and a person who moves it expects the whole of it to
// move. The constant is duplicated rather than shared because internal/trace is deleted
// when Phase 1 lands (section 9) and this package is not.
const StateDirEnv = "GOVERNOR_STATE_DIR"

// DefaultEndpoint is where the daemon listens: unix:// over a socket in the state
// directory, ~/.claude/governor/sock unless overridden.
//
// This is on the hot path, so it is two environment lookups and a home directory — no file
// is opened and no name is resolved (design.md section 4.4). The endpoint is a URL rather
// than a path because that is the seam: a tier-two broker changes this string to tcp://
// and nothing above it changes.
func DefaultEndpoint() (string, error) {
	if e := os.Getenv(EndpointEnv); e != "" {
		return e, nil
	}
	dir := os.Getenv(StateDirEnv)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".claude", "governor")
	}
	return "unix://" + filepath.Join(dir, "sock"), nil
}

// Dial is the v1 Dialer. It speaks unix:// and nothing else.
//
// It is the single implementation the seam is allowed (design.md section 9): a second one
// arrives with tier two, not before. An endpoint in any other scheme is an error rather
// than a best guess, because the gate's answer to an error is to fail open, and failing
// open on a misconfigured endpoint is far better than connecting to something unintended.
type Dial struct{}

// Dial opens the endpoint, honouring ctx's deadline for the connect itself.
func (Dial) Dial(ctx context.Context, endpoint string) (net.Conn, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "unix" {
		return nil, fmt.Errorf("unsupported endpoint scheme %q in %q: v1 speaks unix://", u.Scheme, endpoint)
	}
	path := u.Path
	if path == "" {
		return nil, fmt.Errorf("endpoint %q has no socket path", endpoint)
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}
