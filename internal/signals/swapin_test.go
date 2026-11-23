package signals

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// The two shipped platforms must answer the swap-in counter. Before GOV-B53 darwin did not —
// gopsutil leaves Sin at zero on macOS — and the failure was invisible because a zero rate
// reads exactly like an unpressured machine. This test is what makes that silence a failure.
func TestSwapInIsReadableOnShippedPlatforms(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("swap-in is read on darwin and linux only")
	}
	if _, ok := swapInBytes(context.Background()); !ok {
		t.Fatalf("swapInBytes reported unreadable on %s; the memory cut would never fire here", runtime.GOOS)
	}
	s := newSamplerNamed(selfRootName(t))
	if _, known := s.swapRate(context.Background(), time.Now()); !known {
		t.Errorf("swapRate's priming tick reported unknown on %s; a read counter is a known zero", runtime.GOOS)
	}
}
