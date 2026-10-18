# Multi-core plan

squark runs on one OS thread today (spec rule 10: single-threaded and cooperative). One core
serves ~226k req/s in the notes benchmark while the machine has ten. This file is the plan for
using more cores, in stages, and the log of working through it. Tick items as they land, with the
commit and the test that covers them.

**Why staged.** Stage 1 needs almost no runtime change and shows how far throughput scales.
Stage 2 is the real design. Stage 3 removes the other single-thread bottleneck (blocking C calls).
Go-style M:N scheduling is deliberately out of scope (see the end).

**What makes this tractable.** The lifetime checker already forbids `go` arguments and channel
sends that reference stack or arena memory, so the only memory tasks can share is static
(package-level) memory. That is exactly the surface where data races could happen, and the
compiler can already see it.

---

## Stage 1: a process per core

`workers N` in package `main` makes the program fork N processes after start-up. Each process is a
complete copy (its own runtime, package variables, SQLite connection), and listening sockets use
`SO_REUSEPORT`, so the kernel spreads connections across them. No shared memory means no races
and no change to rule 10.

- [x] Runtime: `SO_REUSEPORT` on listening sockets. — only when more than one worker runs, so an unrelated second server still gets "address in use"
- [x] Language: a `workers N` directive in package `main` (N ≥ 1; `workers 0` or `workers auto` =
      one per core). Default stays 1. — `workers 1..1024` or `workers auto`; checker errors for range and for `auto` with `budget`
- [x] Runtime: fork the workers in `main()` before any squark code runs (before package
      initializers), so each process initializes its own state; the parent supervises. — `run_workers()` at the top of `main()`, before pools, signals and globals
- [x] Supervision: SIGINT/SIGTERM to the parent reach every worker; the parent exits when the
      workers have, with the first non-zero status (or 128+signal). A worker that crashes is
      reported on stderr; decide whether the parent restarts it or shuts everything down (start
      with shutdown: simplest and visible). — shutdown policy: SIGINT/SIGTERM/SIGHUP forwarded; first failing worker stops the rest and its status is the program's; Linux workers get PR_SET_PDEATHSIG (macOS has no equivalent)
- [x] Memory budget: the report and the `budget` check count N workers (per-worker breakdown ×
      N), so the proven worst case stays honest. — code once, static data and stacks × N; report shows the workers line
- [x] Docs: spec (directive, rule 10 unchanged within a process), README. — spec §0 rule 10, §1, §2, §9, error table; README
- [x] Tests: N workers all serve (distinct pids across requests on Linux); signals reach every
      worker; a crashing worker stops the program; budget × N; `workers 1` unchanged. — `TestWorkers` (4 processes, Linux spread, SIGTERM 143, crash → status 3, Linux supervisor SIGKILL), `TestBudget` (workers × budget), `testdata/errors/workers*.sq`, `TestWorkersSQLite`
- [x] Benchmark: harness runs notes with `workers` = cores; record req/s, CPU and RSS against Go. — `bench/workers-linux.sh` (Docker, 6 cores shared with the load generator): workers 3 ≈ 800k / 630k / 215k req/s for health / note / mix vs 450k / 295k / 165k for one process and 470k / 265k / 88k for Go GOMAXPROCS=3. Mix p99 rises to ~4.4ms: SQLite write locks across processes block a worker's whole thread (stage 3). The main harness isn't multi-process yet: its procstat is macOS-only and single-pid
- [x] Note the macOS caveat: its `SO_REUSEPORT` doesn't balance connections well; measure on Linux. — spec §9: Linux spreads connections; macOS sends them all to one worker

**Done when** the notes benchmark on Linux scales with cores and the tests above pass on macOS
and Linux.

## Stage 2: a thread per core, sharing nothing

One process, N scheduler threads (shards). Each shard is today's runtime: its own run queue,
timers, poller, task pools and idle set. Tasks never move between shards, so within a
shard rule 10 still holds and no locks are needed. Shards talk only through cross-shard channels.

Design decisions to settle first (record the answers here):

