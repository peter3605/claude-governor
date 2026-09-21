package main

import (
	"testing"
	"time"
)

// A clock the test drives by hand, so expiry is asserted rather than slept for.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestOverride() (*override, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)}
	return &override{now: c.now}, c
}

// THE STORY'S TRAP, AS A TEST. A pinned limit that outlives its reason is the failure GOV-B01
// was filed about, so the pin ending on its own is the first thing proved about it — before
// anything about what it does while it holds.
func TestAPinnedLimitExpiresOnItsOwn(t *testing.T) {
	o, clock := newTestOverride()

	if s := o.setLimit(2, time.Hour); !s.pinned() || s.Limit != 2 {
		t.Fatalf("pin did not take: %+v", s)
	}

	clock.add(59 * time.Minute)
	if s := o.state(); !s.pinned() {
		t.Error("pin expired early, before the duration it was given")
	}

	clock.add(2 * time.Minute)
	if s := o.state(); s.pinned() {
		t.Errorf("pin outlived its deadline and is still in force: %+v", s)
	}
}

func TestAPauseExpiresOnItsOwn(t *testing.T) {
	o, clock := newTestOverride()
	o.setPaused(30 * time.Minute)

	if !o.state().Paused {
		t.Fatal("pause did not take")
	}
	clock.add(31 * time.Minute)
	if s := o.state(); s.Paused {
		t.Errorf("gating is still paused past its deadline: %+v", s)
	}
}

// An override with no duration still has one. The default is not "forever".
func TestAnOverrideWithNoDurationStillExpires(t *testing.T) {
	o, clock := newTestOverride()
	o.setLimit(3, 0)
	o.setPaused(0)

	if s := o.state(); !s.pinned() || !s.Paused {
		t.Fatalf("overrides did not take: %+v", s)
	}
	clock.add(defaultOverrideTTL + time.Minute)
	if s := o.state(); s.any() {
		t.Errorf("an override given no duration never expired: %+v", s)
	}
}

// A duration nobody could have meant is clamped rather than honoured.
//
// `limit 1 for 99999h` is how a manual override becomes a permanent setting by accident, which
// is the same failure as never expiring, entered by a different door.
func TestAnAbsurdDurationIsClamped(t *testing.T) {
	o, clock := newTestOverride()
	o.setLimit(1, 99999*time.Hour)

	clock.add(maxOverrideTTL + time.Minute)
	if s := o.state(); s.pinned() {
		t.Errorf("a pin asked for 99999h outlived the cap: %+v", s)
	}
}

// release reports whether there was anything to release, because "released" and "there was
// nothing set" are different answers to `governor resume` and the person typing it is asking
// which one happened.
func TestReleaseSaysWhetherThereWasAnythingToRelease(t *testing.T) {
	o, _ := newTestOverride()

	if o.release() {
		t.Error("reported releasing an override on a daemon that had none")
	}
	o.setLimit(4, time.Hour)
	if !o.release() {
		t.Error("released a pin but reported there had been nothing to release")
	}
	if s := o.state(); s.any() {
		t.Errorf("release left something in force: %+v", s)
	}
}

// Releasing clears BOTH, because `resume` is the one word a person reaches for when they want
// the governor back to normal, and leaving a pause behind after "resume" would be the surface
// disagreeing with the word on it.
func TestReleaseClearsAPinAndAPauseTogether(t *testing.T) {
	o, _ := newTestOverride()
	o.setLimit(2, time.Hour)
	o.setPaused(time.Hour)

	o.release()
	if s := o.state(); s.pinned() || s.Paused {
		t.Errorf("resume left an override in force: %+v", s)
	}
}

// Expiry is observable on EVERY read path, not only through a sweep, because nothing may see
// an override after its deadline — including a path added later that does not know a sweeper
// exists.
func TestAnExpiredOverrideIsNeverObservable(t *testing.T) {
	o, clock := newTestOverride()
	o.setLimit(2, time.Minute)
	clock.add(2 * time.Minute)

	if s := o.state(); s.pinned() || s.Limit != 0 {
		t.Errorf("state() reported an expired pin: %+v", s)
	}
	// And a second read does not resurrect it.
	if s := o.state(); s.any() {
		t.Errorf("a second read brought an expired override back: %+v", s)
	}
}
