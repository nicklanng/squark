package main

import (
	"math/bits"
	"time"
)

// hist is a small HDR-style log-linear histogram of nanosecond latencies.
// Each power-of-two range is split into 128 linear sub-buckets, so any
// recorded value is reported with < 0.8% relative error.
const (
	subBits    = 7
	subCount   = 1 << subBits
	histBucket = (64-subBits)*subCount + subCount
)

type hist struct {
	counts [histBucket]uint64
	n      uint64
	sum    uint64
	max    uint64
	min    uint64
}

func bucketOf(v uint64) int {
	if v < 2*subCount {
		return int(v)
	}
	exp := bits.Len64(v) - (subBits + 1)
	return exp*subCount + int(v>>uint(exp))
}

// bucketMid returns a representative value (midpoint) for bucket i.
func bucketMid(i int) uint64 {
	if i < 2*subCount {
		return uint64(i)
	}
	exp := i/subCount - 1
	mant := uint64(i - exp*subCount)
	lo := mant << uint(exp)
	return lo + (uint64(1)<<uint(exp))/2
}

func (h *hist) record(d time.Duration) {
	v := uint64(d)
	h.counts[bucketOf(v)]++
	h.n++
	h.sum += v
	if v > h.max {
		h.max = v
	}
	if h.min == 0 || v < h.min {
		h.min = v
	}
}

func (h *hist) merge(o *hist) {
	for i, c := range o.counts {
		h.counts[i] += c
	}
	h.n += o.n
	h.sum += o.sum
	if o.max > h.max {
		h.max = o.max
	}
	if o.min != 0 && (h.min == 0 || o.min < h.min) {
		h.min = o.min
	}
}

func (h *hist) quantile(q float64) time.Duration {
	if h.n == 0 {
		return 0
	}
	target := uint64(q*float64(h.n) + 0.5)
	if target < 1 {
		target = 1
	}
	var seen uint64
	for i, c := range h.counts {
		seen += c
		if seen >= target {
			v := bucketMid(i)
			if v > h.max {
				v = h.max
			}
			return time.Duration(v)
		}
	}
	return time.Duration(h.max)
}

func (h *hist) mean() time.Duration {
	if h.n == 0 {
		return 0
	}
	return time.Duration(h.sum / h.n)
}