- [x] How work reaches each shard: run `main` on every shard (like stage 1, one address space),
      or start shards explicitly (e.g. `go on(shard) f()`, a per-shard entry point)? — **decided
      2026-10-14: `main` runs on every shard.** Programs written for `workers N` work unchanged.
- [x] Package variables: per-shard copies by default (thread-local), with a way to declare
      shared data that is immutable after initialization, checked by the compiler. — **decided
      2026-10-14: per-shard copies by default**; rule 10 keeps holding per shard.
- [x] Cross-shard channels: a separate type or a property of package-level channels? Element
      rules as today (no stack/arena references), plus a lock-free queue and a cross-thread wakeup
      (eventfd on Linux, EVFILT_USER on macOS). — **decided 2026-10-14: `shared var c chan T cap N`**
      (a property of package-level channel storage); elements may hold no references at all; a
      mutex per channel rather than a lock-free queue (simple, and TSan-checkable).
- [x] Default thread count — **decided 2026-10-14: opt in, then auto.** A program says it is
      written for threads with `threads auto` (or N); opted-in programs use every core by default
      and `SQUARK_THREADS=N` overrides at run time, like GOMAXPROCS (capped at N when a budget was
      proven for N). Programs without the directive stay single-threaded, since each thread runs
      its own copy of `main`. Test: `TestThreadsEnv`.
- [x] Silent per-thread package variables — **decided 2026-10-14:** under `threads`, build, run
      and check print a note listing package variables of the program's own packages that are
      changed after start-up (written, `++`, element/field writes, `&`, slicing, pointer methods,
      channel ops, maps/vectors/channels passed on). `thread var` says "per thread on purpose" and
      drops a variable from the note. Test: `TestPerThreadNote` (testdata/perthread).
- [x] Which externs are safe to call from any shard (C libraries with global state). — **decided
      2026-10-14:** externs run on whichever shard calls them, and C libraries must be thread-safe
      (documented in spec §9); per-shard package variables mean each shard has its own handles
      (e.g. its own SQLite connection).

Implementation steps (each lands with the full suite passing on macOS and Linux):

- [x] 2.1 Runtime state into `struct sq_shard`, reached through one thread-local pointer, with a
      single shard: scheduler context, current task, run queue, counters, poller fd and
      change list, timer heap, idle set, output buffer, `fd_read_kind`, the HTTP date cache. No
      behaviour change; check the benchmarks for the cost of thread-local access. — done: `sq_shard` + `_Thread_local sq_shard *sq_self`; notes HTTP throughput unchanged on macOS (224k/201k/153k vs 226k/202k/153k)
- [x] 2.2 Generated code: package variables (and package arenas, map/channel storage) into a
      per-shard globals struct, reached through the shard; `sq_init_globals` runs per shard.
      Static initializers that hold addresses move into the per-shard init. — done: `struct sq_globals` (package variables, package arenas,
      storage hoisted from initializers), `sq_globals_self` (thread-local) cached in a local `sq_gl`
      by each function that uses globals; `sq_globals_size`/`sq_globals_set` exported for 2.4.
      Error constants became shared `static const` values (and taking their address is now an
      error: it allowed writing to a constant). HTTP throughput on macOS within noise of before.
- [x] 2.3 Task pools per shard: descriptors, task tables and stacks set up per shard at
      start (mmap, so `threads auto` works); generated code names pools by index. — done: generated pools are templates with an `index`; each shard
      has its own copies (shard 0 uses the templates), and `sq_go` maps the template to the current
      shard's pool.
