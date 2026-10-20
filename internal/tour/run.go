package tour

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"squark/internal/driver"
)

// A Result is what compiling, and perhaps running, a lesson's code did.
type Result struct {
	// Stage is how far it got: "compile" (rejected by the compiler, or over budget when Memory
	// is set), "checked" (Check found no errors), "built" (mode build) or "run".
	Stage    string  `json:"stage"`
	Errors   []Diag  `json:"errors,omitempty"`
	Output   string  `json:"output"` // the compiler's message, or the program's stdout and stderr
	Exit     int     `json:"exit"`
	TimedOut bool    `json:"timedOut,omitempty"`
	Memory   *Memory `json:"memory,omitempty"`
	BuildMS  int64   `json:"buildMs"`
	RunMS    int64   `json:"runMs"`
}

// A Diag is one compiler error in the lesson's file.
type Diag struct {
	Line int    `json:"line"`
	Col  int    `json:"col"`
	Msg  string `json:"msg"`
}

// Memory is the worst-case memory report, as rows for the memory map.
type Memory struct {
	Worst     int64    `json:"worst"`
	Budget    int64    `json:"budget"`
	Threads   int64    `json:"threads"` // 0: one per core
	Workers   int64    `json:"workers"`
	Rows      []MemRow `json:"rows"`
	Uncounted []string `json:"uncounted,omitempty"`
	Report    string   `json:"report"` // as squark build -v prints it
}

type MemRow struct {
	Kind   string `json:"kind"` // code, static, stack, helpers, sockets, heap
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	Count  int    `json:"count,omitempty"`  // tasks, for a stack row
	Each   int64  `json:"each,omitempty"`   // bytes per task
	Proven int64  `json:"proven,omitempty"` // the proven stack, for a stack row
}

// Limits on a run.
const (
	runTimeout = 10 * time.Second
	maxOutput  = 256 << 10
)

// builds limits how many compiles run at once: clang is the expensive part; runs, how many
// programs run at once.
var builds, runs = make(chan struct{}, 2), make(chan struct{}, 4)

// maxWorst is the largest proven worst case the tour runs: a program's memory is bounded by it
// (workers 1024, or a 1T arena, would not be).
const maxWorst = 1 << 30

// Check type-checks and lifetime-checks code without compiling it to C, which takes
// milliseconds: the tour checks as you type.
func Check(code string) (*Result, error) {
	dir, err := writeProgram(code)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := driver.Check(dir); err != nil {
		return compileError(dir, err), nil
	}
	return &Result{Stage: "checked"}, nil
}

