package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/peter3605/claude-governor/internal/budget"
	"github.com/peter3605/claude-governor/internal/lease"
	"github.com/peter3605/claude-governor/internal/transport"
)

// The GOV-34 suite: a session asking for its FIRST slot is not refused because another
// session already has one.
//
// THE INCIDENT, from the daemon's own log on 2026-09-21 (docs/design.md section 6.1):
//
//	23:45:34 budget park session=37045485-… slots=1.00/4 wait=20s
//	23:45:54 budget park-expired waited=20.001s
//	23:45:54 budget deny session=37045485-… reason=capacity cause=capacity slots=1.00/4 denials=1
//
// Five of those over ten minutes, every one of them rendered to the model as "Machine at
// capacity (1/4 build slots, thermal: nominal)". One of four slots is not capacity, the
// session that read it held nothing, and the machine measured CPU 0.54 throughout.
//
// The arithmetic was right and the outcome was wrong. `docker compose` classifies Heavy, a
// Heavy build weighs budget.SlotsFor(4.5) = 3.75 slots, and 1.00 + 3.75 > 4 — so on this
// machine's calibrated ceiling of four, a Heavy build cannot be admitted beside ANY other
// lease, however small. The other session held its 1.00 for ten minutes and the blocked
// session never got in. That is invariant 6 broken: every live session is guaranteed one
// slot before any session gets a second.
func TestAFirstSlotIsNotRefusedBecauseAnotherSessionHasOne(t *testing.T) {
	// The incident's shape exactly: budget 4, one confirmed lease held by somebody else, an
	// idle machine. A Build lease is born confirmed (internal/lease.Kind), so this is the
	// "exactly one confirmed lease held by another session" the report describes.
	endpoint, d := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
		if _, ok := d.table.Acquire(lease.Request{
			Session: "holder", Kind: lease.Build, Weight: 1, ToolUseID: "holder-build",
		}, 4); !ok {
			t.Fatal("the holder could not take its lease; the test proves nothing")
		}
	})

	if got := d.table.Slots(); got != 1 {
		t.Fatalf("the table holds %.2f slots, want exactly 1: the incident is not set up", got)
	}

	// The command the blocked session could not run. Heavy, and heavier than the four-slot
	// ceiling has left free — which is the whole point: waiting cannot make this fit while
	// the holder holds, so refusing it is refusing it forever.
	payload := `{"session_id":"blocked","hook_event_name":"PreToolUse","tool_name":"Bash",` +
		`"tool_input":{"command":"docker compose up -d"},"tool_use_id":"toolu_compose"}`

	// runGateFor asserts the grant contract: exit 0, empty stdout, inside the latency
	// ceiling. A refusal fails all three — it parks for the full wait and then prints a
	// decision — so this is the assertion that the spawn proceeded.
	diag := runGateFor(t, endpoint, payload, transport.Dial{})
	if strings.Contains(diag, "capacity") {
		t.Errorf("stderr = %q, want a grant: this session holds nothing and another holds a slot", diag)
	}

	// And it really is over the budget, taken as an escape rather than because the sum
	// happened to fit. If this ever stops being true the test has stopped testing GOV-34.
	if want := 1 + budget.SlotsFor(4.5); d.table.Slots() < want-1e-9 {
		t.Errorf("table holds %.2f slots, want %.2f: the Heavy lease was not taken", d.table.Slots(), want)
	}
	if float64(4) >= d.table.Slots() {
		t.Errorf("table holds %.2f of a 4-slot budget; this case is no longer an over-budget admission", d.table.Slots())
	}
}

