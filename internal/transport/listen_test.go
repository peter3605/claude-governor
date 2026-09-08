package transport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unixPathMax is the practical ceiling on a unix socket path: sockaddr_un.sun_path is 104
// bytes on darwin and 108 on linux. A test whose temp directory pushes past it should say
// so rather than fail as if the code were broken.
const unixPathMax = 100

func tempEndpoint(t *testing.T) (string, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "govt")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	path := filepath.Join(dir, "s")
	if len(path) > unixPathMax {
		t.Skipf("socket path %q is %d bytes, past the sockaddr_un limit", path, len(path))
	}
	return "unix://" + path, path
}

func TestListenBindsAUnixEndpointTheDialerCanReach(t *testing.T) {
	endpoint, path := tempEndpoint(t)

	ln, err := Listen{}.Listen(context.Background(), endpoint)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// The socket is this user's alone: it carries session and lease state and v1 is
	// single-user.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode is %o, want 600", perm)
	}

	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	conn, err := Dial{}.Dial(context.Background(), endpoint)
	if err != nil {
		t.Fatalf("the dialer could not reach the listener: %v", err)
	}
	conn.Close()
}

// Listen creates the state directory rather than requiring one, so a first run works.
func TestListenCreatesTheStateDirectory(t *testing.T) {
	dir, err := os.MkdirTemp("", "govt")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	path := filepath.Join(dir, "nested", "s")
	if len(path) > unixPathMax {
		t.Skipf("socket path %q is too long", path)
	}
	ln, err := Listen{}.Listen(context.Background(), "unix://"+path)
	if err != nil {
		t.Fatalf("listen into a directory that did not exist: %v", err)
	}
	ln.Close()
}

// A unix socket outlives the process that bound it, so a killed daemon leaves a file that
// bind(2) refuses to replace. Nothing is behind this one, so it is cleared.
func TestListenClearsASocketNothingIsBehind(t *testing.T) {
	endpoint, path := tempEndpoint(t)

	first, err := Listen{}.Listen(context.Background(), endpoint)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	first.Close() // leaves the socket file behind, as a killed daemon would
	if _, err := os.Lstat(path); err != nil {
		t.Skipf("this platform removed the socket on close, so there is no stale file to clear: %v", err)
	}

	second, err := Listen{}.Listen(context.Background(), endpoint)
	if err != nil {
		t.Fatalf("second listen over a stale socket: %v", err)
	}
	second.Close()
}

// The other half of the same rule: a socket something IS behind must not be unlinked. Two
// daemons would otherwise take turns replacing each other's socket and every gate would
// reach whichever bound last.
func TestListenRefusesAnEndpointSomethingIsServing(t *testing.T) {
	endpoint, _ := tempEndpoint(t)

	first, err := Listen{}.Listen(context.Background(), endpoint)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	defer first.Close()
	go func() {
		for {
			c, err := first.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	second, err := Listen{}.Listen(context.Background(), endpoint)
	if err == nil {
		second.Close()
		t.Fatal("listen bound an endpoint another listener was already serving")
	}
	if !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("error %q does not name the cause", err)
	}

	// The original is untouched and still reachable.
	conn, err := Dial{}.Dial(context.Background(), endpoint)
	if err != nil {
		t.Fatalf("the original listener was disturbed: %v", err)
	}
	conn.Close()
}

// Removing whatever happens to be in the way is how a governor eats a file it did not
// create.
func TestListenRefusesToReplaceSomethingThatIsNotASocket(t *testing.T) {
	endpoint, path := tempEndpoint(t)
	if err := os.WriteFile(path, []byte("precious"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if ln, err := (Listen{}).Listen(context.Background(), endpoint); err == nil {
		ln.Close()
		t.Fatal("listen replaced a regular file")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "precious" {
		t.Fatalf("the file was disturbed: %q, %v", b, err)
	}
}

// v1 speaks unix:// and nothing else. An endpoint in another scheme is an error rather than
// a best guess, on both halves of the seam.
func TestBothHalvesRejectAnUnsupportedScheme(t *testing.T) {
	for _, ep := range []string{"tcp://127.0.0.1:0", "http://example.com/sock", "/plain/path"} {
		if ln, err := (Listen{}).Listen(context.Background(), ep); err == nil {
			ln.Close()
			t.Errorf("Listen accepted %q", ep)
		}
		if c, err := (Dial{}).Dial(context.Background(), ep); err == nil {
			c.Close()
			t.Errorf("Dial accepted %q", ep)
		}
	}
}

func TestDefaultEndpointFollowsTheStateDirectory(t *testing.T) {
	t.Setenv(EndpointEnv, "")
	t.Setenv(StateDirEnv, "/tmp/some-governor-state")

	got, err := DefaultEndpoint()
	if err != nil {
		t.Fatalf("DefaultEndpoint: %v", err)
	}
	if want := "unix:///tmp/some-governor-state/sock"; got != want {
		t.Fatalf("DefaultEndpoint is %q, want %q", got, want)
	}

	// An explicit endpoint overrides the directory outright.
	t.Setenv(EndpointEnv, "unix:///elsewhere/s")
	if got, _ := DefaultEndpoint(); got != "unix:///elsewhere/s" {
		t.Fatalf("DefaultEndpoint is %q, want the explicit override", got)
	}
}

var _ Listener = Listen{}
var _ Dialer = Dial{}
