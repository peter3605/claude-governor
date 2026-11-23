//go:build !darwin

package signals

// LoadavgFrozenAcrossSleep is false here because it has NOT BEEN MEASURED, not because it is
// known to be false (GOV-B48).
//
// Linux's calc_global_load catches up missed updates after a NO_HZ idle stretch, and whether
// it does the same across suspend-to-RAM has not been observed on a real machine. Correcting
// for a freeze that does not happen would subtract memory the average no longer holds and
// read a busy machine as idle, which is the oversubscribing direction. So on this platform
// budget.WakeFilter detects a sleep, the daemon logs it and clears a pre-sleep cut, and the
// run queue is passed through uncorrected — the behaviour before GOV-B48, which at worst cuts
// again on a stale reading. design.md section 5.6d records it as a documented no-op.
const LoadavgFrozenAcrossSleep = false
