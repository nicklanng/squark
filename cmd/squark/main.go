// Command squark compiles squark programs: a language that never uses the heap.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"squark"
	"squark/internal/compiler"
	"squark/internal/driver"
	"squark/internal/lsp"
	"squark/internal/tour"
)

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  squark build [-o output] [-v] [-work dir] [-cpu c] [-static] [-manifest k8s|systemd] [dir]   compile a program
  squark run [-v] [-cpu c] dir [args...]                     compile and run
  squark test [-run re] [-v] [-asan] [-timeout d] [-p n] [-cpu c] [dir|dir/... ...]   run the tests in _test.sq files
  squark fmt [-l] [-w] [file|dir|dir/... ...]       format source files (print, list or rewrite)
  squark lsp                                        the language server, for editors (stdin and stdout)
  squark check [dir]                                type and lifetime check only
  squark doc [dir] [pkg]                            list functions with their reference provenance
  squark emit-c [dir]                               print the generated C
  squark tour [-http addr] [-open=false] [-export dir]   the squark tour, in your browser
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
	verbose := fs.Bool("v", false, "print task pool and stack report")
	work := fs.String("work", "", "keep intermediate C files in this directory")
	opt := fs.String("opt", "-O3", "C optimization flag")
	cpu := fs.String("cpu", "", "CPU to build for: baseline, native, x86-64-v3, ... (default $SQUARK_CPU, or baseline)")
	manifest := fs.String("manifest", "", "build: also print resource settings that fit the program: k8s or systemd")
	static := fs.Bool("static", false, "link statically, C libraries included, for an empty container image (Linux)")
	httpAddr := fs.String("http", "127.0.0.1:3999", "tour: the address to serve on")
	openBrowser := fs.Bool("open", true, "tour: open the tour in a browser")
	export := fs.String("export", "", "tour: write a static copy of the tour, with every lesson's output baked in, into this directory")
	run := fs.String("run", "", "test: run only the tests whose names match this regular expression")
	asan := fs.Bool("asan", false, "test: build with AddressSanitizer")
	timeout := fs.Duration("timeout", time.Minute, "test: fail a test that runs longer than this")
	parallel := fs.Int("p", 1, "test: run this many tests at once")
	list := fs.Bool("l", false, "fmt: list the files whose formatting differs")
	write := fs.Bool("w", false, "fmt: rewrite the files in place")
	fs.Parse(os.Args[2:])
	// each subcommand takes its own flags: one it ignores would be a silent surprise
	takes := map[string]string{"build": "o v work opt cpu manifest static", "run": "v work opt cpu", "tour": "http open export", "test": "run v asan timeout p cpu", "fmt": "l w"}
	fs.Visit(func(f *flag.Flag) {
		if !slices.Contains(strings.Fields(takes[cmd]), f.Name) {
			fmt.Fprintf(os.Stderr, "squark %s: -%s doesn't apply to %s\n", cmd, f.Name, cmd)
			os.Exit(2)
		}
	})
	if *manifest != "" && *manifest != "k8s" && *manifest != "systemd" {
		fmt.Fprintf(os.Stderr, "squark build: -manifest %s: want k8s or systemd\n", *manifest)
		os.Exit(2)
	}
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	// flags must come before the directory; only run (program arguments) and doc (package) take more
	maxArgs := map[string]int{"build": 1, "check": 1, "lsp": 0, "emit-c": 1, "doc": 2, "tour": 0}
	if n, ok := maxArgs[cmd]; ok && fs.NArg() > n {
		fmt.Fprintf(os.Stderr, "squark %s: unexpected arguments %q (flags go before the directory)\n", cmd, fs.Args()[n:])
		os.Exit(2)
	}
	switch cmd {
	case "build":
		res, err := driver.Build(driver.Options{Dir: dir, Output: *output, WorkDir: *work, Verbose: *verbose, OptFlag: *opt, CPU: *cpu, Static: *static})
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
		if *manifest != "" {
			m, err := driver.Manifest(res, filepath.Base(res.Binary), *manifest)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Print(m)
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
		res, err := driver.Build(driver.Options{Dir: dir, Output: bin, WorkDir: *work, OptFlag: *opt, CPU: *cpu})
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
	case "lsp":
		if err := lsp.Serve(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "squark lsp:", err)
			os.Exit(1)
		}
	case "fmt":
		os.Exit(format(fs.Args(), *list, *write))
	case "test":
		os.Exit(test(fs.Args(), *run, *verbose, *asan, *timeout, *parallel, *cpu))
	case "tour":
		if err := serveTour(*httpAddr, *openBrowser, *export); err != nil {
			fmt.Fprintln(os.Stderr, "squark tour:", err)
			os.Exit(1)
		}
	default:
		usage()
	}
}

