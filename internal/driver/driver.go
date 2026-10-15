// Package driver turns squark source into a native binary: front end, C generation,
// stack proving with clang -fstack-usage, static goroutine pools, link, and a no-heap check.
package driver

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

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
}

type PoolReport struct {
	Name      string
	Count     int
	Stack     int64 // proven bytes
	Slot      int64 // reserved bytes per goroutine (rounded + guard page)
	Touchable int64 // pages a goroutine can actually touch: the proven stack rounded up to pages
}

type Result struct {
	Binary string
	Pools  []PoolReport
	CBytes int
	Memory *MemoryReport
	Notes  string // e.g. the package variables each scheduler thread has its own copy of
}

// MemoryReport is the worst-case memory a program can use, computed at build time.
type MemoryReport struct {
	Budget  int64 // 0: none declared
	Workers int64 // processes (workers N); 0: one per core, unknown when compiling
	Threads int64 // scheduler threads per process (threads N); 0: one per core
	Code    int64 // __TEXT and read-only data
	Static  int64 // data + bss: package variables, arenas, channel buffers, goroutine tables
	Pools   []PoolReport
	CLibs   []string
	// helper threads for blocking externs, per scheduler thread, and each one's stack
	Helpers     int64
	HelperStack int64
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
// data and goroutine stacks; with workers auto this is the total for one worker.
func (m *MemoryReport) Worst() int64 {
	per := satAdd(m.Static, satMul(m.Helpers, m.HelperStack))
	for _, p := range m.Pools {
		per = satAdd(per, satMul(int64(p.Count), p.Touchable))
	}
	return satAdd(m.Code, satMul(satMul(max(m.Workers, 1), max(m.Threads, 1)), per))
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
	notes := perThreadNote(prog)
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
	const progC = "// Code generated by squark. DO NOT EDIT.\n#define _GNU_SOURCE\n#include \"main.c\"\n#include \"sq_rt.c\"\n"
	if err := os.WriteFile(filepath.Join(work, "prog.c"), []byte(progC), 0o644); err != nil {
		return nil, err
	}

	// one section per function and datum, so the linker drops what the program doesn't use
	cflags := []string{opt.OptFlag, "-std=gnu11", "-pthread", "-fwrapv", "-fno-strict-aliasing", "-fno-delete-null-pointer-checks", "-fno-strict-float-cast-overflow", "-w", "-fstack-usage", "-ffunction-sections", "-fdata-sections", "-I", work}
	if runtime.GOOS == "linux" {
		// A C frame (or VLA) larger than a page must touch each page on the way down, or it can
		// jump over a goroutine stack's one guard page into the next stack. gcc on arm64 assumes a
		// 64 KiB guard unless told the page size. (macOS probes large frames already.)
		cflags = append(cflags, "-fstack-clash-protection")
		if runtime.GOARCH == "arm64" {
			cflags = append(cflags, "--param=stack-clash-protection-guard-size=12")
		}
	}
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
		return nil
	}
	if err := cc("prog.c", "prog.o", squarkBug); err != nil {
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
	frames, rtFrames, err := readStackUsage(filepath.Join(work, "prog.su"))
	if err != nil {
		return nil, err
	}
	var rtMax int64
	// functions that run only on a scheduler thread's own stack (or the signal stack), never on a
	// goroutine's: their frames don't count against goroutine stacks
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
	// C code that moves its goroutine to a helper mid-call (sq_detach) needs helpers too; the C
	// code then runs on the goroutine's own stack, so the helper's stack only runs its loop
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
		usable := (need + page - 1) / page * page
		slot := usable + page // guard page
		p.StackSize = need
		total += p.Count
		// stacks are reserved with one mmap per pool at startup (a multi-GiB BSS would collide
		// with the dyld shared region on macOS); their size is still fixed at compile time
		fmt.Fprintf(&pools, "static sq_g sq_gs%d[%d];\n", i, p.Count)
		preemptible := p.Entry != nil && p.Entry.Preemptible
		anyPreemptible = anyPreemptible || preemptible
		fmt.Fprintf(&pools, "sq_pool %s = {.name = %q, .count = %d, .stack_size = %d, .slot_size = %d, .gs = sq_gs%d, .index = %d, .preemptible = %t};\n\n", p.CName, p.Name, p.Count, need, slot, i, i, preemptible)
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
	args := append(append([]string{opt.OptFlag, "-pthread", "-o", absOut}, objs...), ldflags...)
	if runtime.GOOS == "darwin" {
		args = append(args, "-Wl,-dead_strip")
	} else {
		args = append(args, "-Wl,--gc-sections")
	}
	if opt.Sanitize != "" {
		args = append(args, "-fsanitize="+opt.Sanitize)
	}
	cmd := ccCommand(args...)
	cmd.Dir = work
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("link failed:\n%s", stderr.String())
	}
	res.Binary = absOut
	res.Notes = notes
	mem := &MemoryReport{Budget: prog.Budget, Workers: prog.Workers, Threads: prog.Threads, Pools: res.Pools,
		Helpers: helpers, HelperStack: helperStackReserved(helperStack)}
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
	if prog.Budget > 0 && mem.Worst() > prog.Budget {
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
	fmt.Fprintf(&b, "  %-26s %12s   package variables, arenas, channel buffers, goroutine tables\n", "static data", human(m.Static))
	ps := append([]PoolReport(nil), m.Pools...)
	sort.SliceStable(ps, func(i, j int) bool { return int64(ps[i].Count)*ps[i].Touchable > int64(ps[j].Count)*ps[j].Touchable })
	for _, p := range ps {
		name := "task " + p.Name
		if p.Name == "main.main" {
			name = "main goroutine"
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
	if len(m.CLibs) > 0 {
		verb := "manages its"
		if len(m.CLibs) > 1 {
			verb = "manage their"
		}
		fmt.Fprintf(&b, "  %-26s %12s   %s %s own memory\n", "C libraries", "not counted", strings.Join(m.CLibs, ", "), verb)
	}
	return b.String()
}

func absDir(d string) string {
	a, err := filepath.Abs(d)
	if err != nil {
		return d
	}
	return a
}

// readStackUsage parses clang -fstack-usage output: "file:line:col:func<TAB>bytes<TAB>qualifiers".
// It returns every function's frame, and separately the runtime's (defined in sq_rt.c or its
// headers; the program's functions carry #line directives naming their .sq files).
func readStackUsage(path string) (frames, rt map[string]int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading stack usage: %v", err)
	}
	defer f.Close()
	frames, rt = map[string]int64{}, map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) < 3 {
			continue
		}
		loc := parts[0]
		name := loc[strings.LastIndex(loc, ":")+1:]
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		if strings.Contains(parts[2], "dynamic") && !strings.Contains(parts[2], "bounded") {
			return nil, nil, fmt.Errorf("function %s has a dynamically sized stack frame; cannot prove its stack size", name)
		}
		if n > frames[name] {
			frames[name] = n
		}
		file := filepath.Base(strings.SplitN(loc, ":", 2)[0])
		if (file == "sq_rt.c" || file == "sq_rt.h" || file == "sq_internal.h") && n > rt[name] {
			rt[name] = n
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
	return perThreadNote(prog), nil
}

// perThreadNote lists, for a program written for threads, the package variables it changes after
// start-up: each scheduler thread has its own copy, so their values can differ between threads.
func perThreadNote(prog *compiler.Program) string {
	if !prog.ThreadsSet {
		return ""
	}
	vars := compiler.PerThreadVars(prog)
	if len(vars) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("note: with threads, each scheduler thread has its own copy of these package variables:\n")
	for _, v := range vars {
		fmt.Fprintf(&b, "  %s:%d: %s: %s\n", v.Var.Pos.File, v.Var.Pos.Line, v.Var.Name, v.Reason)
	}
	b.WriteString("  (declare them `thread var` if that's intended; share between threads with a shared var channel)\n")
	return b.String()
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
		ents, err := fs.ReadDir(squark.Files, "std")
		if err != nil {
			return "", err
		}
		var src strings.Builder
		src.WriteString("package main\n\nimport (\n")
		for _, e := range ents {
			if e.IsDir() {
				fmt.Fprintf(&src, "\t_%s %q\n", e.Name(), e.Name())
			}
		}
		src.WriteString(")\n\nfunc main() {}\n")
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
		case pkg != "" && p.Name != pkg:
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
	fmt.Fprintf(&b, "%-28s %8s %14s %14s\n", "goroutine pool", "max", "proven stack", "reserved (VM)")
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
