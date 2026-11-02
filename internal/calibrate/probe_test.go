package calibrate

import (
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
	if !summarise(&r, usableReps([]float64{7.05, 5.64, 5.35, 5.28}, 7.06)) {
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
	if summarise(&r, usableReps([]float64{5.01, 4.99, 4.41, 4.29, 3.87})) {
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
	if !summarise(&r, usableReps([]float64{5.76, 5.75, 5.56, 5.24, 3.87})) {
		t.Fatalf("declined a good answer over a wide spread: %s", r.Reason)
	}
	if r.Spread < 1.8 {
		t.Errorf("spread %.2f, want it recorded as measured", r.Spread)
	}
}

// TestTooFewUsableRepetitionsIsAFailureNotASkip: the estimator needs three to stand on.
func TestTooFewUsableRepetitionsIsAFailureNotASkip(t *testing.T) {
	var r ProbeResult
	if summarise(&r, usableReps([]float64{5.6, 5.5}, 2.3, 7.1, 6.4)) {
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
	summarise(&r, usableReps([]float64{5.01, 4.99, 4.41, 4.29, 3.87}))
	if !summarise(&r, usableReps([]float64{5.61, 5.58, 5.57, 5.37, 5.31})) {
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
