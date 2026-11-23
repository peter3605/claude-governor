package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/transport"
)

// pressedSample is a host reading whose swap-in rate is well over the memory cut, with every
// other signal quiet, so that memory is the only thing that can have cut the budget.
func pressedSample() signals.Sample {
	return signals.Sample{
		RunQueue: 0.1, Thermal: signals.Nominal, CPUCapacity: 8,
		SwapInBytesPerSec: budget.SwapInMax * 4, SwapInKnown: true,
	}
}

// GOV-B53 clause 3: a memory cut is named as memory everywhere a cut is named. It drives the
// daemon's own tick (d.step) rather than the controller, so the wiring from Sample through the
// swap-in EWMA to the loop is what is under test, and then reads the refusal, status, the
// refusal's text and the budget log off that one daemon.
func TestAMemoryCutIsNamedOnEverySurface(t *testing.T) {
	var log lockedBuffer
	endpoint, d := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
		d.diag = &log
		d.loop = &budget.Controller{Max: 6}
		d.sources = []budget.Source{budget.LocalSource{Live: d.liveBudget, InUse: d.table.InUse}}
		d.step(pressedSample(), false, false, 0)
		// Hold the table full so the acquire below is refused and carries a cut.
		for i := 0; i < 4; i++ {
			if _, ok := d.table.Acquire(lease.Request{
				Session: "asker", Kind: lease.Build, Weight: 1,
				ToolUseID: fmt.Sprintf("asker-%d", i),
			}, 6); !ok {
				t.Fatalf("could not fill the table (stopped at %d)", i)
			}
		}
	})
	if got := d.loop.Cut(); got != "memory" {
		t.Fatalf("controller Cut() = %q after a pressed tick through d.step, want \"memory\"", got)
	}

	var status statusResponse
	line, err := callRaw(t, endpoint, `{"v":1,"op":"status"}`+"\n")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if err := json.Unmarshal([]byte(line), &status); err != nil {
		t.Fatalf("status reply %q: %v", line, err)
	}
	if status.Cut != "memory" {
		t.Errorf("status cut = %q, want \"memory\": %s", status.Cut, line)
	}

	var refusal acquireResponse
	line, err = callRaw(t, endpoint, `{"v":1,"op":"acquire","session":"asker","kind":"build",`+
		`"slots":1,"tool_use_id":"asker-x","wait_ms":0}`+"\n")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := json.Unmarshal([]byte(line), &refusal); err != nil {
		t.Fatalf("acquire reply %q: %v", line, err)
	}
	if refusal.Cut != "memory" {
		t.Fatalf("refusal cut = %q, want \"memory\": %s", refusal.Cut, line)
	}
	if text := denyReason(refusal, intent{take: true, kind: "build"}); !strings.Contains(text, "by memory") {
		t.Errorf("the refusal text does not name memory: %q", text)
	}

	var out strings.Builder
	printStatus(&out, cachedStatus{At: time.Now(), Reply: status})
	if !strings.Contains(out.String(), "memory") {
		t.Errorf("`governor status` does not name memory:\n%s", out.String())
	}

	if !strings.Contains(log.String(), "cause=memory") {
		t.Errorf("the budget log has no cause=memory line:\n%s", log.String())
	}
}

// The doctor half of clause 3, the same shape as TestDoctorNamesTheSignalCuttingTheBudget.
func TestDoctorNamesAMemoryCut(t *testing.T) {
	w := newWorld(t)
	endpoint, _ := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
		d.loop = &budget.Controller{Max: 6}
		d.loop.Step(budget.Reading{SwapIn: budget.SwapInMax * 4, SwapInKnown: true})
		d.sources = []budget.Source{budget.LocalSource{Live: d.liveBudget, InUse: d.table.InUse}}
	})
	t.Setenv(transport.EndpointEnv, endpoint)
	w.writeHooks(w.settings, selfBinary(t), goodHooks())
	w.writeCalibration()

	_, found, out := doctor(t)
	got := want(t, found, "budget", "warn", out)
	if !strings.Contains(got.detail, "memory") {
		t.Errorf("doctor's budget finding does not name memory: %q", got.detail)
	}
}

// Clause 5 through the daemon: a sample whose swap-in could not be read leaves the budget
// alone however large the number beside it — the platform said nothing, and nothing is not
// pressure.
func TestTheDaemonDoesNotCutOnAnUnreadableSwapIn(t *testing.T) {
	d := newDaemon(4, io.Discard)
	d.loop = &budget.Controller{Max: 6}
	s := pressedSample()
	s.SwapInKnown = false
	for i := 0; i < 5; i++ {
		d.step(s, false, false, 0)
	}
	if d.loop.Budget() != 6 || d.loop.Cut() != "" {
		t.Errorf("budget %d cut %q on an unreadable swap-in, want 6 and no cut",
			d.loop.Budget(), d.loop.Cut())
	}
}