// GOV-34 clause 2: the denial text and `governor status` can no longer disagree.
//
// TestNoRefusalClaimsAFullMachineItIsNotOn is the clause stated as the thing it forbids. The
// literal message the incident produced — a head of "Machine at capacity" above a slot count
// strictly below the budget it prints in the same parentheses — must be unreachable, whatever
// the daemon sends. It is asserted over the reply space rather than over one reply, because
// the defect was not one bad string: it was a renderer with no branch for the case.
func TestNoRefusalClaimsAFullMachineItIsNotOn(t *testing.T) {
	// Every shape the fields can take around the boundary, including the incident's own
	// numbers (1 of 4, a Heavy build asking 3.75) and the cases either side of it.
	for _, tc := range []struct {
		name    string
		slots   float64
		budget  int
		asking  float64
		cause   string
		wantAll []string
	}{
		{
			name: "the 2026-09-21 incident, exactly",
			// One confirmed lease of a whole slot, a four-slot budget, and a Heavy build.
			slots: 1, budget: 4, asking: 4.5 / budget.SlotCores, cause: "capacity",
			wantAll: []string{"Not enough free build slots", "1/4", "needs 3.8"},
		},
		{
			name:  "a genuinely full machine still says so",
			slots: 4, budget: 4, asking: 1, cause: "capacity",
			wantAll: []string{"Machine at capacity", "4/4"},
		},
		{
			name:  "over budget by an escape already outstanding",
			slots: 4.75, budget: 4, asking: 1, cause: "capacity",
			wantAll: []string{"Machine at capacity"},
		},
		{
			name:  "a share refusal is untouched by any of this",
			slots: 2, budget: 6, asking: 1, cause: "share",
			wantAll: []string{"This session is at its fair share", "2/6"},
		},
		{
			name:  "a daemon too old to send the weight says nothing about it",
			slots: 1, budget: 4, asking: 0, cause: "capacity",
			wantAll: []string{"Not enough free build slots", "1/4"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := acquireResponse{
				Reason: "capacity", Budget: tc.budget, Slots: tc.slots,
				Asking: tc.asking, Cause: tc.cause, Denials: 1,
			}
			got := denyReason(resp, intent{take: true, kind: "build"})

			// THE CLAUSE ITSELF. A machine holding less than its budget is not at capacity,
			// and no combination of fields may produce a sentence that says it is.
			if tc.slots < float64(tc.budget) && strings.Contains(got, "Machine at capacity") {
				t.Errorf("reason = %q claims capacity while holding %.2f of %d", got, tc.slots, tc.budget)
			}
			// Nor may the instruction half, which asserts the same fact in other words.
			if tc.slots < float64(tc.budget) {
				for _, forbidden := range []string{"using every slot", "holding every build slot", "saturated"} {
					if strings.Contains(got, forbidden) {
						t.Errorf("reason = %q says %q while holding %.2f of %d", got, forbidden, tc.slots, tc.budget)
					}
				}
			}
			for _, want := range tc.wantAll {
				if !strings.Contains(got, want) {
					t.Errorf("reason = %q, missing %q", got, want)
				}
			}
			// A weight the daemon did not send is a weight the refusal must not invent —
			// the same rule that keeps CPU out of a refusal from an unmeasured daemon.
			if tc.asking == 0 && strings.Contains(got, "needs") {
				t.Errorf("reason = %q quotes a requested weight the daemon never sent", got)
			}
		})
	}
}

// A cut budget and a small machine print the same number, and a person cannot act on the
// difference without being told which one they have. The clause asks the denial to name the
// signal; this is that, over the three states the controller can be in.
func TestARefusalNamesTheSignalThatCutTheBudget(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ceiling       int
		cut           string
		want, wantNot string
	}{
		{
			name: "run queue", ceiling: 6, cut: "run queue",
			want: "budget cut from 6 by run queue",
		},
		{
			name: "thermal", ceiling: 6, cut: "thermal",
			want: "budget cut from 6 by thermal",
		},
		{
			name:    "nothing is cutting: a three-slot machine is just a three-slot machine",
			ceiling: 3, cut: "", wantNot: "budget cut",
		},
		{
			name:    "a daemon too old to send either says neither",
			ceiling: 0, cut: "", wantNot: "budget cut",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := denyReason(acquireResponse{
				Reason: "capacity", Budget: 3, Slots: 3, Asking: 1, Cause: "capacity",
				Ceiling: tc.ceiling, Cut: tc.cut, Denials: 1,
			}, intent{take: true, kind: "build"})

			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("reason = %q, missing %q", got, tc.want)
			}
			if tc.wantNot != "" && strings.Contains(got, tc.wantNot) {
				t.Errorf("reason = %q, must not claim a cut it was not told about", got)
			}
		})
	}
}

