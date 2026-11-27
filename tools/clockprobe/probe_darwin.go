//go:build darwin && cgo

package main

/*
#include <pthread.h>
#include <pthread/qos.h>
#include <stdint.h>
#include <time.h>

// The kernels are C, not Go, so what they execute is fixed by clang at -O2 and not by
// whichever Go compiler built the tool. Vectorisation, interleaving and unrolling are off
// explicitly: a NEON version measures the vector unit, and an unrolled one changes the
// per-iteration cycle count the clock reading divides by.

// probeChain is n strictly dependent integer multiply-adds. Each iteration waits on the last,
// and on both M1 core types one iteration takes 3 cycles (measured: an E-core at its 2064 MHz
// cap reads 1.45 ns an iteration, a warmed P-core reads 0.97 ns against powermetrics' 3.1 GHz).
// So its time IS the clock: MHz = 3000 / (ns per iteration).
static uint64_t probeChain(uint64_t n, uint64_t x) {
#pragma clang loop vectorize(disable) interleave(disable) unroll(disable)
	for (uint64_t i = 0; i < n; i++) {
		x = x * 0x9E3779B97F4A7C15ull + i;
	}
	return x;
}

// probeWide is n independent double-precision adds over sixteen lanes, enough to fill every FP
// pipe. A P-core (Firestorm) has four, an E-core (Icestorm) two, so in units of its own clock a
// P-core finishes in half the time. The ratio wide/chain therefore says which kind of core ran
// the pair, whatever either was clocked at: 0.084 on a P-core and 0.167 on an E-core, measured.
static double probeWide(uint64_t n, double s) {
	double a0 = s, a1 = s + 1, a2 = s + 2, a3 = s + 3, a4 = s + 4, a5 = s + 5, a6 = s + 6, a7 = s + 7;
	double a8 = s + 8, a9 = s + 9, a10 = s + 10, a11 = s + 11, a12 = s + 12, a13 = s + 13, a14 = s + 14, a15 = s + 15;
	const double k = 1.0000001;
#pragma clang loop vectorize(disable) interleave(disable) unroll(disable)
	for (uint64_t i = 0; i < n / 16; i++) {
		a0 += k; a1 += k; a2 += k; a3 += k; a4 += k; a5 += k; a6 += k; a7 += k;
		a8 += k; a9 += k; a10 += k; a11 += k; a12 += k; a13 += k; a14 += k; a15 += k;
	}
	return a0 + a1 + a2 + a3 + a4 + a5 + a6 + a7 + a8 + a9 + a10 + a11 + a12 + a13 + a14 + a15;
}

static int64_t threadNanos(void) {
	struct timespec ts;
	if (clock_gettime(CLOCK_THREAD_CPUTIME_ID, &ts) != 0) {
		return -1;
	}
	return (int64_t)ts.tv_sec * 1000000000 + ts.tv_nsec;
}

static volatile uint64_t probeSink;
static volatile double probeDSink;

// probePair times the chain and then the wide block back to back, in thread CPU time, so the
// two are as likely as possible to have run on the same core. Thread CPU time and not wall
// time: time preempted by the load does not count.
static int probePair(uint64_t chainN, uint64_t wideN, int64_t *chainNs, int64_t *wideNs) {
	int64_t t0 = threadNanos();
	probeSink = probeChain(chainN, (uint64_t)t0);
	int64_t t1 = threadNanos();
	probeDSink = probeWide(wideN, (double)(t1 & 0xff));
	int64_t t2 = threadNanos();
	if (t0 < 0 || t1 < 0 || t2 < 0) {
		return -1;
	}
	*chainNs = t1 - t0;
	*wideNs = t2 - t1;
	return 0;
}

// probeSpin burns about ns of thread CPU time. Measured: a thread that wakes once a second for
// a millisecond is left on an E-core even at user-interactive QoS; two milliseconds of work
// first gets it moved to a P-core before the pair is timed.
static void probeSpin(int64_t ns) {
	int64_t t0 = threadNanos();
	while (threadNanos() - t0 < ns) {
		probeSink = probeChain(4096, probeSink);
	}
}

// probeSetQoS moves the calling thread to a QoS class: 1 user-interactive, 0 background. On
// Apple silicon a background thread runs only on the E-cluster, which calibration relies on.
static int probeSetQoS(int interactive) {
	return pthread_set_qos_class_self_np(interactive ? QOS_CLASS_USER_INTERACTIVE : QOS_CLASS_BACKGROUND, 0);
}
*/
import "C"

import (
	"errors"
	"time"
)

const supported = true

func timePair(chainN, wideN uint64) (chainNs, wideNs int64, err error) {
	var c, w C.int64_t
	if C.probePair(C.uint64_t(chainN), C.uint64_t(wideN), &c, &w) != 0 || c <= 0 || w <= 0 {
		return 0, 0, errors.New("clock_gettime(CLOCK_THREAD_CPUTIME_ID) failed")
	}
	return int64(c), int64(w), nil
}

func spin(d time.Duration) { C.probeSpin(C.int64_t(d.Nanoseconds())) }

func setQoS(interactive bool) error {
	i := C.int(0)
	if interactive {
		i = 1
	}
	if C.probeSetQoS(i) != 0 {
		return errors.New("pthread_set_qos_class_self_np failed")
	}
	return nil
}
