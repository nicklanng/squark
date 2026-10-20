package driver

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// asanOptions makes AddressSanitizer catch what the lifetime checker exists to prevent: a reference
// used after its frame returned (detect_stack_use_after_return) or its block ended. The runtime
// installs its own (static) signal stack, so ASan must not manage one: it would munmap ours when a
// scheduler thread exits.
const asanOptions = "detect_stack_use_after_return=1:detect_stack_use_after_scope=1:use_sigaltstack=0:exitcode=86"

type TestOptions struct {
	Dir      string
	Run      *regexp.Regexp // nil: every test
	Verbose  bool           // report every test, with its output and proven stack
	Sanitize string         // "address": build with AddressSanitizer
	CPU      string
	Timeout  time.Duration // per test (0: one minute)
	Parallel int           // tests run at once (0: one)
	Out      io.Writer
}

// TestSummary counts a package's tests.
type TestSummary struct {
	Passed, Failed, Skipped int
}

// Test builds the tests of the package in opt.Dir and runs each in its own process, in the
// package's directory, reporting on opt.Out. The error is the build's; failed tests are counted.
func Test(opt TestOptions) (TestSummary, error) {
	var sum TestSummary
	if opt.Timeout == 0 {
		opt.Timeout = time.Minute
	}
	opt.Parallel = max(opt.Parallel, 1)
	start := time.Now()
	tmp, err := os.MkdirTemp("", "squark-test-")
	if err != nil {
		return sum, err
	}
	defer os.RemoveAll(tmp)
	abs := absDir(opt.Dir)
	bin := filepath.Join(tmp, filepath.Base(abs)+".test")
	res, err := Build(Options{Dir: opt.Dir, Output: bin, Test: true, Sanitize: opt.Sanitize, CPU: opt.CPU})
	if err != nil {
		return sum, err
	}
	var tests []TestReport
	for _, t := range res.Tests {
		if opt.Run == nil || opt.Run.MatchString(t.Name) {
			tests = append(tests, t)
		}
	}
	results := make([]chan testResult, len(tests))
	sem := make(chan struct{}, opt.Parallel)
	for i, t := range tests {
		results[i] = make(chan testResult, 1)
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] <- runTest(bin, abs, tmp, t, opt)
		}()
	}
	for i, t := range tests {
		r := <-results[i]
		status := "ok"
		switch {
		case r.skipped:
			status = "skip"
			sum.Skipped++
		case r.failure != "":
			status = "FAIL"
			sum.Failed++
		default:
			sum.Passed++
		}
		if !opt.Verbose && status != "FAIL" {
			continue
		}
		detail := duration(r.elapsed)
		if opt.Verbose && t.Stack > 0 {
			detail += ", stack " + human(t.Stack)
		}
		fmt.Fprintf(opt.Out, "--- %s %s (%s)\n", status, t.Name, detail)
		out := r.output
		if r.failure != "" && r.failure != "-" {
			out = append(out, r.failure+"\n"...)
		}
		if r.skipped {
			out = append(out, r.skip+"\n"...)
		}
		if len(out) > 0 {
			for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
				if line == "" {
					fmt.Fprintln(opt.Out)
				} else {
					fmt.Fprintf(opt.Out, "    %s\n", line)
				}
			}
		}
	}
	n := len(tests)
	var what string
	switch {
	case sum.Failed > 0:
		what = fmt.Sprintf("%d of %d failed", sum.Failed, n)
	case n == 1:
		what = "1 test"
	default:
		what = fmt.Sprintf("%d tests", n)
	}
	if sum.Skipped > 0 {
		what += fmt.Sprintf(", %d skipped", sum.Skipped)
	}
	mark := "ok  "
	if sum.Failed > 0 {
		mark = "FAIL"
	}
	if n == 0 {
		mark, what = "?   ", "no tests"
		if opt.Run != nil {
			what = "no tests match -run " + opt.Run.String()
		}
	}
	fmt.Fprintf(opt.Out, "%s %s  %s (%s)\n", mark, opt.Dir, what, duration(time.Since(start)))
	return sum, nil
}

type testResult struct {
	output  []byte
	elapsed time.Duration
	failure string // why it failed ("-": its output says), or ""
	skipped bool
	skip    string // the reason
}

