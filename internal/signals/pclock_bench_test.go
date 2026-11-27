package signals

import "testing"

// BenchmarkPClockNext is the per-tick cost of the P-cluster reading alone, on the real
// source where the platform has one.
func BenchmarkPClockNext(b *testing.B) {
	p := newPClock(newPClockSource())
	p.next()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.next()
	}
}
