// Package driver turns squark source into a native binary: front end, C generation,
// stack proving with clang -fstack-usage, static task pools, link, and a no-heap check.
package driver

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"squark"
	"squark/internal/compiler"
)

type Options struct {
	Dir     string // main package directory
	Output  string
	WorkDir string // keep intermediate files here ("" = temp, removed)
	Verbose bool
	CC      string
	OptFlag string
	// Sanitize, if set, builds with -fsanitize=<Sanitize> (e.g. "thread") and -g, for the
	// runtime's own tests. The no-heap check still applies to the program's objects.
	Sanitize string
	// Remarks, if set, receives clang's loop-vectorization remarks for the program (which
	// loops became SIMD code, and why the others didn't). clang only.
	Remarks io.Writer
	// CPU is the CPU to build for (see cpuFlags); "" means $SQUARK_CPU, or else the baseline.
	CPU string
	// Static links everything into the binary, C libraries (and libc) included, so it runs in an
	// empty container image (FROM scratch). Linux only: macOS has no static libc.
	Static bool
}

// cpuFlags returns the C compiler flags for building for cpu on goarch:
//
//	baseline                    every CPU of the architecture (x86-64: SSE2 only), the default
//	native                      the CPU building the program (the binary may not run elsewhere)
//	x86-64-v2, -v3, -v4         x86-64 levels: v2 adds SSE4.2, v3 AVX2 and FMA, v4 AVX-512
//	armv8.1-a … armv9.5-a       arm64 architecture versions
//
// On x86-64 a program built for more than the baseline checks the CPU when it starts.
func cpuFlags(cpu, goarch string) ([]string, error) {
	switch {
	case cpu == "" || cpu == "baseline":
		return nil, nil
	case cpu == "native" && goarch == "arm64":
		return []string{"-mcpu=native"}, nil
	case cpu == "native" && goarch == "amd64":
		return []string{"-march=native", "-DSQ_CPU=\"native\""}, nil
	case goarch == "amd64" && (cpu == "x86-64-v2" || cpu == "x86-64-v3" || cpu == "x86-64-v4"):
		return []string{"-march=" + cpu, "-DSQ_CPU=\"" + cpu + "\""}, nil
	case goarch == "arm64" && armLevel.MatchString(cpu):
		return []string{"-march=" + cpu}, nil
	}
	levels := "x86-64-v2, x86-64-v3, x86-64-v4"
	if goarch == "arm64" {
		levels = "armv8.1-a to armv8.9-a, armv9-a to armv9.5-a"
	}
	return nil, fmt.Errorf("unknown CPU %q for %s: use baseline, native or %s", cpu, goarch, levels)
}

var armLevel = regexp.MustCompile(`^armv(8\.[1-9]|9(\.[1-5])?)-a$`)

type PoolReport struct {
	Name      string
	Count     int
	Stack     int64 // proven bytes
	Slot      int64 // reserved bytes per task (rounded + guard page)
	Touchable int64 // pages a task can actually touch: the proven stack rounded up to pages
}

type Result struct {
	Binary string
	Pools  []PoolReport
	CBytes int
	Memory *MemoryReport
	Notes  string // e.g. the package variables each scheduler thread has its own copy of
	// Shutdown is the graceful stop's grace period (shutdown D; 0: the runtime's default, 10s)
	Shutdown int64
}

// MemoryReport is the worst-case memory a program can use, computed at build time.
type MemoryReport struct {
	Budget  int64 // 0: none declared
	Workers int64 // processes (workers N); 0: one per core, unknown when compiling
	Threads int64 // scheduler threads per process (threads N); 0: one per core
	Code    int64 // __TEXT and read-only data
	Static  int64 // data + bss: package variables, arenas, channel buffers, task tables
	Pools   []PoolReport
	CLibs   []string
	// helper threads for blocking externs, per scheduler thread, and each one's stack
	Helpers     int64
	HelperStack int64
	// sockets: connections N sockets, each with SO_RCVBUF and SO_SNDBUF of sockbuf N (0, 0: not
	// declared, so not counted); UsesNet says whether the program has sockets at all
	SockBuf, Conns int64
	UsesNet        bool
	Heaps          map[string]int64 // declared C library heaps, by link name
}

// socketCost is what the kernel may charge one socket: Linux doubles SO_RCVBUF and SO_SNDBUF for
// its bookkeeping and charges both to the cgroup, plus about 4 KiB for the socket itself.
func socketCost(sockbuf int64) int64 { return 4*sockbuf + 4<<10 }

// Sockets is the counted socket memory per process (0 if not declared).
func (m *MemoryReport) Sockets() int64 {
	if m.SockBuf == 0 || m.Conns == 0 {
		return 0
	}
	return satMul(m.Conns, socketCost(m.SockBuf))
}

