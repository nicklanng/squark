// Command squark compiles squark programs: a Go-like language that never uses the heap.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"squark/internal/driver"
)

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  squark build [-o output] [-v] [-work dir] [dir]   compile a program
  squark run [-v] dir [args...]                     compile and run
  squark check [dir]                                type and lifetime check only
  squark doc [dir] [pkg]                            list functions with their reference provenance
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
	opt := fs.String("opt", "-O3", "C optimization flag")
	fs.Parse(os.Args[2:])
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	// flags must come before the directory; only run (program arguments) and doc (package) take more
	maxArgs := map[string]int{"build": 1, "check": 1, "emit-c": 1, "doc": 2}
	if n, ok := maxArgs[cmd]; ok && fs.NArg() > n {
		fmt.Fprintf(os.Stderr, "squark %s: unexpected arguments %q (flags go before the directory)\n", cmd, fs.Args()[n:])
		os.Exit(2)
	}
	switch cmd {
	case "build":
		res, err := driver.Build(driver.Options{Dir: dir, Output: *output, WorkDir: *work, Verbose: *verbose, OptFlag: *opt})
		if err != nil && res != nil {
			// budget exceeded: the binary was built but is rejected
			os.Remove(res.Binary)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if *verbose || res.Memory.Budget > 0 {
			fmt.Fprint(os.Stderr, driver.FormatMemory(res.Memory))
		}
	case "run":
		tmp, err := os.MkdirTemp("", "squark-run-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		abs, _ := filepath.Abs(dir)
		bin := filepath.Join(tmp, filepath.Base(abs))
		res, err := driver.Build(driver.Options{Dir: dir, Output: bin, WorkDir: *work, OptFlag: *opt})
		if err != nil {
			os.RemoveAll(tmp)
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if *verbose {
			fmt.Fprint(os.Stderr, driver.FormatMemory(res.Memory))
		}
		c := exec.Command(bin, fs.Args()[min(1, fs.NArg()):]...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		// pass interrupts on to the program and outlive it, so the temporary binary is removed
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
		if err = c.Start(); err == nil {
			go func() {
				for s := range sigs {
					c.Process.Signal(s)
				}
			}()
			err = c.Wait()
		}
		signal.Stop(sigs)
		os.RemoveAll(tmp)
		if ee, ok := err.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				os.Exit(128 + int(ws.Signal())) // as a shell reports it
			}
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
	case "doc":
		pkg := ""
		if fs.NArg() > 1 {
			pkg = fs.Arg(1)
		}
		out, err := driver.Doc(dir, pkg)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print(out)
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
