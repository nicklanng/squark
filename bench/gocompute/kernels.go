package main

import (
	"fmt"
	"os"
)

// The Go version of bench/squark/kernels: one small loop run many times. Go doesn't vectorize
// loops, so this is the scalar baseline.

const kn = 16384

var (
	f32a [kn]float32
	f32b [kn]float32
	f64a [kn]float64
	i32a [kn]int32
	i32b [kn]int32
	u8a  [kn]byte
	u8b  [kn]byte
	u8c  [kn]byte
)

//go:noinline
func saxpy(a float32, x []float32, y []float32) {
	for i := range x {
		y[i] += a * x[i]
	}
}

//go:noinline
func dotInt32(a []int32, b []int32) int32 {
	var s int32
	for i := 0; i < len(a); i++ {
		s += a[i] * b[i]
	}
	return s
}

//go:noinline
func sumFloat64(x []float64) float64 {
	var s float64
	for _, v := range x {
		s += v
	}
	return s
}

//go:noinline
func countByte(b []byte, c byte) int {
	k := 0
	for i := range b {
		if b[i] == c {
			k++
		}
	}
	return k
}

//go:noinline
func maxInt32(x []int32) int32 {
	m := x[0]
	for i := 1; i < len(x); i++ {
		if x[i] > m {
			m = x[i]
		}
	}
	return m
}

//go:noinline
func xorBytes(dst []byte, a []byte, b []byte) {
	for i := range dst {
		dst[i] = a[i] ^ b[i]
	}
}

//go:noinline
func addRows(dst []int32, a []int32, b []int32, w int) {
	rows := len(dst) / w
	for r := 0; r < rows; r++ {
		base := r * w
		for j := 0; j < w; j++ {
			dst[base+j] = a[base+j] + b[base+j]
		}
	}
}

//go:noinline
func brighten(dst []byte, src []byte) {
	for i := range src {
		v := int(src[i]) * 3 / 2
		if v > 255 {
			v = 255
		}
		dst[i] = byte(v)
	}
}

func kernel(name string, reps int) {
	for i := 0; i < kn; i++ {
		f32a[i] = float32(i % 64)
		f64a[i] = float64(i % 64)
		i32a[i] = int32(i%1000 - 500)
		i32b[i] = int32(i%7 + 1)
		u8a[i] = byte(i * 7)
		u8b[i] = byte(i * 13)
	}
	var check int64
	switch name {
	case "saxpy":
		for r := 0; r < reps; r++ {
			saxpy(float32(1-2*(r&1)), f32a[:], f32b[:])
			check += int64(f32b[r%kn])
			f32a[r%kn] = float32(r % 64)
		}
	case "dot-int32":
		for r := 0; r < reps; r++ {
			check += int64(dotInt32(i32a[:], i32b[:]))
			i32a[r%kn] = int32(r%1000 - 500)
		}
	case "sum-float64":
		for r := 0; r < reps; r++ {
			check += int64(sumFloat64(f64a[:]))
			f64a[r%kn] = float64(r % 64)
		}
	case "count-byte":
		for r := 0; r < reps; r++ {
			check += int64(countByte(u8a[:], 7))
			u8a[r%kn] = byte(r)
		}
	case "max-int32":
		for r := 0; r < reps; r++ {
			check += int64(maxInt32(i32a[:]))
			i32a[r%kn] = int32(r % 1001)
		}
	case "xor-bytes":
		for r := 0; r < reps; r++ {
			xorBytes(u8c[:], u8a[:], u8b[:])
			check += int64(u8c[r%kn])
			u8a[r%kn] = byte(r)
		}
	case "add-rows":
		for r := 0; r < reps; r++ {
			addRows(i32b[:], i32a[:], i32b[:], 128)
			check += int64(i32b[r%kn])
			i32b[r%kn] = int32(r % 7)
		}
	case "brighten":
		for r := 0; r < reps; r++ {
			brighten(u8c[:], u8a[:])
			check += int64(u8c[r%kn])
			u8a[r%kn] = byte(r)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown kernel", name)
		os.Exit(2)
	}
	fmt.Println(check)
}