// AllCounted reports whether nothing the report knows of is left uncounted: sockets (if the
// program uses net) and every linked C library's heap.
func (m *MemoryReport) AllCounted() bool {
	if m.UsesNet && m.Sockets() == 0 {
		return false
	}
	for _, l := range m.CLibs {
		if _, ok := m.Heaps[l]; !ok {
			return false
		}
	}
	return true
}

// helpersPerThread is how many helper threads run blocking externs, per scheduler thread.
const helpersPerThread = 2

// helperStackReserved is the stack the runtime gives a helper thread for a proven need (see
// start_helpers in sq_rt.c).
func helperStackReserved(need int64) int64 {
	if need == 0 {
		return 0
	}
	page := int64(os.Getpagesize())
	return max((need+16384+page-1)/page*page, 65536)
}

// Worst returns the worst-case total. Worker processes share the code but each has its own static
// data and task stacks; with workers auto this is the total for one worker.
// Known reports whether Worst is a number: workers auto and threads auto leave it to the machine.
func (m *MemoryReport) Known() bool { return m.Workers > 0 && m.Threads > 0 && m.Code > 0 }

func (m *MemoryReport) Worst() int64 {
	per := satAdd(m.Static, satMul(m.Helpers, m.HelperStack))
	for _, p := range m.Pools {
		per = satAdd(per, satMul(int64(p.Count), p.Touchable))
	}
	total := satAdd(m.Code, satMul(satMul(max(m.Workers, 1), max(m.Threads, 1)), per))
	// sockets and C heaps are per process: the limits apply to each worker
	proc := m.Sockets()
	for _, h := range m.Heaps {
		proc = satAdd(proc, h)
	}
	return satAdd(total, satMul(max(m.Workers, 1), proc))
}

// satAdd and satMul saturate at MaxInt64 instead of wrapping, so a huge worst case can't pass the
// budget check by overflowing (sizes are non-negative).
func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func satMul(a, b int64) int64 {
	if a != 0 && b > math.MaxInt64/a {
		return math.MaxInt64
	}
	return a * b
}

var heapSymbols = []string{"malloc", "calloc", "realloc", "free", "posix_memalign", "aligned_alloc", "valloc", "strdup", "strndup", "asprintf", "vasprintf"}