- [x] 2.4 `threads N` (or `auto`) in package `main`: start N shard threads, each with its own
      signal stack, running `main`. The process exits when every shard's `main` has returned, or
      at the first panic. Budget × N (with `workers`, × workers × threads). — done: `shard_new` lays out a shard in one mmap (pools, task
      tables, timer heap, package variables, signal stack); the main thread runs shard 0. A shard
      whose `main` returns keeps serving its tasks until every shard's `main` has returned
      (stopping it at once stranded connections queued on its socket). `runtime.Shard()` and
      `runtime.Shards()` added. The stack proof now lists the scheduler-only runtime functions
      explicitly (inlining had moved netpoll's 8 KiB frame into the task budget).
- [x] 2.5 Listening: `SO_REUSEPORT` per shard on Linux; on macOS shards share one listening socket
      (a process-wide table keyed by address), since its `SO_REUSEPORT` doesn't balance. — done, both OSes, through a process-wide table: a shard listens
      on an address once (a second Listen is "address in use", as with one thread; otherwise on
      Linux the extra socket joined the SO_REUSEPORT group and received connections nobody
      accepted); Linux gives each shard its own socket, macOS shares one; port 0 is never shared.
- [x] 2.6 Tests: shards serve concurrently; stress tests; the no-heap test with N shards; budget
      × N — done: `TestThreads` (spread, per-shard counters exact under 64 concurrent clients,
      listen rules, SIGTERM, panic), `TestNoHeapAtRuntime` with threads 1 and 4, error tests.
- [x] 2.6b The runtime under ThreadSanitizer. Needs fiber annotations
      (`__tsan_switch_to_fiber`) around task context switches, or TSan misreads them. — done:
      fiber per task slot plus one per scheduler, switched at park/yield/exit and in
      `schedule` (compiled out of normal builds); `Options.Sanitize`; `TestThreadsRace` serves
      concurrent load on 4 shards with no reports on macOS and Linux, and reports races when the
      shards are made to share package variables.
- [x] 2.7 Benchmark against `workers` and Go. — done (`bench/workers-linux.sh`, Linux, 6 cores shared with the
      load generator): threads 3 ≈ 815k / 577k / 207k req/s for health / note / mix vs workers 3
      836k / 660k / 205k and Go GOMAXPROCS=3 468k / 255k / 92k. `/notes/1` is ~13% behind workers,
      unexplained so far; a guess (not measured) is SQLite's process-wide per-file lock state,
      which processes don't share.
- [x] 2.8a Constant composites — **decided 2026-10-14: both 2.8a and 2.8b.** `const T = [N]E{...}`
      / `Config{...}` built from constants is read-only data in the binary, shared by every
      shard (same machinery as error constants). Writes, `&`, slicing and pointer-method calls on
      them (or their elements/fields) are errors. Done: `testdata/run/const_composite.sq`,
      `testdata/errors/const_composite.sq`, `const_data.sq`; builds with clang and gcc.
- [x] 2.8b `shared var x = expr`: initialized once, on shard 0 in its main task (so the
      initializer may do I/O) before the other shards start; read-only afterwards with the same
      rules as constants. Types limited to what can't be written through (numbers, strings, bools,
      errors, arrays and structs of them), since there's no way to freeze what a slice or pointer
      points to. — done: shared vars are one static copy;
      `sq_program_main` initializes shard 0 (shared vars guarded by `first`), then
      `sq_start_shards` has shard 0's scheduler create the other threads. Tests:
      `testdata/run/shared_var.sq` (fails if shards start before init), error tests, and
      `TestThreadsRace` now reads a shared var from every shard.
- [x] 2.9 Cross-shard channels. — done: `shared var c chan T cap N`, set up eagerly by shard 0;
      operations under the channel's mutex; a waiter of another shard goes to that shard's remote
      queue and its poller is woken (EVFILT_USER / eventfd); a shard with shared waiters sleeps
      instead of reporting deadlock. Tests: `testdata/run/shared_chan.sq` (1000 jobs over 4 shards,
      unbuffered ping-pong), `TestSharedChanRace` (TSan, 5 runs), error tests.

**Done when** one process uses every core with no data races by construction, and matches or
beats stage 1. — **Stage 2 done 2026-10-14.** Races: none by construction (per-shard state) and
none under TSan; throughput close to stage 1 (see 2.7).

## Stage 3: blocking C calls off the scheduler

A blocking C call (a SQLite query, file I/O on a slow disk) stops every task on its thread.

- [x] Mark externs that may block (e.g. `extern ... blocking`), and run those calls on a small
      pool of helper threads while the calling task parks. — `extern ... stack N blocking`:
      the call site calls a generated wrapper that packs arguments on the task's stack and
      calls `sq_blocking`; 2 helper threads per scheduler thread (stack proven from the largest
      blocking extern, counted in the memory report) run it and wake the task through its
      shard's remote queue (2.9). `sq_gmutex`: a task lock for C state shared across a wait.
