// Command squark compiles squark programs: a Go-like language that never uses the heap.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

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

// runSignals are the signals that would end squark run. It handles them instead, so that it
// outlives the program and removes its temporary files.
var runSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGALRM, syscall.SIGUSR1, syscall.SIGUSR2}

// signalRelay passes runSignals on to the program once it runs. Before that (during the build),
// one removes the temporary directory and ends squark with 128+n, as a shell reports a signal.
type signalRelay struct {
	mu    sync.Mutex
	child *os.Process
}

func relaySignals(tmp string) *signalRelay {
	r := &signalRelay{}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, runSignals...)
	go func() {
		for s := range sigs {
			r.mu.Lock()
			p := r.child
			r.mu.Unlock()
			if p != nil {
				p.Signal(s)
				continue
			}
			// the compiler may still be writing into it: retry until it is gone (nothing recreates
			// it once removed)
			for i := 0; i < 50 && os.RemoveAll(tmp) != nil; i++ {
				time.Sleep(20 * time.Millisecond)
			}
			os.Exit(128 + int(s.(syscall.Signal)))
		}
	}()
	return r
}

// start starts c; signals from then on go to it.
func (r *signalRelay) start(c *exec.Cmd) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := c.Start(); err != nil {
		return err
	}
	r.child = c.Process
	return nil
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
		fmt.Fprint(os.Stderr, res.Notes)
		if *verbose || res.Memory.Budget > 0 {
			fmt.Fprint(os.Stderr, driver.FormatMemory(res.Memory))
		}
	case "run":
		tmp, err := os.MkdirTemp("", "squark-run-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		relay := relaySignals(tmp)
		abs, _ := filepath.Abs(dir)
		bin := filepath.Join(tmp, filepath.Base(abs))
		// the build's own temporary directory goes inside tmp, so a signal removes it too
		userTmp, hadTmp := os.LookupEnv("TMPDIR")
		os.Setenv("TMPDIR", tmp)
		res, err := driver.Build(driver.Options{Dir: dir, Output: bin, WorkDir: *work, OptFlag: *opt})
		if hadTmp {
			os.Setenv("TMPDIR", userTmp)
		} else {
			os.Unsetenv("TMPDIR")
		}
		if err != nil {
			os.RemoveAll(tmp)
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprint(os.Stderr, res.Notes)
		if *verbose {
			fmt.Fprint(os.Stderr, driver.FormatMemory(res.Memory))
		}
		c := exec.Command(bin, fs.Args()[min(1, fs.NArg()):]...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err = relay.start(c); err == nil {
			err = c.Wait()
		}
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
		notes, err := driver.CheckNotes(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprint(os.Stderr, notes)
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
