package signals

// LoadavgFrozenAcrossSleep is whether the kernel's load average stops while the machine
// sleeps and resumes from the value it stopped at (GOV-B48).
//
// MEASURED TRUE on macOS 26.4.1 (25E253), over every sleep in the 2026-11-22 trace: the first
// sample after each gap reads the pre-sleep loadavg1 exactly, and the next update applies one
// five-second decay rather than the gap's worth. design.md section 5.6d has the numbers and
// the pmset lines that time them. budget.WakeFilter corrects for it.
const LoadavgFrozenAcrossSleep = true