func Build(opt Options) (*Result, error) {
	if opt.Static && runtime.GOOS != "linux" {
		return nil, fmt.Errorf("-static builds are for Linux: macOS has no static C library (build in a Linux container; see examples/shop)")
	}
	if opt.CC == "" {
		opt.CC = os.Getenv("CC")
		if opt.CC == "" {
			opt.CC = "clang"
		}
	}
	// CC may carry arguments ("ccache clang", "clang -m64")
	ccWords := strings.Fields(opt.CC)
	if len(ccWords) == 0 {
		return nil, fmt.Errorf("CC is empty")
	}
	if _, err := exec.LookPath(ccWords[0]); err != nil {
		return nil, fmt.Errorf("C compiler %q not found (set CC to a clang or gcc): %v", ccWords[0], err)
	}
	ccCommand := func(args ...string) *exec.Cmd {
		return exec.Command(ccWords[0], append(append([]string{}, ccWords[1:]...), args...)...)
	}
	if opt.OptFlag == "" {
		opt.OptFlag = "-O3"
	}
	l := &compiler.Loader{Std: squark.Files, MainDir: opt.Dir}
	prog, err := l.Load()
	if err != nil {
		return nil, err
	}
	order, err := compiler.Compile(prog)
	if err != nil {
		return nil, err
	}
	notes := ""
	out, err := compiler.Generate(prog, order)
	if err != nil {
		return nil, err
	}

	work := opt.WorkDir
	if work == "" {
		work, err = os.MkdirTemp("", "squark-build-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(work)
	} else if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, err
	}
	for _, n := range []string{"sq_rt.h", "sq_internal.h", "sq_rt.c"} {
		b, err := fs.ReadFile(squark.Files, "runtime/"+n)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(work, n), b, 0o644); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(work, "main.c"), []byte(out.MainC), 0o644); err != nil {
		return nil, err
	}
	// The program and the runtime compile as one translation unit, so the C compiler can inline
	// runtime functions (channel and map operations, I/O wrappers) into the program, and the
	// stack proof reads the frames that result.
	// The idle-connection queue is 256 KiB of each thread's static data in a server; a program
	// without sockets needs none of it, and one with connections N needs N entries.
	idleSlots := int64(1)
	for _, p := range prog.Packages {
		if p.Path == "net" {
			idleSlots = 1 << 16
			if prog.Conns > 0 {
				idleSlots = min(prog.Conns, 1<<16)
			}
		}
	}
	progC := fmt.Sprintf("// Code generated by squark. DO NOT EDIT.\n#define _GNU_SOURCE\n#define SQ_IDLE_SLOTS %d\n#include \"main.c\"\n#include \"sq_rt.c\"\n", idleSlots)
	if err := os.WriteFile(filepath.Join(work, "prog.c"), []byte(progC), 0o644); err != nil {
		return nil, err
	}

	if opt.CPU == "" {
		opt.CPU = os.Getenv("SQUARK_CPU")
	}
	cpu, err := cpuFlags(opt.CPU, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	// one section per function and datum, so the linker drops what the program doesn't use
	cflags := []string{opt.OptFlag, "-std=gnu11", "-pthread", "-fwrapv", "-fno-strict-aliasing", "-fno-delete-null-pointer-checks", "-w", "-fstack-usage", "-ffunction-sections", "-fdata-sections", "-I", work}
	if runtime.GOOS == "linux" {
		// A C frame (or VLA) larger than a page must touch each page on the way down, or it can
		// jump over a task stack's one guard page into the next stack. gcc on arm64 assumes a
		// 64 KiB guard unless told the page size. (macOS probes large frames already.)
		cflags = append(cflags, "-fstack-clash-protection")
		if runtime.GOARCH == "arm64" {
			cflags = append(cflags, "--param=stack-clash-protection-guard-size=12")
		}
	}
	cflags = append(cflags, cpu...)
	if opt.Sanitize != "" {
		cflags = append(cflags, "-fsanitize="+opt.Sanitize, "-g")
	}
	var extraCflags, ldflags []string
	for _, p := range prog.Packages {
		extraCflags = append(extraCflags, p.CFlags...)
		ldflags = append(ldflags, p.LDFlags...)
		for _, lib := range p.Links {
			ldflags = append(ldflags, "-l"+lib)
		}
	}
	// cc compiles src; blame says whose code it is when it fails
	const squarkBug = "this is a squark compiler bug"
	cc := func(src, obj, blame string, extra ...string) error {
		args := append(append(append([]string{}, cflags...), extra...), "-c", src, "-o", obj)
		cmd := ccCommand(args...)
		cmd.Dir = work
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("C compiler failed on %s (%s): %v\n%s", src, blame, err, stderr.String())
		}
		if opt.Remarks != nil && src == "prog.c" {
			opt.Remarks.Write(stderr.Bytes())
		}
		return nil
	}
	var progExtra []string
	if opt.Remarks != nil {
		progExtra = []string{"-gline-tables-only", "-Rpass=loop-vectorize", "-Rpass-missed=loop-vectorize", "-Rpass-analysis=loop-vectorize"}
	}
	if err := cc("prog.c", "prog.o", squarkBug, progExtra...); err != nil {
		return nil, err
	}
	objs := []string{"prog.o"}
	checked := []string{"prog.o"}
	// each package's C files (and headers) go in their own directory, under their own names
	for i, p := range prog.Packages {
		if len(p.CFiles) == 0 {
			continue
		}
		sub := fmt.Sprintf("cpkg%d_%s", i, p.Name)
		if err := os.MkdirAll(filepath.Join(work, sub), 0o755); err != nil {
			return nil, err
		}
		for _, cf := range p.CFiles {
			if err := os.WriteFile(filepath.Join(work, sub, cf.Name), cf.Src, 0o644); err != nil {
				return nil, err
			}
		}
		blame := squarkBug // std packages ship their C with squark
		if filepath.IsAbs(p.Path) {
			blame = "C code in package " + p.Name + " at " + p.Dir
		}
		for _, cf := range p.CFiles {
			if !strings.HasSuffix(cf.Name, ".c") {
				continue
			}
			src := filepath.Join(sub, cf.Name)
			obj := strings.TrimSuffix(src, ".c") + ".o"
			if err := cc(src, obj, blame, append([]string{"-I", sub}, extraCflags...)...); err != nil {
				return nil, err
			}
			objs = append(objs, obj)
			checked = append(checked, obj)
		}
	}

	// stack proving
	asan := opt.Sanitize == "address"
	frames, rtFrames, err := readStackUsage(filepath.Join(work, "prog.su"), asan)
	if err != nil {
		return nil, err
	}
	var rtMax int64
	// functions that run only on a scheduler thread's own stack (or the signal stack), never on a
	// task's: their frames don't count against task stacks
	schedulerOnly := map[string]bool{"main": true, "schedule": true, "on_fault": true, "netpoll": true,
		"shard_run": true, "shard_thread": true, "shard_new": true, "start_shards": true, "run_workers": true, "fire_timers": true,
		"helper_thread": true, "start_helpers": true, "watchdog": true, "start_watchdog": true, "thread_cpu_ms": true, "watch_warn": true}
	for name, sz := range rtFrames {
		if schedulerOnly[name] {
			continue
		}
		if sz > rtMax {
			rtMax = sz
		}
	}
	// runtime calls nest at most a few frames deep, plus libc syscall wrappers
	runtimeBudget := 3*rtMax + 2048
	const externDefault = 64 << 10
	sizes, _ := compiler.StackPlan(prog, out.Pools, frames, runtimeBudget, externDefault)
	// blocking externs run on helper threads: helpersPerThread per scheduler thread, each with
	// a stack proven for the largest of them (plus libc's share of the thread)
	helperStack := compiler.HelperStack(prog, frames, externDefault)
	var helpers int64
	if helperStack > 0 {
		helperStack += 2 * rtMax
		helpers = helpersPerThread
	}
	// C code that moves its task to a helper mid-call (sq_detach) needs helpers too; the C
	// code then runs on the task's own stack, so the helper's stack only runs its loop
	for _, o := range checked {
		syms, err := undefinedSymbols(filepath.Join(work, o))
		if err != nil {
			return nil, err
		}
		for _, s := range syms {
			if s == "sq_detach" || s == "_sq_detach" {
				helpers = helpersPerThread
				helperStack = max(helperStack, 4*rtMax)
			}
		}
	}
	page := int64(os.Getpagesize())
	const margin = 1024
	res := &Result{CBytes: len(out.MainC)}
	var pools strings.Builder
	pools.WriteString("// Code generated by squark. DO NOT EDIT.\n#include \"sq_internal.h\"\n\n")
	total := 0
	var names []string
	anyPreemptible := false
	for _, p := range out.Pools {
		if p.Entry != nil && p.Entry.StackLimit > 0 && sizes[p]+margin > p.Entry.StackLimit {
			return nil, fmt.Errorf("%s: task %s needs %d bytes of stack, more than its declared stack %d", p.Entry.Decl.P, p.Name, sizes[p]+margin, p.Entry.StackLimit)
		}
	}
	for i, p := range out.Pools {
		need := sizes[p] + margin
		if asan {
			// AddressSanitizer pads every frame with redzones (and makes some frames dynamic):
			// the proof doesn't hold, so give its builds ample stacks instead
			need = need*4 + 256<<10
		}
		usable := (need + page - 1) / page * page
		slot := usable + page // guard page
		p.StackSize = need
		total += p.Count
		// stacks are reserved with one mmap per pool at startup (a multi-GiB BSS would collide
		// with the dyld shared region on macOS); their size is still fixed at compile time
		fmt.Fprintf(&pools, "static sq_g sq_gs%d[%d];\n", i, p.Count)
		preemptible := p.Entry != nil && p.Entry.Preemptible
		anyPreemptible = anyPreemptible || preemptible
		isolate := p.Entry != nil && p.Entry.Isolate
		fmt.Fprintf(&pools, "sq_pool %s = {.name = %q, .count = %d, .stack_size = %d, .slot_size = %d, .gs = sq_gs%d, .index = %d, .preemptible = %t, .isolate = %t};\n\n", p.CName, p.Name, p.Count, need, slot, i, i, preemptible, isolate)
		names = append(names, "&"+p.CName)
		res.Pools = append(res.Pools, PoolReport{Name: p.Name, Count: p.Count, Stack: need, Slot: slot, Touchable: usable})
	}
	fmt.Fprintf(&pools, "sq_pool *const sq_pools[] = {%s};\n", strings.Join(names, ", "))
	fmt.Fprintf(&pools, "const bool sq_any_preemptible = %t;\n", anyPreemptible)
	fmt.Fprintf(&pools, "const int64_t sq_npools = %d;\n", len(out.Pools))
	fmt.Fprintf(&pools, "const int64_t sq_max_goroutines = %d;\n", total)
	fmt.Fprintf(&pools, "const int64_t sq_workers = %d;\n", prog.Workers)
	fmt.Fprintf(&pools, "const int64_t sq_threads = %d;\n", prog.Threads)
	// SQUARK_THREADS overrides the count, but only in programs written for threads (a threads
	// directive), and never above a count the memory budget was proven for
	threadsMax := int64(0)
	if prog.Budget > 0 {
		threadsMax = prog.Threads
	}
	fmt.Fprintf(&pools, "const bool sq_threads_env = %t;\nconst int64_t sq_threads_max = %d;\n", prog.ThreadsSet, threadsMax)
	fmt.Fprintf(&pools, "sq_g *sq_timer_heap[%d];\n", total)
	fmt.Fprintf(&pools, "const int64_t sq_helpers_per_thread = %d;\nconst int64_t sq_helper_stack = %d;\n", helpers, helperStack)
	fmt.Fprintf(&pools, "const int64_t sq_shutdown_ns = %d;\n", prog.Shutdown)
	fmt.Fprintf(&pools, "const int64_t sq_sockbuf = %d;\nconst int64_t sq_max_conns = %d;\n", prog.SockBuf, prog.Conns)
	var heapNames []string
	for l := range prog.Heaps {
		heapNames = append(heapNames, l)
	}
	sort.Strings(heapNames)
	pools.WriteString("const sq_heap_limit sq_heap_limits[] = {")
	for _, l := range heapNames {
		fmt.Fprintf(&pools, "{%q, %d}, ", l, prog.Heaps[l])
	}
	pools.WriteString("{0, 0}};\n")
	if err := os.WriteFile(filepath.Join(work, "pools.c"), []byte(pools.String()), 0o644); err != nil {
		return nil, err
	}
	if err := cc("pools.c", "pools.o", squarkBug); err != nil {
		return nil, err
	}
	objs = append(objs, "pools.o")
	checked = append(checked, "pools.o")

	// the no-heap guarantee: no squark object may reference an allocator
	for _, o := range checked {
		syms, err := undefinedSymbols(filepath.Join(work, o))
		if err != nil {
			return nil, err
		}
		for _, s := range syms {
			for _, h := range heapSymbols {
				if s == h || s == "_"+h {
					return nil, fmt.Errorf("heap check failed: %s references %s", o, h)
				}
			}
		}
	}

	output := opt.Output
	if output == "" {
		output = filepath.Base(absDir(opt.Dir))
	}
	absOut, err := filepath.Abs(output)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(absOut); err == nil && st.IsDir() {
		return nil, fmt.Errorf("build output %q already exists and is a directory (use -o)", output)
	}
	// limits.o holds the proven worst case, which the runtime checks against the cgroup's memory
	// limit at start-up. It is only known once linked (code and static data are measured from
	// the binary), so the first link has 0 there and, once measured, the binary is linked again;
	// the constant's size doesn't change, so neither does the worst case.
	writeLimits := func(worst int64) error {
		src := fmt.Sprintf("// Code generated by squark. DO NOT EDIT.\n#include <stdint.h>\nconst int64_t sq_worst_case = %d;\n", worst)
		if err := os.WriteFile(filepath.Join(work, "limits.c"), []byte(src), 0o644); err != nil {
			return err
		}
		return cc("limits.c", "limits.o", squarkBug)
	}
	if err := writeLimits(0); err != nil {
		return nil, err
	}
	objs = append(objs, "limits.o")
	args := append(append([]string{opt.OptFlag, "-pthread", "-o", absOut}, objs...), ldflags...)
	if opt.Static {
		// glibc's static build works here: the runtime doesn't use getaddrinfo or the other NSS
		// lookups that need shared libraries at run time (net resolves names itself)
		args = append(args, "-static")
	}
	if runtime.GOOS == "darwin" {
		args = append(args, "-Wl,-dead_strip")
	} else {
		// libm is separate from libc on Linux; gcc calls sqrt and friends where clang inlines them
		args = append(args, "-Wl,--gc-sections", "-lm")
	}
	if opt.Sanitize != "" {
		args = append(args, "-fsanitize="+opt.Sanitize)
	}
	link := func() error {
		cmd := ccCommand(args...)
		cmd.Dir = work
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("link failed:\n%s", stderr.String())
		}
		return nil
	}
	if err := link(); err != nil {
		return nil, err
	}
	res.Binary = absOut
	res.Notes = notes
	res.Shutdown = prog.Shutdown
	mem := &MemoryReport{Budget: prog.Budget, Workers: prog.Workers, Threads: prog.Threads, Pools: res.Pools,
		Helpers: helpers, HelperStack: helperStackReserved(helperStack), SockBuf: prog.SockBuf, Conns: prog.Conns, Heaps: prog.Heaps}
	for _, p := range prog.Packages {
		mem.UsesNet = mem.UsesNet || p.Path == "net"
	}
	mem.Code, mem.Static, err = segmentSizes(absOut)
	if err != nil {
		if prog.Budget > 0 {
			return res, fmt.Errorf("cannot check the memory budget: %v", err)
		}
		mem.Code, mem.Static = 0, 0 // reported as unknown sizes; nothing to enforce
	}
	for _, p := range prog.Packages {
		mem.CLibs = append(mem.CLibs, p.Links...)
	}
	res.Memory = mem
	if w := mem.Worst(); w > 0 && mem.Known() && !asan {
		if err := writeLimits(w); err != nil {
			return nil, err
		}
		if err := link(); err != nil {
			return nil, err
		}
	}
	if prog.Budget > 0 && mem.Worst() > prog.Budget && !asan { // ASan stacks are padded, not proven
		return res, fmt.Errorf("memory budget exceeded: worst case %s > budget %s\n%sto fit: lower task pool sizes (task f() pool N), shrink package arenas, arrays and channel capacities, use fewer threads or workers, or raise the budget", human(mem.Worst()), human(prog.Budget), FormatMemory(mem))
	}
	return res, nil
}

