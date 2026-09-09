package main

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// The budget event log (GOV-11, design.md section 4.5).
//
// This log is the instrument the dogfooding note is written from, so it is tested like one.
// The question a week of it has to answer is "did a fixed budget of four ever bind, and what
// did it cost when it did", and each test below pins one of the three lines that answer it:
// the high-water mark, the refusal, and the wait.

// logLines returns the budget events in buf, in order.
func logLines(buf *bytes.Buffer) []string {
	var out []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if i := strings.Index(l, " budget "); i >= 0 {
			out = append(out, l[i+len(" budget "):])
		}
	}
	return out
}

// TestTheLogRecordsEachNewPeakOnce is the line that decides whether the whole exercise
// found anything.
//
// If a week's log never shows a peak equal to the budget then the budget never bound, and
// no reasoning about denials is needed or possible. Recording only the RISE is what keeps
// that readable: a line per grant would bury the one number that matters under a week of
// ordinary traffic.
func TestTheLogRecordsEachNewPeakOnce(t *testing.T) {
	var buf bytes.Buffer
	d := newDaemon(4, &buf)

	for i := 0; i < 3; i++ {
		if _, ok := d.acquire(request{V: protocolVersion, Session: "s"}).(grantReply); !ok {
			t.Fatalf("acquire %d was not granted", i)
		}
	}
	// Back down to one, then up again: the peak must not be re-reported at a level it has
	// already been seen at.
	d.table.ReleaseSession("s")
	if _, ok := d.acquire(request{V: protocolVersion, Session: "s"}).(grantReply); !ok {
		t.Fatal("acquire after release was not granted")
	}

	want := []string{"peak in_use=1/4", "peak in_use=2/4", "peak in_use=3/4"}
	got := logLines(&buf)
	if len(got) != len(want) {
		t.Fatalf("log = %q, want exactly %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestTheLogRecordsARefusalWithWhatItCost covers the event that actually costs the model
// something: a spawn the budget turned away.
func TestTheLogRecordsARefusalWithWhatItCost(t *testing.T) {
	var buf bytes.Buffer
	d := newDaemon(1, &buf)

	d.acquire(request{V: protocolVersion, Session: "first"})
	// wait_ms 0: a gate that cannot wait, so the refusal is immediate rather than parked.
	if _, ok := d.acquire(request{V: protocolVersion, Session: "second"}).(refuseReply); !ok {
		t.Fatal("the second acquire against a budget of 1 was not refused")
	}

	got := strings.Join(logLines(&buf), "\n")
	if !strings.Contains(got, "deny session=second reason=capacity in_use=1/1 denials=1") {
		t.Errorf("log = %q, want a capacity deny naming the session, the count and the pressure", got)
	}
}

// TestTheLogRecordsADepthRefusal keeps the nested-spawn case distinguishable from the
// capacity case.
//
// The two are different findings and must not be counted together: a capacity deny says the
// budget is too low for this machine, while a depth deny says a subagent tried to fan out,
// which no budget would have allowed.
func TestTheLogRecordsADepthRefusal(t *testing.T) {
	var buf bytes.Buffer
	d := newDaemon(4, &buf)

	if _, ok := d.acquire(request{V: protocolVersion, Session: "s", Depth: 1}).(refuseReply); !ok {
		t.Fatal("a depth-1 acquire was not refused")
	}
	if got := strings.Join(logLines(&buf), "\n"); !strings.Contains(got, "deny session=s reason=depth") {
		t.Errorf("log = %q, want a depth deny", got)
	}
}

// TestTheLogRecordsAWaitSeparatelyFromARefusal is what separates "the budget bound and it
// was absorbed" from "the budget bound and it cost a spawn".
//
// A week where every park was granted a moment later is a week where a fixed budget of four
// worked. A week of the same number of denials is not, and a log that could not tell them
// apart would answer the story's question wrongly in the direction that flatters the tool.
func TestTheLogRecordsAWaitSeparatelyFromARefusal(t *testing.T) {
	var buf bytes.Buffer
	d := newDaemon(1, &buf)

	d.acquire(request{V: protocolVersion, Session: "first"})
	if _, ok := d.acquire(request{V: protocolVersion, Session: "second", WaitMS: 5000}).(*parkedAcquire); !ok {
		t.Fatal("the second acquire was not parked")
	}

	got := strings.Join(logLines(&buf), "\n")
	if !strings.Contains(got, "park session=second in_use=1/1 wait=5s") {
		t.Errorf("log = %q, want a park line naming the wait it committed to", got)
	}
	if strings.Contains(got, "deny") {
		t.Errorf("log = %q, want no deny: a parked spawn has not been refused", got)
	}
}

// TestConcurrentAcquiresAreSerialisedInTheLog covers the writer rather than the events.
//
// The daemon answers every connection in its own goroutine and they all share one log. A
// week's worth of that is exactly the shape that produces interleaved lines on a real
// machine and a data race in a test whose log is a bytes.Buffer — and the log being
// unreadable is not a cosmetic problem here, because it is the only evidence GOV-11's
// question gets answered from.
//
// It is a -race test: without the detector it passes whether or not the writer is guarded.
func TestConcurrentAcquiresAreSerialisedInTheLog(t *testing.T) {
	var buf bytes.Buffer
	d := newDaemon(64, &buf)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d.acquire(request{V: protocolVersion, Session: fmt.Sprintf("s%d", i)})
		}(i)
	}
	wg.Wait()

	// Every line the log holds is a whole line. A torn write shows up as a line with no
	// recognisable event in it, which the prefix scan in logLines would drop silently — so
	// the count is checked against what was actually written rather than against itself.
	if got, want := len(logLines(&buf)), strings.Count(buf.String(), "\n"); got != want {
		t.Errorf("%d parseable events out of %d lines: the log has torn writes in it", got, want)
	}
}