// runTest runs one test: the binary with the test's name, in dir. The testing package's messages
// arrive on file descriptor 3.
func runTest(bin, dir, tmp string, t TestReport, opt TestOptions) testResult {
	var r testResult
	ctx, cancel := context.WithTimeout(context.Background(), opt.Timeout)
	defer cancel()
	ttmp, err := os.MkdirTemp(tmp, t.Name+"-")
	if err != nil {
		r.failure = err.Error()
		return r
	}
	defer os.RemoveAll(ttmp)
	pr, pw, err := os.Pipe()
	if err != nil {
		r.failure = err.Error()
		return r
	}
	defer pr.Close()
	cmd := exec.CommandContext(ctx, bin, t.Name)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "SQUARK_TEST=1", "TMPDIR="+ttmp)
	if opt.Sanitize == "address" {
		cmd.Env = append(cmd.Env, "ASAN_OPTIONS="+asanOptions)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.ExtraFiles = []*os.File{pw}
	cmd.WaitDelay = time.Second // a task it started may hold the output open
	msgs := make(chan []byte, 1)
	begin := time.Now()
	err = cmd.Start()
	pw.Close()
	if err != nil {
		r.failure = err.Error()
		return r
	}
	go func() {
		b, _ := io.ReadAll(pr)
		msgs <- b
	}()
	err = cmd.Wait()
	r.elapsed = time.Since(begin)
	r.output = out.Bytes()
	var expectPanic string
	expecting := false
	for _, m := range parseMessages(<-msgs) {
		switch m[0] {
		case "skip":
			r.skipped, r.skip = true, m[1]
		case "expect_panic":
			expecting, expectPanic = true, m[1]
		}
	}
	status := 0
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		r.failure = "timed out after " + opt.Timeout.String() + " (squark test -timeout)"
		return r
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			r.failure = "killed by signal " + ws.Signal().String()
			return r
		}
		status = ee.ExitCode()
	case err != nil:
		r.failure = err.Error()
		return r
	}
	if r.skipped && status == 0 {
		return r
	}
	r.skipped = false
	if expecting {
		i := bytes.LastIndex(r.output, []byte("panic: "))
		switch {
		case status == 2 && i >= 0 && bytes.Contains(r.output[i:], []byte(expectPanic)):
		case status == 2 && i >= 0:
			r.failure = fmt.Sprintf("panicked, but not with %q", expectPanic)
		default:
			r.failure = fmt.Sprintf("did not panic (expected a panic with %q)", expectPanic)
		}
		return r
	}
	switch status {
	case 0:
	case 86:
		if opt.Sanitize == "address" {
			r.failure = "-" // AddressSanitizer's report
			break
		}
		fallthrough
	default:
		r.failure = "-" // a panic or a returned error, in the output
		if !bytes.Contains(r.output, []byte("panic: ")) && !bytes.HasPrefix(r.output, []byte("error: ")) && !bytes.Contains(r.output, []byte("\nerror: ")) {
			r.failure = "exit status " + strconv.Itoa(status)
		}
	}
	return r
}

// parseMessages splits the testing package's "kind len\ntext\n" messages.
func parseMessages(b []byte) [][2]string {
	var out [][2]string
	rd := bufio.NewReader(bytes.NewReader(b))
	for {
		head, err := rd.ReadString('\n')
		if err != nil {
			return out
		}
		kind, n, _ := strings.Cut(strings.TrimSuffix(head, "\n"), " ")
		size, err := strconv.Atoi(n)
		if err != nil || size < 0 || size > len(b) {
			return out
		}
		text := make([]byte, size+1)
		if _, err := io.ReadFull(rd, text); err != nil {
			return out
		}
		out = append(out, [2]string{kind, string(text[:size])})
	}
}

// duration prints d in the unit that suits it: 0.4ms, 12ms, 1.25s.
func duration(d time.Duration) string {
	switch {
	case d < 10*time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

// TestDirs expands a list of directories, where dir/... is every directory under dir (itself
// included) that holds a _test.sq file, skipping testdata and hidden directories.
func TestDirs(args []string) ([]string, error) {
	var dirs []string
	for _, a := range args {
		root, ok := strings.CutSuffix(a, "/...")
		if !ok && a != "..." {
			dirs = append(dirs, a)
			continue
		}
		if a == "..." {
			root = "."
		}
		var found []string
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if n := d.Name(); p != root && (n == "testdata" || n == "node_modules" || strings.HasPrefix(n, ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(p, "_test.sq") {
				if dir := filepath.Dir(p); len(found) == 0 || found[len(found)-1] != dir {
					found = append(found, dir)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Strings(found)
		dirs = append(dirs, found...)
	}
	return dirs, nil
}
