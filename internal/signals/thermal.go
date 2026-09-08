package signals

// ReadThermal returns the host's current thermal pressure.
//
// On macOS this is ProcessInfo.thermalState, read through the cgo shim in
// thermal_darwin.go. It is public API, needs no privileges, and costs a property read —
// powermetrics is deliberately not used here (design.md section 5.3): it wants sudo, and a
// dependency on elevated privileges kills the install story.
//
// Everywhere else — every Linux host, and a macOS binary built without cgo — this reports
// Unknown. That is a statement about the measurement, not about the machine: see Elevated.
func ReadThermal() Thermal {
	return readThermal()
}

// Elevated reports whether the thermal state is a cut trigger for the control loop
// (design.md section 5.2: any non-nominal state takes B to B_min).
//
// Unknown is NOT elevated. A host that cannot measure its own temperature is not a hot
// host, and treating it as one would pin every Linux user's budget at the floor forever.
// Section 5.3 requires Unknown be treated exactly as Nominal, and this is the one place
// that rule is written down — callers ask Elevated rather than comparing to Nominal
// themselves.
func (t Thermal) Elevated() bool {
	switch t {
	case Fair, Serious, Critical:
		return true
	default:
		// Nominal, Unknown, and any state a future macOS adds that we do not recognise.
		return false
	}
}

// thermalFromRaw maps a raw NSProcessInfoThermalState to a Thermal.
//
// It is deliberately separate from the cgo read and lives in the platform-neutral file so
// that every branch is reachable from a test on any host. Without this split, fair,
// serious and critical are untestable without a genuinely hot Mac — which is to say
// untested, on the three states that are the entire point of the signal.
func thermalFromRaw(raw int) Thermal {
	switch raw {
	case 0: // NSProcessInfoThermalStateNominal
		return Nominal
	case 1: // NSProcessInfoThermalStateFair
		return Fair
	case 2: // NSProcessInfoThermalStateSerious
		return Serious
	case 3: // NSProcessInfoThermalStateCritical
		return Critical
	default:
		// -1 from the shim when the OS predates the API, or a state a future macOS adds
		// that we cannot name. Unknown is the correct degradation: it is treated as
		// nominal, so an unrecognised value never becomes a spurious cut.
		return Unknown
	}
}