// segmentSizes reads code and static data sizes from a linked binary: read-only and executable
// segments are code, writable ones (data and bss) are static data.
func segmentSizes(bin string) (code, static int64, err error) {
	if f, err := macho.Open(bin); err == nil {
		defer f.Close()
		for _, l := range f.Loads {
			if seg, ok := l.(*macho.Segment); ok {
				switch seg.Name {
				case "__TEXT", "__DATA_CONST":
					code += int64(seg.Memsz)
				case "__DATA":
					static += int64(seg.Memsz)
				}
			}
		}
		return code, static, nil
	}
	f, err := elf.Open(bin)
	if err != nil {
		return 0, 0, fmt.Errorf("%s is neither Mach-O nor ELF: %v", bin, err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		if p.Flags&elf.PF_W != 0 {
			static += int64(p.Memsz)
		} else {
			code += int64(p.Memsz)
		}
	}
	return code, static, nil
}

// FormatMemory renders the worst-case memory breakdown.
func FormatMemory(m *MemoryReport) string {
	var b strings.Builder
	if m.Budget > 0 {
		fmt.Fprintf(&b, "memory: worst case %s of %s budget\n", human(m.Worst()), human(m.Budget))
	} else if m.Workers == 0 || m.Threads == 0 {
		fmt.Fprintf(&b, "memory: worst case %s per core (no budget declared)\n", human(m.Worst()))
	} else {
		fmt.Fprintf(&b, "memory: worst case %s (no budget declared)\n", human(m.Worst()))
	}
	fmt.Fprintf(&b, "  %-26s %12s\n", "code", human(m.Code))
	fmt.Fprintf(&b, "  %-26s %12s   package variables, arenas, channel buffers, task tables\n", "static data", human(m.Static))
	ps := append([]PoolReport(nil), m.Pools...)
	sort.SliceStable(ps, func(i, j int) bool { return int64(ps[i].Count)*ps[i].Touchable > int64(ps[j].Count)*ps[j].Touchable })
	for _, p := range ps {
		name := "task " + p.Name
		if p.Name == "main.main" {
			name = "main task"
		}
		fmt.Fprintf(&b, "  %-26s %12s   = %d × %s stack (proven %d B)\n", name, human(satMul(int64(p.Count), p.Touchable)), p.Count, human(p.Touchable), p.Stack)
	}
	if m.Helpers > 0 {
		fmt.Fprintf(&b, "  %-26s %12s   = %d × %s stack: threads that run blocking and detached C calls\n", "blocking-call helpers", human(m.Helpers*m.HelperStack), m.Helpers, human(m.HelperStack))
	}
	switch {
	case m.Workers == 0:
		fmt.Fprintf(&b, "  %-26s %12s   one process per core: everything but code is per worker, so multiply by the core count\n", "workers", "auto")
	case m.Workers > 1:
		fmt.Fprintf(&b, "  %-26s %12s   processes; code is shared, static data and stacks above are per worker (counted %d×)\n", "workers", fmt.Sprint(m.Workers), m.Workers)
	}
	switch {
	case m.Threads == 0:
		fmt.Fprintf(&b, "  %-26s %12s   one scheduler thread per core: static data and stacks are per thread, so multiply by the core count\n", "threads", "auto")
	case m.Threads > 1:
		fmt.Fprintf(&b, "  %-26s %12s   scheduler threads; static data and stacks above are per thread (counted %d×)\n", "threads", fmt.Sprint(m.Threads), m.Threads)
	}
	if s := m.Sockets(); s > 0 {
		fmt.Fprintf(&b, "  %-26s %12s   = %d × %s: kernel buffers for at most %d sockets (sockbuf %s)%s\n", "sockets", human(s), m.Conns, human(socketCost(m.SockBuf)), m.Conns, human(m.SockBuf), perWorker(m))
	} else if m.UsesNet {
		fmt.Fprintf(&b, "  %-26s %12s   kernel socket buffers: declare connections N and sockbuf N to count them\n", "sockets", "not counted")
	}
	var uncounted []string
	for _, l := range m.CLibs {
		if h, ok := m.Heaps[l]; ok {
			fmt.Fprintf(&b, "  %-26s %12s   its heap limit (heap %s)%s\n", "C library "+l, human(h), l, perWorker(m))
		} else {
			uncounted = append(uncounted, l)
		}
	}
	if len(uncounted) > 0 {
		verb := "manages its"
		if len(uncounted) > 1 {
			verb = "manage their"
		}
		fmt.Fprintf(&b, "  %-26s %12s   %s %s own memory (heap %s N would count it)\n", "C libraries", "not counted", strings.Join(uncounted, ", "), verb, uncounted[0])
	}
	return b.String()
}

func perWorker(m *MemoryReport) string {
	if m.Workers > 1 {
		return fmt.Sprintf(", per worker (counted %d×)", m.Workers)
	}
	return ""
}

func absDir(d string) string {
	a, err := filepath.Abs(d)
	if err != nil {
		return d
	}
	return a
}

// readStackUsage parses clang -fstack-usage output: "file:line:col:func<TAB>bytes<TAB>qualifiers".
// With allowDynamic (sanitizer builds) a dynamic frame counts its static part. It returns every
// function's frame, and separately the runtime's (defined in sq_rt.c or its
// headers; the program's functions carry #line directives naming their .sq files).
func readStackUsage(path string, allowDynamic bool) (frames, rt map[string]int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading stack usage: %v", err)
	}
	defer f.Close()
	// gcc clones functions under suffixed names (f.isra.0, f.part.0, f.constprop.0, f.cold) and may
	// report only the clone. A C name has no '.', so the base name is the function; its frame is
	// the sum of its variants, since a .part clone runs nested inside the original.
	variants := map[string]map[string]int64{} // base name -> full name -> largest frame
	rtNames := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) < 3 {
			continue
		}
		loc := parts[0]
		full := loc[strings.LastIndex(loc, ":")+1:]
		name, _, _ := strings.Cut(full, ".")
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		if strings.Contains(parts[2], "dynamic") && !strings.Contains(parts[2], "bounded") && !allowDynamic {
			return nil, nil, fmt.Errorf("function %s has a dynamically sized stack frame; cannot prove its stack size", name)
		}
		if variants[name] == nil {
			variants[name] = map[string]int64{}
		}
		variants[name][full] = max(variants[name][full], n)
		file := filepath.Base(strings.SplitN(loc, ":", 2)[0])
		if file == "sq_rt.c" || file == "sq_rt.h" || file == "sq_internal.h" {
			rtNames[name] = true
		}
	}
	frames, rt = map[string]int64{}, map[string]int64{}
	for name, vs := range variants {
		for _, n := range vs {
			frames[name] += n
		}
		if rtNames[name] {
			rt[name] = frames[name]
		}
	}
	return frames, rt, sc.Err()
}

