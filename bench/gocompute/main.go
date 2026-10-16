// gocompute holds the Go versions of the small compute benchmarks that the
// squark versions mirror.
//
//	gocompute sieve [N]          primes below N (default 50_000_000)
//	gocompute nbody [N]          n-body, N steps (default 50_000_000)
//	gocompute wordfreq FILE      top 10 word frequencies
//	gocompute json [N]           encode a struct N times (default 2_000_000)
//	gocompute kernel NAME REPS   run one loop kernel REPS times (see kernels.go)
//	gocompute gen-text FILE MB   write deterministic text input for wordfreq
package main

import (
	"fmt"
	"os"
	"strconv"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  gocompute sieve [N]          count primes below N (default 50000000)
  gocompute nbody [N]          n-body simulation, N steps (default 50000000)
  gocompute wordfreq FILE      print top 10 words as "count word"
  gocompute json [N]           JSON-encode a struct N times (default 2000000)
  gocompute kernel NAME REPS   run one loop kernel REPS times (see kernels.go)
  gocompute gen-text FILE MB   write MB MiB of deterministic text`)
	os.Exit(2)
}

func intArg(args []string, i int, def int) int {
	if len(args) <= i {
		return def
	}
	n, err := strconv.Atoi(args[i])
	if err != nil || n < 0 {
		fmt.Fprintf(os.Stderr, "bad number %q\n", args[i])
		os.Exit(2)
	}
	return n
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "sieve":
		fmt.Println(sieve(intArg(args, 0, 50_000_000)))
	case "nbody":
		nbody(intArg(args, 0, 50_000_000))
	case "wordfreq":
		if len(args) != 1 {
			usage()
		}
		err = wordfreq(args[0])
	case "json":
		fmt.Println(jsonBench(intArg(args, 0, 2_000_000)))
	case "kernel":
		if len(args) != 2 {
			usage()
		}
		kernel(args[0], intArg(args, 1, 0))
	case "gen-text":
		if len(args) != 2 {
			usage()
		}
		err = genText(args[0], intArg(args, 1, 0))
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
