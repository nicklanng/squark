// Command squark compiles squark programs: a Go-like language that never uses the heap.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"squark/internal/driver"
)

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  squark build [-o output] [-v] [-work dir] [dir]   compile a program
  squark run [-v] dir [args...]                     compile and run
  squark check [dir]                                type and lifetime check only
  squark emit-c [dir]                               print the generated C
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	output := fs.String("o", "", "output binary")
	verbose := fs.Bool("v", false, "print goroutine pool and stack report")
	work := fs.String("work", "", "keep intermediate C files in this directory")
	opt := fs.String("opt", "-O2", "C optimization flag")
	fs.Parse(os.Args[2:])
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	switch cmd {
	case "build":
		res, err := driver.Build(driver.Options{Dir: dir, Output: *output, WorkDir: *work, Verbose: *verbose, OptFlag: *opt})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if *verbose {
			fmt.Fprint(os.Stderr, driver.FormatPools(res.Pools))
		}
	case "run":
		tmp, err := os.MkdirTemp("", "squark-run-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		bin := filepath.Join(tmp, filepath.Base(dir))
		res, err := driver.Build(driver.Options{Dir: dir, Output: bin, WorkDir: *work, OptFlag: *opt})
		if err != nil {
			os.RemoveAll(tmp)
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if *verbose {
			fmt.Fprint(os.Stderr, driver.FormatPools(res.Pools))
		}
		c := exec.Command(bin, fs.Args()[min(1, fs.NArg()):]...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		err = c.Run()
		os.RemoveAll(tmp)
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		} else if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "check":
		if err := driver.Check(dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "emit-c":
		src, err := driver.EmitC(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print(src)
	default:
		usage()
	}
}
