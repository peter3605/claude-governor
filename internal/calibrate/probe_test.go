package calibrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// usableReps builds an attempt's repetitions from effective-core readings, all past the drift
// check unless listed in rejected.
func usableReps(effs []float64, rejected ...float64) []RepSample {
	var s []RepSample
	for _, e := range effs {
		s = append(s, RepSample{Eff: e, Usable: true, Attempt: 1})
	}
	for _, e := range rejected {
		s = append(s, RepSample{Eff: e, Drift: 0.3, Attempt: 1})
	}
	return s
}

// TestOneInflatedRepetitionDoesNotSetTheCeiling is GOV-47's central finding. The readings are
// trial 22 of the 30 recorded in docs/calibration.md §8: one repetition read 7.05 on a host
// whose others read 5.28 to 5.64. The maximum reported 7.05 and b_max 5; the true figure on this
// machine centres on 5.64, b_max 4. The drift check passed the 7.05 — both its baselines were
// slow together — so the estimator itself has to be what refuses it.
func TestOneInflatedRepetitionDoesNotSetTheCeiling(t *testing.T) {
	var r ProbeResult
	if !summarise(&r, usableReps([]float64{7.05, 5.64, 5.35, 5.28}, 7.06), Basis{}) {
		t.Fatalf("declined a trial whose answer was good: %s", r.Reason)
	}
	if r.EffectiveCores != 5.64 {
		t.Errorf("effective cores %.2f, want the second-highest 5.64 — one inflated repetition must not win", r.EffectiveCores)
	}
}

// TestRepetitionsThatDisagreeAreNotTrusted is clause 1's bound: the answer is used only when the
// repetition next to it agrees. Trial 26 of the recorded 30 — the one whose answer was furthest
// off, 4.99 against 5.64 — is the one it exists to stop.
func TestRepetitionsThatDisagreeAreNotTrusted(t *testing.T) {
	var r ProbeResult
	if summarise(&r, usableReps([]float64{5.01, 4.99, 4.41, 4.29, 3.87}), Basis{}) {
		t.Fatalf("trusted %.2f from repetitions %.1f%% apart, over the %.0f%% bound",
			r.EffectiveCores, r.Disagreement*100, ProbeMaxDisagreement*100)
	}
	if r.OK || r.EffectiveCores != 0 {
		t.Errorf("a declined attempt still carries a figure: %+v", r)
	}
	if !strings.Contains(r.Reason, "disagreed") {
		t.Errorf("reason does not say the repetitions disagreed: %q", r.Reason)
	}
}

// TestAWideSpreadAloneIsNotARefusal: the spread is dominated by the lowest repetition, which
// the estimator ignores. Trial 17 had a spread of 1.88 — among the widest recorded — and an
// answer of 5.75, 0.11 from the centre. Rejecting it would be rejecting a good measurement.
func TestAWideSpreadAloneIsNotARefusal(t *testing.T) {
	var r ProbeResult
	if !summarise(&r, usableReps([]float64{5.76, 5.75, 5.56, 5.24, 3.87}), Basis{}) {
		t.Fatalf("declined a good answer over a wide spread: %s", r.Reason)
	}
	if r.Spread < 1.8 {
		t.Errorf("spread %.2f, want it recorded as measured", r.Spread)
	}
}

// TestTooFewUsableRepetitionsIsAFailureNotASkip: the estimator needs three to stand on.
func TestTooFewUsableRepetitionsIsAFailureNotASkip(t *testing.T) {
	var r ProbeResult
	if summarise(&r, usableReps([]float64{5.6, 5.5}, 2.3, 7.1, 6.4), Basis{}) {
		t.Fatal("trusted a figure from two usable repetitions")
	}
	if r.Skipped || r.Reason == "" {
		t.Errorf("want a reported failure, got skipped=%v reason=%q", r.Skipped, r.Reason)
	}
}

// TestASuccessfulRetryClearsTheFirstAttemptsVerdict: summarise is called once per attempt on
// the same result, so a pass must not carry the failed attempt's reason into the provenance.
func TestASuccessfulRetryClearsTheFirstAttemptsVerdict(t *testing.T) {
	var r ProbeResult
	summarise(&r, usableReps([]float64{5.01, 4.99, 4.41, 4.29, 3.87}), Basis{})
	if !summarise(&r, usableReps([]float64{5.61, 5.58, 5.57, 5.37, 5.31}), Basis{}) {
		t.Fatalf("second attempt declined: %s", r.Reason)
	}
	if r.Reason != "" || r.EffectiveCores != 5.58 {
		t.Errorf("after a passing retry: reason %q, effective %.2f", r.Reason, r.EffectiveCores)
	}
}

// TestTheReportShowsEveryRepetition: a figure that turns out wrong can only be diagnosed from
// the repetitions behind it. GOV-47 lost exactly that for its one live escape.
func TestTheReportShowsEveryRepetition(t *testing.T) {
	m := Measurement{Cores: 8, CPUCapacity: 8, Probe: ProbeResult{
		OK: true, EffectiveCores: 5.64, Workers: 8, Reps: 5, UsableReps: 4, Attempts: 2,
		Samples: []RepSample{
			{Eff: 5.01, Usable: true, Attempt: 1}, {Eff: 4.99, Usable: true, Attempt: 1},
			{Eff: 7.06, Attempt: 2}, {Eff: 5.64, Usable: true, Attempt: 2},
		},
	}}
	got := Report(m, Resolve(Derive(m), Prior{}), Prior{})
	if !strings.Contains(got, "repetitions        5.01 4.99 | 7.06† 5.64") {
		t.Errorf("report does not list every repetition by attempt, marking the dropped one:\n%s", got)
	}
}

