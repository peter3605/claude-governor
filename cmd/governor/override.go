package main

import (
	"sync"
	"time"
)

// The manual overrides of design.md section 9.1 (GOV-B01).
//
// An adaptive budget is only safe to leave switched on if a person can say "not right now".
// The control loop discovers a heavy build by being hurt by it; someone who already knows one
// is coming should be able to say so instead of waiting for the loop to find out. That is the
// whole argument for these existing, and it is also the argument for every one of them
// expiring on its own.
//
// EVERY OVERRIDE HAS A DEADLINE, AND THAT IS THE STORY'S TRAP RATHER THAN A DETAIL. A manual
// override that silently outlives the reason it was set is the classic way a tool like this
// stops being trusted: the budget is wrong, nothing on screen explains why, and the person who
// pinned it three hours ago is in a different context and has no reason to suspect themselves.
// So a pin is not a mode, it is a lease — it is reported while it holds, and it ends whether or
// not anybody remembers it.
//
// NOTHING HERE IS PERSISTED, for the reason section 12.2 gives about the lease table it sits
// beside: a restart losing the state is correct on one machine. An override that survived a
// daemon restart would be the forgotten-override failure with a longer fuse, since `governor
// install` restarts the daemon and an upgrade would carry the pin silently across it.
// THE ZERO VALUE IS USABLE, and that is a correctness property rather than a convenience. The
// daemon is built as a struct literal in dozens of tests and by newDaemonWithLeases in
// production, and a pointer field that only one of those paths initialised produced a nil
// dereference on the ACQUIRE path — the hot path, reached from every spawn. A type whose zero
// value works cannot be forgotten by a constructor added later.
type override struct {
	mu sync.Mutex
	// now is the clock, injectable so expiry can be tested without sleeping. Nil means
	// time.Now; see clock.
	now func() time.Time

	// limit is the pinned budget in slots. Zero means no pin.
	limit      int
	limitUntil time.Time

	paused      bool
	pausedUntil time.Time
}

// defaultOverrideTTL is how long a pin or a pause lasts when no duration is given.
//
// One hour is chosen against what an override is FOR rather than against any measurement. The
// reason someone pins the budget is a piece of work they can see the end of — a release build,
// a big test run, a demo — and an hour outlasts those while staying well inside one sitting.
// Longer and it survives the context that justified it, which is the failure this whole file
// is shaped around; much shorter and it expires in the middle of the thing it was set for,
// which teaches people to set it again and again until they reach for a number so large it
// never expires.
const defaultOverrideTTL = time.Hour

// maxOverrideTTL is the longest an override may be asked for.
//
// It exists so that `limit 1 for 99999h` cannot become a permanent setting entered by accident.
// Anything genuinely permanent belongs in config (section 13.4), where it is written down and
// can be read back, rather than in the memory of a daemon nobody restarts.
const maxOverrideTTL = 24 * time.Hour

// clock is the time source, defaulting to the wall clock so that a zero override works.
func (o *override) clock() time.Time {
	if o.now == nil {
		return time.Now()
	}
	return o.now()
}

// overrideState is a snapshot of what is in force, for the status reply and the status line.
//
// It is a value rather than a view onto the struct so that a reader cannot hold the lock, and
// so that the two fields that must agree — whether a pin is on and when it ends — are read
// together. Reading them separately would let a pin expire between the two and report itself
// as on, ending at a time already past.
type overrideState struct {
	// Limit is the pinned budget in slots, or 0 when nothing is pinned.
	Limit       int
	LimitUntil  time.Time
	Paused      bool
	PausedUntil time.Time
}

// pinned reports whether a budget pin is in force.
func (s overrideState) pinned() bool { return s.Limit > 0 }

// any reports whether anything at all is overridden, which is what decides whether a surface
// needs to say so.
func (s overrideState) any() bool { return s.pinned() || s.Paused }

// clamp bounds a requested duration, and supplies the default for a request that names none.
func clampOverrideTTL(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultOverrideTTL
	}
	if d > maxOverrideTTL {
		return maxOverrideTTL
	}
	return d
}

// setLimit pins the budget at n slots for d, and returns the resulting state.
//
// n is floored at budget.Floor by the caller that reads it, not here: this type records what
// was asked for and the budget's own invariant is enforced where the budget is computed. Two
// places applying the same floor is how the two come to disagree about what "1" means.
func (o *override) setLimit(n int, d time.Duration) overrideState {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.limit = n
	o.limitUntil = o.clock().Add(clampOverrideTTL(d))
	return o.stateLocked()
}

// setPaused stops gating for d.
//
// Pausing does not stop the daemon, stop the lease table, or stop anything being recorded. It
// removes the cap and nothing else, so that `governor status` during a pause still reports what
// is genuinely running. A pause that also blinded the instruments would leave a person with no
// way to decide whether it was safe to end.
func (o *override) setPaused(d time.Duration) overrideState {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.paused = true
	o.pausedUntil = o.clock().Add(clampOverrideTTL(d))
	return o.stateLocked()
}

// release ends every override early, and reports whether there was one to end.
//
// The boolean is what lets `governor resume` tell "I have released your pin" from "there was
// nothing pinned", which are different answers to the same command and a person who has just
// typed it deserves to know which one they got.
func (o *override) release() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	had := o.stateLocked().any()
	o.limit, o.limitUntil = 0, time.Time{}
	o.paused, o.pausedUntil = false, time.Time{}
	return had
}

// state is what is in force right now, with anything expired already dropped.
func (o *override) state() overrideState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stateLocked()
}

// stateLocked expires lazily and returns the result.
//
// EXPIRY IS ON READ RATHER THAN ON A TIMER, and that is deliberate. A timer is a second thing
// that can fail — it can be missed, it can fire against a daemon that is shutting down, and it
// puts the correctness of the override on a goroutine nobody waits for. Expiring where the
// value is used means an override cannot be observed after its deadline by any path at all,
// including one added later that forgets a timer exists. The reaper sweeps too, but only so a
// log line lands at roughly the right moment; nothing depends on it.
func (o *override) stateLocked() overrideState {
	now := o.clock()
	if o.limit > 0 && !o.limitUntil.IsZero() && !now.Before(o.limitUntil) {
		o.limit, o.limitUntil = 0, time.Time{}
	}
	if o.paused && !o.pausedUntil.IsZero() && !now.Before(o.pausedUntil) {
		o.paused, o.pausedUntil = false, time.Time{}
	}
	return overrideState{
		Limit: o.limit, LimitUntil: o.limitUntil,
		Paused: o.paused, PausedUntil: o.pausedUntil,
	}
}
