package main

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/lease"
)

// GOV-B49: a reply the daemon could not deliver names what it was replying to, and the two
// expected kinds of hang-up are not logged as errors.

func TestHangupCause(t *testing.T) {
	now := time.Date(2026, 11, 22, 21, 20, 0, 0, time.UTC)
	ms := time.Millisecond
	cases := []struct {
		name       string
		took, wall time.Duration
		givesUp    time.Time
		lastWake   time.Time
		want       string
	}{
		{"client passed its deadline", 60 * ms, 60 * ms, now.Add(-10 * ms), time.Time{}, "deadline"},
		{"exactly at the deadline", 60 * ms, 60 * ms, now, time.Time{}, "deadline"},
		{"client still had time", 3 * ms, 3 * ms, now.Add(40 * ms), time.Time{}, "unexplained"},
		{"older client sends no deadline", 60 * ms, 60 * ms, time.Time{}, time.Time{}, "unexplained"},
		{"the exchange spanned a sleep", 20 * ms, 20*ms + 9*time.Second, now.Add(time.Hour), time.Time{}, "sleep"},
		{"a wake just before", 3 * ms, 3 * ms, now.Add(time.Hour), now.Add(-1500 * ms), "sleep"},
		{"a wake long before", 3 * ms, 3 * ms, now.Add(time.Hour), now.Add(-time.Minute), "unexplained"},
		{"clock jitter is not a sleep", 3 * ms, 3*ms + 200*ms, now.Add(time.Hour), time.Time{}, "unexplained"},
	}
	for _, c := range cases {
		if got := hangupCause(c.took, c.wall, now, c.givesUp, c.lastWake); got != c.want {
			t.Errorf("%s: cause %q, want %q", c.name, got, c.want)
		}
	}
}

func TestEncodeRequestStampsTheClientsDeadline(t *testing.T) {
	deadline := time.UnixMilli(1795408010811)
	line, err := encodeRequest(statusRequest{V: protocolVersion, Op: "status", Client: "statusline"}, deadline)
	if err != nil {
		t.Fatal(err)
	}
	var got request
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("stamped request %s does not decode: %v", line, err)
	}
	if got.V != protocolVersion || got.Op != "status" || got.Client != "statusline" {
		t.Errorf("stamping changed the request: %s", line)
	}
	if got.DeadlineMS != deadline.UnixMilli() {
		t.Errorf("deadline_ms = %d, want %d", got.DeadlineMS, deadline.UnixMilli())
	}

	empty, err := encodeRequest(struct{}{}, deadline)
	if err != nil || string(empty) != `{"deadline_ms":1795408010811}` {
		t.Errorf("empty object stamped as %s (%v)", empty, err)
	}
}

func TestClientOfNamesEverySender(t *testing.T) {
	for op, want := range map[string]string{
		"acquire": "gate", "finish": "hook", "settle": "hook", "advise": "mcp", "control": "control",
	} {
		if got := clientOf(op, ""); got != want {
			t.Errorf("clientOf(%s) = %s, want %s", op, got, want)
		}
	}
	if got := clientOf("status", "statusline"); got != "statusline" {
		t.Errorf("a named status client is %s", got)
	}
	// Text a client sends is not copied into the log unless it is one of the known names.
	if got := clientOf("status", "x y=z"); got != "-" {
		t.Errorf("an unknown status client is logged as %q", got)
	}
	if ex := exchangeOf(request{Op: "rm -rf"}, time.Now()); ex.op != "other" {
		t.Errorf("an unknown op is logged as %q", ex.op)
	}
}

// hangUp sends one request and closes the connection without reading, so the daemon's write
// of the reply fails, then returns the log line that write produced.
func hangUp(t *testing.T, req map[string]any) string {
	t.Helper()
	var log lockedBuffer
	endpoint, _ := startTestDaemonConfigured(t, 4, lease.Options{},
		func(d *daemon) { d.diag = newDaemonLog(&log) })

	conn, err := net.Dial("unix", strings.TrimPrefix(endpoint, "unix://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	line, _ := json.Marshal(req)
	if _, err := conn.Write(append(line, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range strings.Split(log.String(), "\n") {
			if strings.Contains(l, "reply undelivered") || strings.Contains(l, "write reply") {
				return l
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no undelivered-reply line; log:\n%s", log.String())
	return ""
}

func TestAStatusLineThatGaveUpIsAnOrdinaryLine(t *testing.T) {
	req := acquireStatusReq()
	req["client"] = "statusline"
	req["deadline_ms"] = time.Now().Add(-5 * time.Millisecond).UnixMilli()
	l := hangUp(t, req)
	for _, want := range []string{"reply undelivered", "op=status", "client=statusline", "reply=only", "took=", "left=-", "cause=deadline"} {
		if !strings.Contains(l, want) {
			t.Errorf("line lacks %q: %s", want, l)
		}
	}
	if strings.Contains(l, "write reply") {
		t.Errorf("a client past its own deadline is logged as a write error: %s", l)
	}
}

func TestAnUnexplainedHangUpIsStillAnError(t *testing.T) {
	req := acquireStatusReq()
	req["client"] = "doctor"
	req["deadline_ms"] = time.Now().Add(time.Minute).UnixMilli()
	l := hangUp(t, req)
	for _, want := range []string{"write reply:", "op=status", "client=doctor", "cause=unexplained"} {
		if !strings.Contains(l, want) {
			t.Errorf("line lacks %q: %s", want, l)
		}
	}
}

func TestAnOlderClientsHangUpSaysItsDeadlineIsUnknown(t *testing.T) {
	l := hangUp(t, acquireReq("old-gate"))
	for _, want := range []string{"write reply:", "op=acquire", "client=gate", "deadline=unknown", "cause=unexplained"} {
		if !strings.Contains(l, want) {
			t.Errorf("line lacks %q: %s", want, l)
		}
	}
}
