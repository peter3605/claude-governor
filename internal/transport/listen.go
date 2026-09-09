package transport

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// staleProbe is how long Listen waits for an existing socket to prove a daemon is behind
// it. On darwin a unix connect to a listener whose backlog is full is refused immediately
// rather than blocking (design.md section 4.4), so this is a bound on the pathological
// case, not the normal one: a live daemon accepts in microseconds and a stale socket file
// is refused just as fast.
const staleProbe = 100 * time.Millisecond

// Listen is the v1 Listener. Like Dial it speaks unix:// and nothing else.
//
// It is the single implementation the seam is allowed (design.md section 9). A tier-two
// broker binds tcp:// with TLS behind this same interface; until it exists, an endpoint in
// any other scheme is an error rather than a best guess.
type Listen struct{}

// Listen binds the endpoint, creating its directory and clearing a stale socket first.
//
// A unix socket outlives the process that bound it, so a daemon that was killed leaves a
// file behind that bind(2) refuses to replace. Removing it unconditionally would be worse
// than the problem: two daemons would take turns unlinking each other's socket and every
// gate would talk to whichever won last. So an existing socket is *probed* — if something
// answers, this is a second daemon and it refuses to start; only a socket nothing is behind
// is removed.
func (Listen) Listen(ctx context.Context, endpoint string) (net.Listener, error) {
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

	// Named rather than left to the kernel, which answers a path this long with EINVAL —
	// surfacing as "bind: invalid argument", which reads like a bad argument to this
	// function rather than like a path a few characters too long.
	if len(path) > MaxSocketPath {
		return nil, fmt.Errorf("socket path %q is %d bytes, past this platform's limit of %d: set %s to a shorter directory",
			path, len(path), MaxSocketPath, StateDirEnv)
	}

	// 0700: the socket carries this user's session and lease state and v1 is single-user.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state directory for %q: %w", path, err)
	}
	if err := clearStale(path); err != nil {
		return nil, err
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %q: %w", path, err)
	}
	// Belt and braces over the directory mode: the socket is this user's alone. A failure
	// here leaves a listener nobody should reach, so it is fatal rather than logged.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("restrict %q: %w", path, err)
	}
	return ln, nil
}

// clearStale removes a socket file left by a dead daemon, and refuses to touch one a live
// daemon is still listening on.
func clearStale(path string) error {
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %q: %w", path, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		// Not a socket. Removing an arbitrary file because it is in the way is how a
		// governor eats something it did not create.
		return fmt.Errorf("%q exists and is not a socket (mode %s): refusing to replace it", path, fi.Mode())
	}
	if c, err := net.DialTimeout("unix", path, staleProbe); err == nil {
		c.Close()
		return fmt.Errorf("a governor daemon is already listening on %q", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket %q: %w", path, err)
	}
	return nil
}