// format formats the files that paths name; without -l or -w it prints them. It returns the exit
// status: 1 if a file doesn't parse.
func format(paths []string, list, write bool) int {
	if len(paths) == 0 {
		paths = []string{"."}
	}
	files, err := driver.SourceFiles(paths)
	if err != nil {
		fmt.Fprintln(os.Stderr, "squark fmt:", err)
		return 1
	}
	status := 0
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, "squark fmt:", err)
			status = 1
			continue
		}
		out, err := compiler.Format(f, src)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			status = 1
			continue
		}
		changed := !bytes.Equal(src, out)
		if list && changed {
			fmt.Println(f)
		}
		if write && changed {
			if err := os.WriteFile(f, out, 0o644); err != nil {
				fmt.Fprintln(os.Stderr, "squark fmt:", err)
				status = 1
			}
		}
		if !list && !write {
			os.Stdout.Write(out)
		}
	}
	return status
}

// test runs the tests of each directory, and returns the exit status: 1 if any failed or didn't
// build.
func test(args []string, run string, verbose, asan bool, timeout time.Duration, parallel int, cpu string) int {
	var re *regexp.Regexp
	if run != "" {
		var err error
		if re, err = regexp.Compile(run); err != nil {
			fmt.Fprintf(os.Stderr, "squark test: -run: %v\n", err)
			return 2
		}
	}
	if len(args) == 0 {
		args = []string{"."}
	}
	dirs, err := driver.TestDirs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "squark test:", err)
		return 1
	}
	if len(dirs) == 0 {
		fmt.Fprintf(os.Stderr, "squark test: no _test.sq files in %s\n", strings.Join(args, " "))
		return 1
	}
	opt := driver.TestOptions{Run: re, Verbose: verbose, Timeout: timeout, Parallel: parallel, CPU: cpu, Out: os.Stdout}
	if asan {
		opt.Sanitize = "address"
	}
	status := 0
	for _, d := range dirs {
		opt.Dir = d
		sum, err := driver.Test(opt)
		if err != nil {
			fmt.Printf("FAIL %s  build failed\n", d)
			for _, line := range strings.Split(strings.TrimRight(err.Error(), "\n"), "\n") {
				fmt.Printf("    %s\n", line)
			}
			status = 1
		} else if sum.Failed > 0 {
			status = 1
		}
	}
	return status
}

func serveTour(addr string, openBrowser bool, export string) error {
	site, err := tour.New(mustSub(squark.Tour, "tour"))
	if err != nil {
		return err
	}
	if export != "" {
		return site.Export(export, os.Stderr)
	}
	// the tour compiles and runs the code it is sent: it serves this machine only
	if host, _, err := net.SplitHostPort(addr); err != nil || host != "localhost" {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("-http %s: the tour runs the code it is sent, so it serves only on a loopback address (127.0.0.1:3999, localhost:3999)", addr)
		}
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	var k [16]byte
	if _, err := rand.Read(k[:]); err != nil {
		return err
	}
	key := hex.EncodeToString(k[:]) // another user on this machine can reach the port, not the key
	url := "http://" + l.Addr().String() + "/?k=" + key
	fmt.Fprintln(os.Stderr, "squark tour: serving on", url)
	if openBrowser {
		opener := "xdg-open"
		if runtime.GOOS == "darwin" {
			opener = "open"
		}
		exec.Command(opener, url).Start()
	}
	return http.Serve(l, site.Handler(key))
}

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
