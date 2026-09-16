package calibrate

import (
	"fmt"
	"strings"
	"time"
)

// probeNotice is the line printed before the probe starts.
//
// IT IS PRINTED BEFORE, NOT AFTER, AND THAT IS THE WHOLE REQUIREMENT. "The probe must be
// bounded and polite — a few seconds, clearly announced. An installer that pegs someone's CPU
// without warning is a bad first impression." A notice that appears once the fans have already
// spun up is an explanation, not a warning.
func probeNotice(workers int, total time.Duration) string {
	return fmt.Sprintf("measuring what %d cores actually deliver: about %s of full load, starting now",
		workers, total.Round(100*time.Millisecond))
}

// Provenance is the note recorded in the config beside the calibrated numbers: when this host
// was measured, and from what.
//
// A NUMBER NOBODY CAN TRACE BACK TO A MEASUREMENT GETS TREATED AS ARBITRARY AND EDITED AT
// RANDOM SIX MONTHS LATER — the story's own trap, and the reason this is a required field
// rather than a nicety. It is one line so it survives being read in a JSON file, and it names
// the reading that decided the threshold rather than merely the tool that wrote it, because
// "governor calibrate wrote this" tells a reader nothing they could act on.
func Provenance(m Measurement) string {
	var b strings.Builder
	fmt.Fprintf(&b, "governor calibrate %s", m.At.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "; %d cores", m.Cores)
	if m.PerformanceCoresKnown {
		fmt.Fprintf(&b, " (%d performance, %d efficiency)", m.PerformanceCores, m.Cores-m.PerformanceCores)
	} else {
		b.WriteString(" (no performance/efficiency split published by this platform)")
	}
	fmt.Fprintf(&b, "; %.1f GiB RAM", float64(m.MemoryBytes)/(1<<30))
	fmt.Fprintf(&b, "; capacity %.2f cores from %s", m.CPUCapacity, orUnknown(m.CPUCapacitySource))
	fmt.Fprintf(&b, "; ambient run queue %.2f per core (loadavg1 %.2f)", m.AmbientRunQueue, m.LoadAvg1)
	switch {
	case m.Probe.OK:
		fmt.Fprintf(&b, "; probe measured %.2f effective cores of %d (max of %d usable of %d reps, spread %.2f)",
			m.Probe.EffectiveCores, m.Probe.Workers, m.Probe.UsableReps, m.Probe.Reps, m.Probe.Spread)
	case m.Probe.Skipped:
		fmt.Fprintf(&b, "; probe skipped (%s)", m.Probe.Reason)
	case m.Probe.Reason != "":
		fmt.Fprintf(&b, "; probe failed (%s)", m.Probe.Reason)
	default:
		b.WriteString("; probe not run")
	}
	fmt.Fprintf(&b, "; run-queue cut = ambient + %.2f headroom, floored at the shipped 2.0", BuildHeadroom)
	return b.String()
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// Report renders a calibration for a person, which is a different job from Provenance. It is
// what `governor calibrate` prints: what was measured, what was derived, and — the part a
// one-line note cannot carry — what to do about a reading that is not representative.
func Report(m Measurement, d Derived, prev Derived) string {
	var b strings.Builder

	b.WriteString("measured\n")
	fmt.Fprintf(&b, "  cores              %d\n", m.Cores)
	if m.PerformanceCoresKnown {
		fmt.Fprintf(&b, "  performance cores  %d of %d\n", m.PerformanceCores, m.Cores)
	} else {
		fmt.Fprintf(&b, "  performance cores  not distinguishable on this platform\n")
	}
	fmt.Fprintf(&b, "  memory             %.1f GiB\n", float64(m.MemoryBytes)/(1<<30))
	fmt.Fprintf(&b, "  cpu capacity       %.2f cores (%s)\n", m.CPUCapacity, orUnknown(m.CPUCapacitySource))
	fmt.Fprintf(&b, "  ambient run queue  %.2f per core (loadavg1 %.2f over the last minute)\n",
		m.AmbientRunQueue, m.LoadAvg1)
	switch {
	case m.Probe.OK:
		fmt.Fprintf(&b, "  effective cores    %.2f of %d under parallel load\n",
			m.Probe.EffectiveCores, m.Probe.Workers)
		fmt.Fprintf(&b, "                     (max of %d usable of %d repetitions, spread %.2f)\n",
			m.Probe.UsableReps, m.Probe.Reps, m.Probe.Spread)
	case m.Probe.Skipped:
		fmt.Fprintf(&b, "  effective cores    not measured: %s\n", m.Probe.Reason)
	default:
		fmt.Fprintf(&b, "  effective cores    probe failed: %s\n", m.Probe.Reason)
	}

	b.WriteString("\nderived\n")
	fmt.Fprintf(&b, "  run-queue cut      %.2f per core%s\n", d.RunQueueMax, moved(d.RunQueueMax, prev.RunQueueMax))
	fmt.Fprintf(&b, "  b_max              %d slots%s\n", d.BMax, movedInt(d.BMax, prev.BMax))

	// THE ADVICE IS PART OF THE OUTPUT, because the limitation it names is the one this
	// measurement genuinely has and the one a reader cannot see from the numbers.
	b.WriteString("\nThe run-queue cut is placed above what this machine was doing in the minute\n")
	b.WriteString("before now. That is the right baseline only if now is representative — a\n")
	b.WriteString("machine calibrated while idle will throttle once you are actually working.\n")
	b.WriteString("Re-run `governor calibrate` mid-session to correct it.\n")
	return b.String()
}

func moved(now, before float64) string {
	if before == 0 || before == now {
		return ""
	}
	return fmt.Sprintf("  (was %.2f)", before)
}

func movedInt(now, before int) string {
	if before == 0 || before == now {
		return ""
	}
	return fmt.Sprintf("  (was %d)", before)
}
