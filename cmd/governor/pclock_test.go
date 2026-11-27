package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/peter3605/claude-governor/internal/signals"
	"github.com/peter3605/claude-governor/internal/trace"
)

// `governor status` shows the P-cluster clock with its busy share first, says "idle" for an
// idle cluster rather than a misleading low MHz, and gives an unknown reading with its reason.
// A daemon older than GOV-B61 sends no p_clock and gets no line.
func TestStatusShowsThePClusterClock(t *testing.T) {
	cases := map[string]struct {
		pc   *pclockStatus
		want string
	}{
		"throttled": {&pclockStatus{Known: true, MHz: 1851, Active: 1, Frac: 1851.0 / 3204, TopMHz: 3204},
			"p-cluster 100% busy at 1851 MHz, 58% of its 3204 MHz top"},
		"idle":       {&pclockStatus{Known: true, TopMHz: 3204}, "p-cluster idle"},
		"unknown":    {&pclockStatus{Reason: "no IOReport on this platform"}, "p-cluster unknown (no IOReport on this platform)"},
		"old daemon": {nil, ""},
	}
	for name, c := range cases {
		reply := statusResponse{V: protocolVersion, Budget: 3, Thermal: signals.Nominal, PClock: c.pc}
		var out bytes.Buffer
		printStatus(&out, cachedStatus{At: time.Now(), Reply: reply})
		got := out.String()
		if c.want == "" {
			if strings.Contains(got, "p-cluster") {
				t.Errorf("%s: printed a p-cluster line for a daemon that sent none:\n%s", name, got)
			}
			continue
		}
		if !strings.Contains(got, c.want+"\n") {
			t.Errorf("%s: want %q in:\n%s", name, c.want, got)
		}
	}
}

// The daemon's reply carries what the sampler read, and a reading that was not taken arrives
// as known=false with its reason and no numbers.
func TestTheStatusReplyCarriesThePClock(t *testing.T) {
	for _, pc := range []signals.PClock{
		{MHz: 2017, Active: 0.98, Frac: 2017.0 / 3204, TopMHz: 3204, Known: true},
		{Reason: "IOReport subscription refused"},
	} {
		raw, err := json.Marshal(statusReply{PClock: func() *pclockStatus { s := pclockStatusOf(pc); return &s }()})
		if err != nil {
			t.Fatal(err)
		}
		var back statusResponse
		if err := json.Unmarshal(raw, &back); err != nil || back.PClock == nil {
			t.Fatalf("round trip: %v, %+v", err, back.PClock)
		}
		got := *back.PClock
		if got.Known != pc.Known || got.MHz != pc.MHz || got.Reason != pc.Reason {
			t.Errorf("sent %+v, got %+v", pc, got)
		}
		if !pc.Known && strings.Contains(string(raw), `"mhz"`) {
			t.Errorf("an unknown clock carried a number: %s", raw)
		}
	}
}

// In the trace an unknown clock is ABSENT, not zero: an unread clock is not a stopped one, and
// throttle analysis over the trace must not see a cluster at 0 MHz.
func TestAnUnknownClockIsAbsentFromTheTrace(t *testing.T) {
	var s trace.Sample
	s.PMHz, s.PActive, s.PFrac, s.PTopMHz = pclockFields(signals.PClock{Reason: "no IOReport on this platform"})
	raw, _ := json.Marshal(s)
	for _, k := range []string{"p_mhz", "p_active", "p_frac", "p_top_mhz"} {
		if strings.Contains(string(raw), `"`+k+`"`) {
			t.Errorf("unknown clock wrote %s: %s", k, raw)
		}
	}
	s.PMHz, s.PActive, s.PFrac, s.PTopMHz = pclockFields(signals.PClock{MHz: 1850, Active: 1, Frac: 0.577, TopMHz: 3204, Known: true})
	raw, _ = json.Marshal(s)
	if !strings.Contains(string(raw), `"p_mhz":1850`) || !strings.Contains(string(raw), `"p_top_mhz":3204`) {
		t.Errorf("known clock not recorded: %s", raw)
	}
}