// Run compiles code, as squark build does, and runs it unless mode is "build".
func Run(ctx context.Context, code, mode string) (*Result, error) {
	dir, err := writeProgram(code)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	select {
	case builds <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	t0 := time.Now()
	bin := filepath.Join(dir, "prog")
	res, err := driver.Build(driver.Options{Dir: dir, Output: bin, WorkDir: filepath.Join(dir, "work")})
	<-builds
	buildMS := time.Since(t0).Milliseconds()
	if err != nil {
		r := compileError(dir, err)
		r.BuildMS = buildMS
		if res != nil && res.Memory != nil {
			r.Memory = memoryOf(res.Memory) // over budget: the build shows why
		}
		return r, nil
	}
	r := &Result{Stage: "built", BuildMS: buildMS, Memory: memoryOf(res.Memory), Output: strings.ReplaceAll(res.Notes, dir+string(filepath.Separator), "")}
	if mode == "build" {
		return r, nil
	}
	// the worst case on this machine: workers auto and threads auto are one per core
	w := float64(res.Memory.Worst())
	if res.Memory.Workers == 0 {
		w *= float64(runtime.NumCPU())
	}
	if res.Memory.Threads == 0 {
		w *= float64(runtime.NumCPU())
	}
	if w > maxWorst {
		r.Output += "the tour runs programs whose worst case is at most 1 GiB here; build this one with squark build\n"
		return r, nil
	}
	select {
	case runs <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	r.Stage = "run"
	runProgram(ctx, bin, dir, r)
	<-runs
	r.Output = strings.ReplaceAll(r.Output, dir+string(filepath.Separator), "") // panics name the file
	return r, nil
}

func writeProgram(code string) (string, error) {
	dir, err := os.MkdirTemp("", "squark-tour-")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "main.sq"), []byte(code), 0o644); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

var diagRE = regexp.MustCompile(`^main\.sq:(\d+):(\d+): (.*)$`)

func compileError(dir string, err error) *Result {
	msg := strings.ReplaceAll(err.Error(), dir+string(filepath.Separator), "")
	r := &Result{Stage: "compile", Output: msg, Exit: 1}
	for _, ln := range strings.Split(msg, "\n") {
		if m := diagRE.FindStringSubmatch(ln); m != nil {
			line, _ := strconv.Atoi(m[1])
			col, _ := strconv.Atoi(m[2])
			r.Errors = append(r.Errors, Diag{Line: line, Col: col, Msg: m[3]})
		}
	}
	return r
}

// limitedBuffer keeps the first maxOutput bytes and calls full once when more arrive.
type limitedBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	over bool
	full func()
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := maxOutput - b.buf.Len(); len(p) > room {
		b.buf.Write(p[:max(room, 0)])
		if !b.over {
			b.over = true
			b.full()
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

func runProgram(ctx context.Context, bin, dir string, r *Result) {
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	c := exec.Command(bin)
	c.Dir = dir
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // workers N: kill the whole group
	out := &limitedBuffer{full: cancel}
	c.Stdout, c.Stderr = out, out
	t0 := time.Now()
	if err := c.Start(); err != nil {
		r.Output, r.Exit = err.Error(), 1
		return
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		err = <-done
		r.TimedOut = ctx.Err() == context.DeadlineExceeded
	}
	r.RunMS = time.Since(t0).Milliseconds()
	out.mu.Lock()
	r.Output = out.buf.String()
	if out.over {
		r.Output += "\n[output truncated at 256 KiB]\n"
	}
	out.mu.Unlock()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		r.Exit = ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			r.Exit = 128 + int(ws.Signal())
		}
	} else if err != nil {
		r.Output += err.Error()
		r.Exit = 1
	}
}

func memoryOf(m *driver.MemoryReport) *Memory {
	if m == nil {
		return nil
	}
	mem := &Memory{Worst: m.Worst(), Budget: m.Budget, Threads: m.Threads, Workers: m.Workers, Report: driver.FormatMemory(m)}
	mem.Rows = append(mem.Rows, MemRow{Kind: "code", Name: "code", Bytes: m.Code}, MemRow{Kind: "static", Name: "static data", Bytes: m.Static})
	// the biggest pools first, as squark build -v lists them
	pools := slices.Clone(m.Pools)
	slices.SortStableFunc(pools, func(a, b driver.PoolReport) int {
		return cmp.Compare(int64(b.Count)*b.Touchable, int64(a.Count)*a.Touchable)
	})
	for _, p := range pools {
		name := "task " + p.Name
		if p.Name == "main.main" {
			name = "main task"
		}
		mem.Rows = append(mem.Rows, MemRow{Kind: "stack", Name: name, Bytes: int64(p.Count) * p.Touchable, Count: p.Count, Each: p.Touchable, Proven: p.Stack})
	}
	if m.Helpers > 0 {
		mem.Rows = append(mem.Rows, MemRow{Kind: "helpers", Name: "blocking-call helpers", Bytes: m.Helpers * m.HelperStack, Count: int(m.Helpers), Each: m.HelperStack})
	}
	if s := m.Sockets(); s > 0 {
		mem.Rows = append(mem.Rows, MemRow{Kind: "sockets", Name: "sockets", Bytes: s})
	}
	for _, l := range m.CLibs {
		if h, ok := m.Heaps[l]; ok {
			mem.Rows = append(mem.Rows, MemRow{Kind: "heap", Name: "C library " + l, Bytes: h})
		} else {
			mem.Uncounted = append(mem.Uncounted, l)
		}
	}
	return mem
}
