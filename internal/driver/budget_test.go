package driver

import (
	"math"
	"testing"
)

// TestWorstSaturates: a worst case too large for int64 saturates instead of wrapping (a wrapped
// product once made 2^52 goroutines × 16 KiB report 0 B and pass a 64M budget).
func TestWorstSaturates(t *testing.T) {
	m := &MemoryReport{Budget: 64 << 20, Workers: 1024, Threads: 1024, Static: 1 << 40,
		Pools: []PoolReport{{Name: "t", Count: 1 << 24, Touchable: 1 << 40}}}
	if w := m.Worst(); w != math.MaxInt64 {
		t.Errorf("Worst() = %d, want MaxInt64", w)
	}
	small := &MemoryReport{Code: 100, Static: 10, Pools: []PoolReport{{Count: 2, Touchable: 5}}}
	if w := small.Worst(); w != 120 {
		t.Errorf("Worst() = %d, want 120", w)
	}
}