// TestThreeSurvivorsAreNotEnough is GOV-B59 clause 3, with the repetitions of the 2026-11-24
// field reading exactly as calibrate printed them: 3.31, 4.58†, 3.46†, 4.76, 3.19. Three
// survived, the two the answer rested on agreed within 3.6%, and the answer was 41% below this
// host's centre. Losing two of five to the drift check is the machine moving, and the survivors
// of a moving machine are not trusted however well they agree.
func TestThreeSurvivorsAreNotEnough(t *testing.T) {
	var r ProbeResult
	if summarise(&r, usableReps([]float64{3.31, 4.76, 3.19}, 4.58, 3.46), Basis{}) {
		t.Fatalf("trusted %.2f from three survivors of five", r.EffectiveCores)
	}
	if !strings.Contains(r.Reason, "3 of 5 repetitions survived") {
		t.Errorf("reason does not say how many survived: %q", r.Reason)
	}
}

// TestAReadingFarBelowTheHostsHistoryIsNotTrusted is GOV-B59 clause 1. The answer is the
// 2026-11-02 field reading, 4.73 at a 5.7% gap — inside the agreement bound, and it wrote b_max
// 3 over a measured 4. Against this host's 5.64 it is 16% low, past ProbeMaxShortfall.
func TestAReadingFarBelowTheHostsHistoryIsNotTrusted(t *testing.T) {
	reps := usableReps([]float64{4.95, 4.73, 4.46, 4.40, 4.21})
	basis := Basis{EffectiveCores: 5.64, At: "2026-11-01T19:34:07Z"}

	var r ProbeResult
	if summarise(&r, reps, basis) {
		t.Fatalf("trusted %.2f against a basis of %.2f", r.EffectiveCores, basis.EffectiveCores)
	}
	if !r.BelowBasis || r.OK || r.EffectiveCores != 0 {
		t.Errorf("a refused answer must carry no figure and say why: %+v", r)
	}
	for _, want := range []string{"4.73", "16% below", "5.64", "2026-11-01T19:34:07Z", "12%"} {
		if !strings.Contains(r.Reason, want) {
			t.Errorf("reason %q does not name %q", r.Reason, want)
		}
	}

	// The same reading is accepted on a host with no history, and when the person says the
	// machine really is slower (clause 4).
	for name, b := range map[string]Basis{"no basis": {}, "waived": {EffectiveCores: 5.64, Waived: true}} {
		var r ProbeResult
		if !summarise(&r, reps, b) || r.EffectiveCores != 4.73 {
			t.Errorf("%s: want 4.73 accepted, got ok=%v %.2f (%s)", name, r.OK, r.EffectiveCores, r.Reason)
		}
	}
}

// TestTheShortfallBoundRefusesNothingTheBenchAccepted is the derivation of ProbeMaxShortfall,
// run against the data it was derived from: every answer accepted across the attempts recorded
// in docs/calibration.md §8 must be accepted against every other one as the stored basis. If a
// later recording widens the accepted range past the bound, this fails and the constant has to
// be argued again rather than drifting silently out of date.
func TestTheShortfallBoundRefusesNothingTheBenchAccepted(t *testing.T) {
	var attempts [][]RepSample
	for _, f := range []string{"max", "validation", "after-escape"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "data", "calibration", "gov-47-probe-trials-"+f+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var trials []struct{ Samples []RepSample }
		if err := json.Unmarshal(raw, &trials); err != nil {
			t.Fatal(err)
		}
		for _, tr := range trials {
			by := map[int][]RepSample{}
			for _, s := range tr.Samples {
				by[s.Attempt] = append(by[s.Attempt], s)
			}
			for _, a := range by {
				attempts = append(attempts, a)
			}
		}
	}
	var accepted []float64
	for _, a := range attempts {
		var r ProbeResult
		if summarise(&r, a, Basis{}) {
			accepted = append(accepted, r.EffectiveCores)
		}
	}
	if len(accepted) < 50 {
		t.Fatalf("only %d of %d recorded attempts accepted; the data is not what the bound was set from", len(accepted), len(attempts))
	}
	for _, basis := range accepted {
		for i, a := range attempts {
			var plain, r ProbeResult
			if !summarise(&plain, a, Basis{}) {
				continue
			}
			if !summarise(&r, a, Basis{EffectiveCores: basis}) {
				t.Fatalf("attempt %d (%.2f) refused against a basis of %.2f, both accepted on the bench: %s",
					i, plain.EffectiveCores, basis, r.Reason)
			}
		}
	}
}

// TestTheProvenanceNoteYieldsTheBasisItRecorded: a config written before probe_effective_cores
// existed carries its reading only in the note, and the first calibrate after an upgrade — the
// one install runs unattended — has to find it there.
func TestTheProvenanceNoteYieldsTheBasisItRecorded(t *testing.T) {
	m := Measurement{Cores: 8, CPUCapacity: 8, AmbientOK: true, Probe: ProbeResult{
		OK: true, EffectiveCores: 5.64, Workers: 8, Reps: 5, UsableReps: 5, Attempts: 1,
	}}
	got, ok := EffectiveCoresInProvenance(Provenance(m, Resolve(Derive(m), Prior{})))
	if !ok || got != 5.64 {
		t.Errorf("read back %.2f, %v from the note; want 5.64", got, ok)
	}
	m.Probe = ProbeResult{Reason: "the repetitions disagreed"}
	if _, ok := EffectiveCoresInProvenance(Provenance(m, Resolve(Derive(m), Prior{BMax: 4, At: "x"}))); ok {
		t.Error("a note whose probe failed yielded a basis")
	}
}