- [x] Mark the SQLite shim's calls; measure the notes mix scenario before and after. — **first
      attempt (every Step on a helper) was a disaster**: threads 3 note 577k → 76k req/s, mix
      207k → 8.6k: each statement held its connection across two thread handoffs, serializing the
      thread. **Now Step runs inline with a busy handler that declines to wait; only a Step that
      found the database locked runs again on a helper** (safe: SQLite gave up at a lock it would
      have waited for). Open/Exec/Prepare/Finalize/Close always run on a helper. Each connection
      has a task lock held from a statement's first Bind/Step to its Reset. Linux, 6 cores
      shared with the load generator: threads 3 mix 207k → 249k req/s and p99 4.47 → 1.99 ms;
      workers 3 mix 205k → 284k, p99 4.15 → 1.91 ms; one process 165k → 173k; health and note
      unchanged (Go: 95k mix).
- [x] Tests: a slow blocking call doesn't delay other tasks; the no-heap test still passes.
      — `TestBlocking` (testdata/blocking: two 300ms calls overlap and a ticker keeps ticking; 1
      and 4 threads, TSan), `TestSQLiteThreads` (4 threads × 8 tasks insert and read back on
      one database; fails with the connection lock removed; TSan), no-heap test (uses SQLite).
- [x] Slow statements (not just lock waits) — **decided 2026-10-14: move the task mid-call.**
      `sq_detach()` / `sq_reattach()` in `sq_rt.h`: C code that turns out slow moves its task
      (its stack, mid-call) to a helper thread and back; the scheduler hands it over once its
      context is saved. SQLite calls it from its progress handler (over 100µs, checked every 1000
      VM steps) and busy handler (then retries the lock at once and backs off from 50µs), which
      replaces the inline-then-retry Step. Reads and writes, inside transactions too, with no
      restart. Tests: `TestSQLiteSlow` (a 2M-row read and a 1M-row insert in a transaction while a
      ticker keeps ticking; fails without the detach; TSan), `TestDetach` (plain C, no blocking
      externs; 1 and 4 threads; TSan). Linux mix, two runs: threads 3 261k/247k req/s, p99 1.42ms
      (was 1.99ms); workers 3 223k/254k, p99 ~2ms; health and note unchanged.

## Out of scope: Go-style M:N

Tasks migrating between threads with work stealing would need locks or atomics in every
channel, map and pool, user-visible mutexes and a memory model, and a race detector. It removes
rule 10 and changes the language's character. Revisit only if parallel computation over shared
in-memory data becomes a goal; explicit constructs (a parallel `for` over arrays) would be the
first thing to consider instead.

---

## Progress log

- 2026-10-14: plan written. Starting stage 1.
- 2026-10-14: stage 1 done. Found on the way: workers opening one SQLite file at once failed with
  "database is locked" — std/sqlite now sets a 5s busy timeout and `Exec` retries busy statements
  outside transactions (SQLite skips the busy handler where it could deadlock). Next: stage 2's
  design decisions.
- 2026-10-14: stage 2 decisions recorded (main on every shard; per-shard package variables);
  steps 2.1–2.9 written; 2.1 done.
- 2026-10-14: 2.2 done.
- 2026-10-14: 2.3–2.7 done; 2.6b (TSan) done after. Next: 2.8 shared immutable data, 2.9 cross-shard
  channels.
- 2026-10-14: 2.8a (constant composites) and 2.8b (shared var) done. Next: 2.9 cross-shard channels.
- 2026-10-14: 2.9 done; stage 2 complete. Next: stage 3 (blocking C calls off the scheduler).
- 2026-10-14: `SQUARK_THREADS` override added (opt in with `threads auto`, then auto by default).
- 2026-10-14: per-thread variable note and `thread var`.
- 2026-10-14: stage 3 done: blocking externs, helper threads, SQLite waits off the scheduler.
- 2026-10-14: slow SQLite statements move to a helper mid-call (sq_detach).
