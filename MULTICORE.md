# Multi-core plan

squark runs on one OS thread today (spec rule 10: single-threaded and cooperative). One core
serves ~226k req/s in the notes benchmark while the machine has ten. This file is the plan for
using more cores, in stages, and the log of working through it. Tick items as they land, with the
commit and the test that covers them.

**Why staged.** Stage 1 needs almost no runtime change and shows how far throughput scales.
Stage 2 is the real design. Stage 3 removes the other single-thread bottleneck (blocking C calls).
Go-style M:N scheduling is deliberately out of scope (see the end).

**What makes this tractable.** The lifetime checker already forbids `go` arguments and channel
sends that reference stack or arena memory, so the only memory goroutines can share is static
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
timers, poller, goroutine pools and idle set. Goroutines never move between shards, so within a
shard rule 10 still holds and no locks are needed. Shards talk only through cross-shard channels.

Design decisions to settle first (record the answers here):

- [x] How work reaches each shard: run `main` on every shard (like stage 1, one address space),
      or start shards explicitly (e.g. `go on(shard) f()`, a per-shard entry point)? — **decided
      2026-10-14: `main` runs on every shard.** Programs written for `workers N` work unchanged.
- [x] Package variables: per-shard copies by default (thread-local), with a way to declare
      shared data that is immutable after initialization, checked by the compiler. — **decided
      2026-10-14: per-shard copies by default**; rule 10 keeps holding per shard.
- [ ] Cross-shard channels: a separate type or a property of package-level channels? Element
      rules as today (no stack/arena references), plus a lock-free queue and a cross-thread wakeup
      (eventfd on Linux, EVFILT_USER on macOS).
- [ ] Which externs are safe to call from any shard (C libraries with global state).

Implementation steps (each lands with the full suite passing on macOS and Linux):

- [x] 2.1 Runtime state into `struct sq_shard`, reached through one thread-local pointer, with a
      single shard: scheduler context, current goroutine, run queue, counters, poller fd and
      change list, timer heap, idle set, output buffer, `fd_read_kind`, the HTTP date cache. No
      behaviour change; check the benchmarks for the cost of thread-local access. — done: `sq_shard` + `_Thread_local sq_shard *sq_self`; notes HTTP throughput unchanged on macOS (224k/201k/153k vs 226k/202k/153k)
- [ ] 2.2 Generated code: package variables (and package arenas, map/channel storage) into a
      per-shard globals struct, reached through the shard; `sq_init_globals` runs per shard.
      Static initializers that hold addresses move into the per-shard init.
- [ ] 2.3 Goroutine pools per shard: descriptors, goroutine tables and stacks set up per shard at
      start (mmap, so `threads auto` works); generated code names pools by index.
- [ ] 2.4 `threads N` (or `auto`) in package `main`: start N shard threads, each with its own
      signal stack, running `main`. The process exits when every shard's `main` has returned, or
      at the first panic. Budget × N (with `workers`, × workers × threads).
- [ ] 2.5 Listening: `SO_REUSEPORT` per shard on Linux; on macOS shards share one listening socket
      (a process-wide table keyed by address), since its `SO_REUSEPORT` doesn't balance.
- [ ] 2.6 Tests: shards serve concurrently; stress tests; the runtime under ThreadSanitizer; the
      no-heap test with N shards; budget × N.
- [ ] 2.7 Benchmark against `workers` and Go.
- [ ] 2.8 Shared immutable data: a way to declare package data shared by all shards, immutable
      after initialization, checked by the compiler.
- [ ] 2.9 Cross-shard channels.

**Done when** one process uses every core with no data races by construction, and matches or
beats stage 1.

## Stage 3: blocking C calls off the scheduler

A blocking C call (a SQLite query, file I/O on a slow disk) stops every goroutine on its thread.

- [ ] Mark externs that may block (e.g. `extern ... blocking`), and run those calls on a small
      pool of helper threads while the calling goroutine parks.
- [ ] Mark the SQLite shim's calls; measure the notes mix scenario before and after.
- [ ] Tests: a slow blocking call doesn't delay other goroutines; the no-heap test still passes.

## Out of scope: Go-style M:N

Goroutines migrating between threads with work stealing would need locks or atomics in every
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
