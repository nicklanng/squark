package main

import (
	"math"
	"testing"
	"time"
)

func TestHistBuckets(t *testing.T) {
	prev := -1
	for v := uint64(0); v < 1<<20; v++ {
		b := bucketOf(v)
		if b < prev || b > prev+1 {
			t.Fatalf("bucketOf(%d)=%d not contiguous after %d", v, b, prev)
		}
		prev = b
		mid := bucketMid(b)
		if v > 0 && math.Abs(float64(mid)-float64(v))/float64(v) > 0.008 {
			t.Fatalf("bucketMid(bucketOf(%d))=%d: error too large", v, mid)
		}
	}
	if b := bucketOf(math.MaxUint64); b >= histBucket {
		t.Fatalf("max bucket %d out of range %d", b, histBucket)
	}
}

func TestHistQuantile(t *testing.T) {
	var h hist
	for i := 1; i <= 1000; i++ {
		h.record(time.Duration(i) * time.Microsecond)
	}
	for _, c := range []struct {
		q    float64
		want time.Duration
	}{{0.5, 500 * time.Microsecond}, {0.9, 900 * time.Microsecond}, {0.99, 990 * time.Microsecond}} {
		got := h.quantile(c.q)
		if math.Abs(float64(got-c.want))/float64(c.want) > 0.01 {
			t.Errorf("q%.2f = %v, want ~%v", c.q, got, c.want)
		}
	}
	if h.max != uint64(time.Millisecond) {
		t.Errorf("max = %d", h.max)
	}
}