// The denial and `governor status` read the SAME controller state, rather than each deriving
// its own answer from the signals. Two derivations is how they came to disagree.
func TestTheDenialAndStatusQuoteTheSameBudget(t *testing.T) {
	endpoint, d := startTestDaemonConfigured(t, 4, lease.Options{}, func(d *daemon) {
		// A session already holding something, so the GOV-34 escape does not apply and the
		// refusal path is reached at all.
		for i := 0; i < 4; i++ {
			if _, ok := d.table.Acquire(lease.Request{
				Session: "asker", Kind: lease.Build, Weight: 1,
				ToolUseID: fmt.Sprintf("asker-%d", i),
			}, 4); !ok {
				t.Fatalf("could not fill the table (stopped at %d)", i)
			}
		}
	})

	var refusal refuseReply
	line, err := callRaw(t, endpoint, `{"v":1,"op":"acquire","session":"asker","kind":"build",`+
		`"slots":1,"tool_use_id":"asker-x","wait_ms":0}`+"\n")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := json.Unmarshal([]byte(line), &refusal); err != nil {
		t.Fatalf("acquire reply %q: %v", line, err)
	}
	if refusal.Reason != "capacity" {
		t.Fatalf("reply = %q, want a capacity refusal", line)
	}

	var status statusReply
	line, err = callRaw(t, endpoint, `{"v":1,"op":"status"}`+"\n")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if err := json.Unmarshal([]byte(line), &status); err != nil {
		t.Fatalf("status reply %q: %v", line, err)
	}

	if refusal.Budget != status.Budget {
		t.Errorf("the refusal quotes budget %d and status quotes %d; these are the two "+
			"surfaces the incident report found disagreeing", refusal.Budget, status.Budget)
	}
	if refusal.Cut != status.Cut || refusal.Ceiling != status.Ceiling {
		t.Errorf("refusal cut=%q ceiling=%d, status cut=%q ceiling=%d: they must quote one "+
			"controller, not two derivations of it",
			refusal.Cut, refusal.Ceiling, status.Cut, status.Ceiling)
	}
	if refusal.Asking != 1 {
		t.Errorf("asking = %v, want 1: the refusal must name what the request weighed", refusal.Asking)
	}
	_ = d
}

// The message a session in the 2026-09-21 situation would read now, in full.
//
// GOLDEN RATHER THAN A LIST OF SUBSTRINGS, because the defect was never in one clause. Every
// individual fact in "Machine at capacity (1/4 build slots, thermal: nominal)" was correct and
// the sentence as a whole was not, so the thing worth pinning is the sentence. It goes stale
// on any wording change, which is the point: this text reaches a model verbatim
// (docs/hook-surface.md section 10.2) and should not be edited without somebody reading it.
func TestTheIncidentMessageInFull(t *testing.T) {
	got := denyReason(acquireResponse{
		Reason: "capacity", Budget: 4, InUse: 1, Slots: 1,
		Asking: budget.SlotsFor(4.5), Thermal: "nominal", Cause: "capacity", Denials: 1,
	}, intent{take: true, kind: "build"})

	const want = "Not enough free build slots (1/4 build slots held, this one needs 3.8, " +
		"thermal: nominal). This command was not run. This is a large build and the slots " +
		"left free are not enough for it. Wait a few seconds and run exactly the same " +
		"command again; do not look for a different way to build, and do not treat this as " +
		"a build failure."

	if got != want {
		t.Errorf("the refusal text has changed.\n got: %s\nwant: %s", got, want)
	}
}
