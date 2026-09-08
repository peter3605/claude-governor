package signals

import "testing"

// The whole point of Unknown is that it is not a cut trigger. If this test ever goes
// green-to-red, a Linux user's budget has just been pinned at B_min forever.
func TestElevatedTreatsUnknownAsNominal(t *testing.T) {
	for _, tc := range []struct {
		state Thermal
		want  bool
	}{
		{Nominal, false},
		{Unknown, false},
		{Fair, true},
		{Serious, true},
		{Critical, true},
		{Thermal("something macOS 30 invented"), false},
	} {
		if got := tc.state.Elevated(); got != tc.want {
			t.Errorf("Thermal(%q).Elevated() = %v, want %v", tc.state, got, tc.want)
		}
	}
}

// The three states that matter are unreachable on a cool machine, so the mapping is tested
// directly. Getting fair and serious the wrong way round would be invisible until the day
// the budget needed to drop, which is the worst possible day to find out.
func TestThermalFromRawMapsEveryState(t *testing.T) {
	for _, tc := range []struct {
		raw  int
		want Thermal
	}{
		{0, Nominal},
		{1, Fair},
		{2, Serious},
		{3, Critical},
		{-1, Unknown}, // the shim's "API not available on this OS"
		{99, Unknown}, // a state some future macOS invents
	} {
		if got := thermalFromRaw(tc.raw); got != tc.want {
			t.Errorf("thermalFromRaw(%d) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// The mapping and the cut rule have to agree: every state the shim can produce except
// nominal must actually trigger a cut, or the signal is decorative.
func TestEveryNonNominalRawStateIsACutTrigger(t *testing.T) {
	for raw := 1; raw <= 3; raw++ {
		if !thermalFromRaw(raw).Elevated() {
			t.Errorf("thermalFromRaw(%d) = %q, which is not Elevated — it must cut the budget", raw, thermalFromRaw(raw))
		}
	}
	if thermalFromRaw(0).Elevated() {
		t.Error("nominal must not cut the budget")
	}
}

// ReadThermal must always answer with a state the control loop can act on, on every
// platform, without erroring — there is no error return precisely so that no caller can
// mistake "cannot measure" for "too hot".
func TestReadThermalAlwaysReturnsANamedState(t *testing.T) {
	got := ReadThermal()
	switch got {
	case Nominal, Fair, Serious, Critical, Unknown:
	default:
		t.Fatalf("ReadThermal() = %q, want one of the five named states", got)
	}
}

// Reading is a property access, not a subprocess: the sampler calls it at 1 Hz and the
// statusline calls it on a 50 ms budget, so it must be cheap and it must not accumulate.
func TestReadThermalIsRepeatable(t *testing.T) {
	for i := 0; i < 1000; i++ {
		if ReadThermal() == "" {
			t.Fatalf("ReadThermal() returned empty on call %d", i)
		}
	}
}
