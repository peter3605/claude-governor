Prediction recorded 2026-09-15 22:57, BEFORE the candidate arm reported.

Baseline (measured this session, RunQueueMax=2.0, CPU cut live):
  refusals 17, build core-sec 144.6, rq p50 13.58, p90 18.93, drain 92.9s
  budget floored to 1 immediately, never rose (daemon log: peak in_use=1/1 only)
Ungoverned reference (same session): rq p50 15.46, p90 26.60, drain 70.4s, 213.9 core-sec

Candidate (run queue decides both directions, cut 2.0 / climb 1.5):
  1. The daemon log MUST show the budget taking more than one distinct value.
     If it only ever shows /1 again, the fix did not take and nothing else matters.
  2. Fewer refusals than 17 -- the budget spends time above the floor.
  3. Drain time at or below the baseline's 92.9s, and still above ungoverned's 70.4s.
  4. rq p50 above the baseline's 13.58 and below ungoverned's 15.46.
  5. Climb-condition share well above the baseline's 4% -- this is the point of the change.

Falsifiers: if rq p50 lands at or above ungoverned, the loop has stopped governing and the
change must be reverted regardless of what the throughput columns say. If refusals and the
budget trajectory are unchanged, the CPU cut was not what was pinning it and the diagnosis in
section 3 is wrong.