func undefinedSymbols(obj string) ([]string, error) {
	out, err := exec.Command("nm", "-u", obj).Output()
	if err != nil {
		return nil, fmt.Errorf("nm %s: %v", obj, err)
	}
	var syms []string
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		f := strings.Fields(l)
		syms = append(syms, f[len(f)-1])
	}
	return syms, nil
}

// Check runs only the front end.
func Check(dir string) error {
	_, err := CheckNotes(dir)
	return err
}

// CheckNotes runs only the front end and returns its notes (see Result.Notes).
func CheckNotes(dir string) (string, error) {
	l := &compiler.Loader{Std: squark.Files, MainDir: dir}
	prog, err := l.Load()
	if err != nil {
		return "", err
	}
	if _, err = compiler.Compile(prog); err != nil {
		return "", err
	}
	return "", nil
}

// Doc lists the exported functions of the program's packages (or of the named package) with
// their provenance, inferred or declared.
func Doc(dir, pkg string) (string, error) {
	if dir == "std" {
		// document the standard library through a program that imports all of it
		tmp, err := os.MkdirTemp("", "squark-doc-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(tmp)
		// every directory holding .sq files is a package, nested ones (math/rand) included
		var paths []string
		seen := map[string]bool{}
		err = fs.WalkDir(squark.Files, "std", func(p string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() && strings.HasSuffix(p, ".sq") {
				dir := strings.TrimPrefix(path.Dir(p), "std/")
				if !seen[dir] {
					seen[dir] = true
					paths = append(paths, dir)
				}
			}
			return err
		})
		if err != nil {
			return "", err
		}
		var src strings.Builder
		src.WriteString("program\n\nuse (\n")
		for _, p := range paths {
			fmt.Fprintf(&src, "\t%q as _%s\n", p, strings.ReplaceAll(p, "/", "_"))
		}
		src.WriteString(")\n\nfn main() {}\n")
		if err := os.WriteFile(filepath.Join(tmp, "main.sq"), []byte(src.String()), 0o644); err != nil {
			return "", err
		}
		dir = tmp
		if pkg == "" {
			pkg = "*"
		}
	}
	l := &compiler.Loader{Std: squark.Files, MainDir: dir}
	prog, err := l.Load()
	if err != nil {
		return "", err
	}
	if _, err := compiler.Compile(prog); err != nil {
		return "", err
	}
	var parts []string
	for _, p := range prog.Packages {
		switch {
		case pkg == "*" && p == prog.Main:
			continue
		case pkg == "*":
		case pkg == "" && p != prog.Main:
			continue
		case pkg != "" && p.Name != pkg && p.Path != pkg:
			continue
		}
		parts = append(parts, compiler.DocPackage(p, p == prog.Main))
	}
	if len(parts) == 0 && pkg != "" && pkg != "*" {
		var names []string
		for _, p := range prog.Packages {
			if p != prog.Main {
				names = append(names, p.Name)
			}
		}
		return "", fmt.Errorf("no package %s (have %s)", pkg, strings.Join(names, ", "))
	}
	return strings.Join(parts, "\n"), nil
}

// EmitC returns the generated C for a program.
func EmitC(dir string) (string, error) {
	l := &compiler.Loader{Std: squark.Files, MainDir: dir}
	prog, err := l.Load()
	if err != nil {
		return "", err
	}
	order, err := compiler.Compile(prog)
	if err != nil {
		return "", err
	}
	out, err := compiler.Generate(prog, order)
	if err != nil {
		return "", err
	}
	return out.MainC, nil
}

func hashOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

func FormatPools(ps []PoolReport) string {
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })
	var b strings.Builder
	fmt.Fprintf(&b, "%-28s %8s %14s %14s\n", "task pool", "max", "proven stack", "reserved (VM)")
	for _, p := range ps {
		fmt.Fprintf(&b, "%-28s %8d %12d B %12s\n", p.Name, p.Count, p.Stack, human(int64(p.Count)*p.Slot))
	}
	return b.String()
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Manifest returns resource settings that match a built program, for a Kubernetes pod spec
// (format "k8s") or a systemd unit ("systemd"): a memory limit of the proven worst case plus
// headroom for what squark doesn't count (kernel socket buffers, C libraries), a CPU limit of
// its scheduler threads, and a stop timeout longer than its graceful stop's grace period.
func Manifest(res *Result, name, format string) (string, error) {
	m := res.Memory
	if !m.Known() {
		return "", fmt.Errorf("the worst-case memory isn't known with threads auto or workers auto: write threads N (and workers N) for a manifest")
	}
	worst := m.Worst()
	// headroom for what squark doesn't count: page cache, and sockets and C heaps unless declared
	headroom := max(worst/10, 16<<20)
	if m.AllCounted() {
		headroom = max(worst/20, 8<<20)
	}
	limitMi := (worst + headroom + 1<<20 - 1) >> 20
	cpus := max(m.Workers, 1) * max(m.Threads, 1)
	grace := res.Shutdown
	if grace == 0 {
		grace = 10e9
	}
	stopSec := (grace+999999999)/1e9 + 5 // time to drain, plus a margin before SIGKILL
	var b strings.Builder
	switch format {
	case "k8s":
		fmt.Fprintf(&b, "# %s: proven worst case %s; the limit adds %s for what squark doesn't count\n", name, human(worst), human(headroom))
		fmt.Fprintf(&b, "# (%s). requests = limits: the Guaranteed QoS class.\n", uncountedText(m))
		fmt.Fprintf(&b, "resources:\n  requests:\n    cpu: \"%d\"\n    memory: \"%dMi\"\n  limits:\n    cpu: \"%d\"\n    memory: \"%dMi\"\n", cpus, limitMi, cpus, limitMi)
		fmt.Fprintf(&b, "# goes in the pod spec, next to containers: longer than the %s graceful stop\nterminationGracePeriodSeconds: %d\n", time.Duration(grace), stopSec)
	case "systemd":
		fmt.Fprintf(&b, "# %s: proven worst case %s, plus %s for what squark doesn't count\n", name, human(worst), human(headroom))
		fmt.Fprintf(&b, "# (%s); goes in the [Service] section\n", uncountedText(m))
		fmt.Fprintf(&b, "MemoryMax=%dM\nCPUQuota=%d%%\nKillSignal=SIGTERM\nTimeoutStopSec=%d\n", limitMi, cpus*100, stopSec)
	default:
		return "", fmt.Errorf("unknown manifest format %q: want k8s or systemd", format)
	}
	return b.String(), nil
}

// uncountedText names what the worst case leaves out.
func uncountedText(m *MemoryReport) string {
	parts := []string{"page cache"}
	if m.UsesNet && m.Sockets() == 0 {
		parts = append(parts, "kernel socket buffers")
	}
	for _, l := range m.CLibs {
		if _, ok := m.Heaps[l]; !ok {
			parts = append(parts, l+"'s heap")
		}
	}
	return strings.Join(parts, ", ")
}
