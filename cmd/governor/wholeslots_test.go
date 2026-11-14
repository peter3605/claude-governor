package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/peter3605/claude-governor/internal/budget"
)

// GOV-B32: no surface shows a fractional slot count. The owner saw "3.8/4 slots" — a Heavy
// build at 3.75 plus one subagent at 0.08 — and asked for whole numbers, rounded up. Only the
// display rounds (design.md section 5.2); these tests hold the display to it.

// fractional matches a decimal anywhere a slot figure could be printed.
var fractional = regexp.MustCompile(`\d\.\d`)

func TestWholeSlotsRoundsUp(t *testing.T) {
	heavy := budget.SlotsFor(4.5)
	for _, tc := range []struct {
		in   float64
		want int
	}{
		{0, 0},
		{budget.SlotsFor(0.1), 1}, // a gated Light build is still a build
		{1, 1},
		{heavy, 4}, // 3.75 reads as 4, per the owner's call
		{1 + 1 + 1, 3},
		{0.1 + 0.2 + 2.7, 3}, // float noise in a sum must not tip a whole figure up
		{heavy + 1, 5},
	} {
		if got := wholeSlots(tc.in); got != tc.want {
			t.Errorf("wholeSlots(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Clause 3: an agent is NOT rounded up to a whole slot. Rounding 0.08 to 1 would let four
// subagents read as a full four-slot machine.
func TestAgentsAreCountedBesideTheFigureNotRoundedIntoIt(t *testing.T) {
	heavy := budget.SlotsFor(4.5)
	for _, tc := range []struct {
		name   string
		build  float64
		agents int
		want   string
	}{
		{"the 3.8/4 the owner saw", heavy, 1, "4/4 slots - 1 agent"},
		{"four agents do not fill a four-slot machine", 0, 4, "0/4 slots - 4 agents"},
		{"builds alone", 2, 0, "2/4 slots"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slots := tc.build + float64(tc.agents)*budget.AgentWeight
			raw, _ := json.Marshal(map[string]any{"v": protocolVersion, "budget": 4,
				"in_use": tc.agents, "slots": slots, "build_slots": tc.build, "agents": tc.agents})
			var reply statusResponse
			if err := json.Unmarshal(raw, &reply); err != nil {
				t.Fatal(err)
			}
			got := render(view(cachedStatus{Reply: reply}, ""), 0)
			if got != tc.want {
				t.Errorf("status line %q, want %q", got, tc.want)
			}
			if fractional.MatchString(got) {
				t.Errorf("status line %q shows a fractional slot count", got)
			}

			var out bytes.Buffer
			printStatus(&out, cachedStatus{Reply: reply})
			if fractional.MatchString(out.String()) {
				t.Errorf("governor status shows a fraction:\n%s", out.String())
			}
		})
	}
}

// A daemon from before GOV-B32 sends only the mixed total. Rounding THAT up would round its
// agents up with it, so it rounds to the nearest slot instead.
func TestAnOlderDaemonsTotalRoundsToNearest(t *testing.T) {
	if got := heldSlots(budget.SlotsFor(4.5)+budget.AgentWeight, nil); got != 4 {
		t.Errorf("Heavy + agent from an old daemon reads %d, want 4", got)
	}
	if got := heldSlots(2*budget.AgentWeight, nil); got != 0 {
		t.Errorf("two agents from an old daemon read %d, want 0", got)
	}
}

func TestRefusalsShowWholeSlots(t *testing.T) {
	heavy := budget.SlotsFor(4.5)
	build := heavy
	resp := acquireResponse{
		Reason: "capacity", Budget: 4, InUse: 3, Slots: heavy + 2*budget.AgentWeight,
		BuildSlots: &build, Agents: 2, Asking: heavy, Thermal: "nominal", Cause: "capacity",
		Denials: 1,
	}
	got := denyReason(resp, intent{take: true, kind: "build"})
	if !strings.Contains(got, "(4/4 build slots held, 2 agents, this one needs 4,") {
		t.Errorf("build refusal %q does not show whole slots", got)
	}
	if fractional.MatchString(got) {
		t.Errorf("build refusal %q shows a fraction", got)
	}

	resp.Asking = budget.AgentWeight
	got = denyReason(resp, intent{take: true, kind: "agent"})
	if strings.Contains(got, "needs") {
		t.Errorf("agent refusal %q states a rounded-up cost for a subagent", got)
	}
	if fractional.MatchString(got) {
		t.Errorf("agent refusal %q shows a fraction", got)
	}
}
