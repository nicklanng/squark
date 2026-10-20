# Roadmap: services with resource contracts

Goal (2026-10-17): make squark the obvious language for a network service that runs under resource
limits (a Kubernetes pod, a Docker container, a systemd unit with `MemoryMax=`). squark already
knows its worst-case memory at build time; these items make that number match what the kernel
charges, make the runtime respect the limits it's given, and make overload and shutdown safe.

Nothing here is Kubernetes-only. Keywords name the resource (`deadline`, `shutdown`, `sockbuf`),
never the platform. Kubernetes is the showcase; `examples/` gets a service with manifests.

Work top to bottom; each item lists its tests and its "done when". Tick the box and add a one-line
note (what changed, which test covers it) when an item is done, as in BUGS.md. Language items say
what in `docs/spec.md` changes; settle the open questions before starting them.

---

## 1. Runtime and std blockers

These are what a real deployment hits first. No language changes.

- [x] **1.1 DNS and IPv6 in `net.dial`.** — done: `std/net/dns.sq` (resolver: hosts file, resolv.conf
  search/ndots/timeout/attempts, A+AAAA in parallel over UDP, TCP on truncation), `std/net/ip.sq`
  (`IP`, `parse_ip`), runtime `sq_dial` (TCP/UDP, v4/v6), `listen(":port")` dual-stack;
  tests: `TestLookupIP` (fake name servers), `TestDialIPv6`, lookups added to `TestNoHeapAtRuntime`.
  Not done: the run test with a squark DNS server (needs a UDP listen API; the Go fake covers it)
  and the in-container example (3.7). Today `parse_addr` (`runtime/sq_rt.c:2128`) is a bare
  `inet_pton(AF_INET)`, so `orders.default.svc.cluster.local:8080`, `localhost` and `[::1]:80` all
  fail.
  - `getaddrinfo` is out: it allocates (breaks `TestNoHeapAtRuntime`) and blocks a thread.
  - Instead, a resolver in squark (`std/net/dns.sq`): read `/etc/hosts` and `/etc/resolv.conf`
    (`nameserver`, `search`, `options ndots:N timeout:N attempts:N`), send A and AAAA queries over
    UDP on the poller, try search domains in resolv.conf order, fixed-size buffers only.
  - Runtime: UDP sockets (`sq_udp_dial`, send/recv on the poller), `sockaddr_in6`, and
    `listen` on `[::]`.
  - `net.lookup_host(name, out IP[]) -> int !` is public; `dial` uses it.
  - Tests: a run test with a fake DNS server on 127.0.0.1 (a squark task answering UDP),
    covering search domains, ndots, NXDOMAIN and truncation; `/etc/hosts` and `localhost`;
    IPv6 loopback dial and listen. Linux in Docker (real resolv.conf).
  - Done when: an example dials a service by name inside a container.

- [x] **1.2 `threads auto` and `workers auto` read cgroup CPU limits.** — done: runtime `cpu_count`
  (affinity, cgroup v2 `cpu.max` up the tree, v1 `cfs_quota_us`; quota rounded up, at least 1);
  test: `TestThreadsAutoLimits` (taskset on Linux). Checked by hand in Docker: `--cpus 1.5` → 2,
  `--cpus 3` → 3, `--cpus 0.2` → 1, `--cpuset-cpus 0` → 1, `--cpuset-cpus 0,1 --cpus 4` → 2.
  (Note: `workers auto` with `threads auto` still gives CPUs × CPUs shards, as before.) Today they use
  `sysconf(_SC_NPROCESSORS_ONLN)` (`runtime/sq_rt.c:2540`, `:2933`), which is the node's core count.
  - Use the smallest of: `sched_getaffinity` count, cgroup v2 `cpu.max` (quota/period, rounded
    up), cgroup v1 `cpu.cfs_quota_us`/`cpu.cfs_period_us`. Go 1.25 does the same for `GOMAXPROCS`.
  - `SQUARK_THREADS` still wins.
  - Tests: a C-level unit test of the parser on sample files; Docker run with `--cpus 2` on a
    bigger host reports 2 shards (`runtime.shards()`).

- [x] **1.3 Graceful shutdown on SIGTERM.** — done, opt-in rather than for every program (a CLI
  tool must still die on Ctrl-C): `runtime.stop_gracefully()` (which `http.serve` calls) and
  `runtime.stopping()`; blocked `accept`s get `net.Stopping`; the http server finishes requests
  (`Connection: close`), returns nil from `serve`; the watchdog enforces `SQUARK_SHUTDOWN`
  (default 10s, then status 1); a second signal ends it at once. Idle keep-alive connections close
  with the process. tests: `TestGracefulStop`, `TestGracefulStopLimits`, `TestStopSignalDefault`;
  `TestWorkers` and `TestThreads` now expect status 0 after SIGTERM. Plan below as first written: Today a single-process program has no handler, so
  SIGTERM drops requests in flight.
  - First cut, before deadlines exist: on SIGTERM or SIGINT, close every listener, close idle
    keep-alive connections, let running tasks finish, exit 0 when they have; exit after a grace
    period (default 10s, `SQUARK_SHUTDOWN`) regardless. A second signal exits at once.
  - `runtime.stopping() -> bool` for loops that should end early.
  - `workers` mode: the supervisor already forwards; workers now drain too.
  - 2.1 (deadlines) later caps every task at the grace period; 2.3 makes the period a declaration.
  - Tests: Go test in `internal/driver` that starts a server, holds a slow request open, sends
    SIGTERM, checks that the request completes, new connections are refused, and the exit is 0.

---

## 2. Language

Each needs a design pass first: the open questions below are decided in this file before code.

- [x] **2.1 Deadlines.** — done: `with deadline d { }` (DeadlineStmt; a C cleanup variable restores
  the outer deadline on every exit), a per-task deadline that `wait_fd` turns into a timer,
  `net.Timeout` from read, write, accept, dial and lookups, `runtime.deadline_left()`; a
  graceful stop caps every task's I/O at 90% of the grace period (blocked ones get a timer).
  **Decided:** channels, `time.sleep` and `blocking` externs are not affected (select with
  `time.after` and `deadline_left()` cover channels; C calls can't be interrupted); a task started
  in the block doesn't inherit the deadline; the sooner of nested deadlines wins. tests:
  `testdata/run/deadline.sq`, `testdata/errors/deadline.sq`, `TestDeadlineUpstream`, grammar.
  Plan as first written: One mechanism for request timeouts, outbound call timeouts and shutdown.
  ```squark
  with deadline 2s {
      resp := client.get("/stock")!       // net.Timeout once 2s have passed
  }
  ```
  - A per-task deadline stack; nested blocks take the earlier one.
  - Every blocking operation that has an error result (net reads, writes, dial and DNS; http
    client; `sqlite` busy waits) returns `net.Timeout` once the deadline passes. The runtime arms a
    timer on the task's existing timer heap.
  - On shutdown (1.3), every task's deadline becomes at most the remaining grace period.
  - `runtime.deadline_left() -> time.Duration`.
  - Open: channel operations have no error result. Proposal: deadlines don't affect `<-ch`, and
    `select` gains no new case; code that must time out on a channel already uses `select` with a
    timeout, which can read `deadline_left()`. Alternative: a `ch.recv_by(deadline)` method
    returning `(T, bool)`.
  - Open: `blocking` externs run on a helper thread and can't be interrupted; they only check
    the deadline before starting.
  - Spec: §4 (`with deadline`), §9 runtime behaviour. Tests: run tests for nesting and expiry,
    errors tests for misuse, an http test where a slow upstream gets cut off.

- [x] **2.2 Overload that's an error, not a crash.** — done: `start t(args) else { ... }`
  (`sq_try_go`), `v, ok := x in a` for every allocating form; `http.shed_load` (off by default)
  answers 503 + `Retry-After: 1` through a `shed` task pool (16) when serve_conn is full.
  **Decided:** comma-ok rather than `else` or `!` for allocation (it works in functions without an
  error result, like handlers, and matches `v, ok := m[k]`); only the expression's own allocations
  fail softly (they get a negative line; `sq_panic_arena` longjmps to the expression's
  `_setjmp`), those inside functions it calls still panic, so no callee is left half-done;
  shedding is opt-in because waiting is the safer default for a lone server. tests:
  `testdata/run/overload.sq`, `TestShedLoad`. Plan as first written:
  - `start t(args) else { ... }` runs the block instead of waiting when the pool is full. The
    http server uses it to answer `503` with `Retry-After` and close.
  - Allocation that can fail: `x := new(Node) in a else { return ... }` (also `make`, `append`,
    `+`, conversions). This is the open "try ... in a" item from the README.
  - Without `else`, behaviour is unchanged (wait, or panic).
  - Open: is `else` the right word, given that `!` is how errors propagate? Alternative:
    `new(Node) in a!` with a `ArenaFull` fault, which reads as "can fail" everywhere else. `else`
    allows recovery without a fault value; `!` composes with existing error handling.
  - Spec: §4, §5, §7 error table. Tests: run tests for both; http test that floods a pool of 2.

- [x] **2.3 The `program` block as a resource contract.** — done: `shutdown 20s` (ms, s, m;
  `SQUARK_SHUTDOWN` overrides), the proven worst case linked into the binary (`sq_worst_case`:
  link, measure, relink), a start-up check of cgroup `memory.max` / v1 `limit_in_bytes`, and
  `squark build -manifest k8s|systemd`. **Decided:** memory below the worst case refuses to start
  (status 1) but CPU below `threads N` only warns, since CPU is throttled, not killed; the
  manifest's memory limit is worst case + max(10%, 16 MiB) until 2.4 counts sockets and C
  libraries, with requests = limits (Guaranteed QoS) and a stop timeout of grace + 5s; no
  manifest (and no memory check) with `threads auto`/`workers auto`, whose worst case isn't known.
  tests: `TestManifest`, `TestShutdownDirective`, `testdata/errors/shutdown.sq`, grammar; checked
  by hand in Docker: `--memory 32m` refuses a 48.4 MiB program, `--memory 64m` runs it,
  `SQUARK_IGNORE_LIMITS=1` overrides, `threads 4` under `--cpus 2` warns. Plan as first written:
  ```squark
  program {
      budget 128M
      threads 2
      shutdown 20s
  }
  ```
  - `shutdown D` sets the grace period from 1.3.
  - At start-up on Linux, the runtime reads cgroup `memory.max` and `cpu.max` and refuses to start
    (exit 1, one line naming both numbers) if `memory.max` is below the proven worst case or the
    CPU quota is below `threads`. `SQUARK_IGNORE_LIMITS=1` overrides. No cgroup or not Linux: no
    check.
  - Toolchain: `squark build -manifest` prints the matching Kubernetes `resources:` and
    `terminationGracePeriodSeconds` (and, as a second format, systemd `MemoryMax=`/`CPUQuota=`).
  - Spec: §2 program block, §10 toolchain. Tests: driver test for `-manifest`; Docker run with
    `--memory` below and above the budget.

- [x] **2.4 Count the memory the budget misses, so `limits.memory = budget` is true.** — done:
  `connections N` (a process-wide socket cap: accept waits, dial and listen return
  `net.TooManyConnections`, a listener holds `min(128, N/16)` for its backlog), `sockbuf N`
  (SO_RCVBUF/SO_SNDBUF on every socket), `heap LIB N` (a module's is a default the program
  overrides; `sqlite` enforces it with `sqlite3_hard_heap_limit64`, 16M by default); the report
  counts N × (4 × sockbuf + 4 KiB) and the heaps, per worker; the manifest's headroom drops to
  max(5%, 8 MiB) when nothing is left uncounted. **Decided:** pools can't bound sockets (idle
  keep-alive connections have no task), so the cap is on sockets; backlogs count against it.
  **Measured** (Docker, Linux arm64): `connections 1100`, `sockbuf 16K`, 64 KiB responses to
  clients that never read: 582 sockets full, cgroup `sock` 27.3 MiB (~47 KiB each, under the 68 KiB
  counted), peak 72.4 MiB against a worst case of 110.1 MiB and a limit of 119 MiB, no OOM. tests:
  `testdata/run/connections.sq`, `TestCountedMemory`, `testdata/errors/counted.sq`. Plan as first
  written:
  - `pool http.serve_conn 512 sockbuf 64K`: the runtime sets `SO_RCVBUF`/`SO_SNDBUF` on every
    connection the pool serves, and the budget adds pool × (kernel's doubling of) sockbuf.
    cgroup v2 charges socket memory to the pod.
  - `link "sqlite3" heap 8M`: a C library declares a cap. The budget counts it; for `sqlite`,
    the package calls `sqlite3_hard_heap_limit64` with it. A `link` with no `heap` stays
    "not counted" in the report.
  - Open: how much headroom for page cache and the kernel's per-socket overhead beyond the
    buffers. Measure in Docker under the load generator and document a rule of thumb.
  - Tests: budget report test; a Docker load test where peak `memory.current` stays under the
    reported worst case.

- [x] **2.5 Isolated tasks.** — done: `task t() pool N isolate`; in a program with one, every
  function with deferred calls pushes an `sq_unwind` frame (`_setjmp`) and pops it on return; a
  panic in an isolate task prints as usual, then unwinds frame by frame running deferred
  calls, ends at the task's entry (slot returned) and releases the task locks it held.
  `http.serve_conn` is isolate and closes the connection of a handler that panicked.
  **Decided:** deferred calls do run (Go's semantics; resources are released with `defer`, so
  no fd-ownership tracking is needed); no checker restriction on writing package or thread
  variables (every HTTP handler would hit it, since handlers are called indirectly; Go's net/http
  recovers handler panics the same way), documented instead; faults in C code and stack overflows
  stay fatal; the cost is a `setjmp` per call of a function with deferred calls, only in programs
  with an isolate task. Found while testing: a deferred call's pointer argument must be a
  volatile *variable* (`__typeof__(T) volatile`), not a pointer to volatile, or longjmp restores
  it to its value at entry. tests: `testdata/run/isolate.sq`, `testdata/errors/isolate.sq`,
  `TestIsolateHandlerPanic`, `TestIsolateReleasesLocks`, grammar. Plan as first written: `task serve_conn(...) pool 512 isolate`: a panic ends only that task
  instance. The region checker already proves nothing outside the task points into its stack or
  arenas, so dropping them is memory-safe.
  - Checker: an `isolate` task (and everything it calls) may not write `thread` or package
    variables, or hold a lock across a call that can panic, so a panic can't leave torn shared
    state. Channels are fine.
  - Runtime: the task's entry is a recovery point; on panic, print the panic as today, close the
    file descriptors the task opened (the runtime tracks an owner per fd slot), release its pool
    slot and stack.
  - Open: do deferred calls run? Go runs them. squark's defers are generated inline; running
    them on panic means emitting a cleanup path per function. Proposal: not in the first cut,
    which is why fds are closed by the runtime.
  - Spec: §4 tasks, §7 memory rules, §9. Tests: errors tests for each forbidden write; a run test
    where one task panics and the others finish; ASan run.

---

## 3. Standard library and tooling

- [x] **3.1 Metrics.** — done as planned: `std/metrics` (`counter`, `gauge`, `write_text`; 64
  cells, about 14 KiB of static data), process-wide pool stats, `http.handle_metrics`,
  `http_requests_total{code}` and `http_handler_panics_total`. **Decided:** registering panics
  on misuse (table full, kind clash), as Prometheus' MustRegister does, so a package variable can
  hold a metric. tests: `testdata/run/metrics.sq`, `TestServeAdmin`. Plan:
  - Pool counters (`live`, `peak`, `started`) become relaxed atomics, so any thread can read every
    shard's (a scrape lands on one shard under `threads N`, and must report the process).
  - `std/metrics`: counters and gauges in a fixed runtime-owned table of atomic cells (C owns
    them, so threads sharing them breaks no squark rule): `metrics.counter(name, labels, help)`
    returns a handle (registering is idempotent by name and labels, so every shard's `main` can
    do it), `c.add(n)`, `g.set(v)`. `metrics.append_text(dst)` writes the Prometheus text format:
    the program's metrics plus the runtime's (pools, open sockets and the connections limit,
    idle connections, the worst case and budget, threads, whether a stop has begun).
  - `http` counts requests by status class and handler panics (isolate).
- [x] **3.2 Structured logs.** — done: `std/log`; an `Entry` is a value on the caller's stack
  (a shared buffer would mix lines when a field's value blocks), `log.err` because `error` is a
  type; values are cut by their real escaped length. test: `TestLog`. Plan: `std/log`: JSON lines to stdout, fixed-size records, levels;
  `log.info("served", "path", r.path, "ms", ms)`. Never allocates; truncates instead.
- [x] **3.3 An admin port that doesn't starve.** — done: `http.serve_admin`, `set_ready`,
  `net.Listener.accept_always` (accepts through a graceful stop, so `/readyz` reports `stopping`
  until the exit). test: `TestServeAdmin`. Plan: Not `handle_probe` on the main listener: a
  request is only known to be a probe once read, and by then it holds a `serve_conn` slot. Instead
  `http.serve_admin(":9090")` serves `/healthz` (alive), `/readyz` (ready: 503 during a graceful
  stop, so the endpoint is removed before the server stops accepting, and while the program says
  `http.set_ready(false)`) and `/metrics`, from its own listener and a small task pool of its
  own, so a full `serve_conn` pool can't fail liveness.
- [x] **3.4 Static builds.** — done: `squark build -static` (Linux; an error on macOS, which has no
  static libc); glibc static works because `net` never calls getaddrinfo/NSS. musl wasn't needed.
  test: `TestStatic` (no PT_INTERP, no shared libraries, SQLite included; it runs). Plan: `squark build -static` links statically (musl on Linux; libsqlite3
  static too), so the image can be `FROM scratch`. `examples/` gets a Dockerfile.
- [-] **3.5 TLS for outbound calls.** — dropped (2026-10-20): not needed. No third-party
  library is vendored into the repo, and TLS is left to a service mesh or a sidecar, which inside
  a cluster often terminates it anyway.
- [x] **3.6 Postgres client.** — done: `std/postgres` (the v3 protocol in squark: trust,
  cleartext and SCRAM-SHA-256 authentication, the extended protocol with text parameters,
  `Rows`, SQLSTATE codes, a per-thread `Pool` of up to 16) and `std/crypto/sha256` (SHA-256,
  HMAC). No TLS (3.5). tests: `testdata/run/sha256.sq` (FIPS 180-4, RFC 4231 vectors),
  `TestPostgres` via `scripts/test-postgres.sh` (Postgres 16 in Docker, SCRAM). Plan: The wire protocol in squark, over `net`, with fixed buffers and a
  per-thread connection like `sqlite`.
- [x] **3.7 A showcase service.** — done: `examples/shop` (one program, roles stock and orders;
  self-test in the suite), Dockerfile (static, `FROM scratch`, 1.5 MB image), docker-compose.yml,
  k8s.yaml. **Checked in Docker Compose:** orders reached stock by name through Docker's DNS,
  `slow` answered 504 at 309 ms, probes and metrics on :9090, JSON logs, `--memory 32m` refused
  (worst case 60 MiB), `docker compose stop` exited 0 for both. k8s.yaml was only parsed, not
  applied: there was no local cluster (and the default kube context is a real one). Plan: `examples/svc`: two services calling each other by name, with
  deadlines, metrics, probes, `-manifest` output and Kubernetes manifests; a script that runs it
  in kind.

- [x] **3.8 Retries with backoff** (asked for on 2026-10-17). — done: `std/retry`: `backoff(Policy)`
  and `while b.next() { ... }` (no closures, so the loop is the caller's), exponential waits with
  full jitter, `after` for a Retry-After, stopping early when the wait would pass the deadline or
  a graceful stop has begun; `transient(err)` (timeouts, refused/reset/unreachable connections,
  classified in C with the system's own errno numbers) and `transient_status(code)`.
  `examples/shop` retries a failed dial (never the POST). test: `testdata/run/retry.sq`.

## 4. Tooling for writing squark every day (2026-10-20)

What someone writing squark daily misses first: a test command, a formatter, and an editor that
shows the compiler's errors as they type.

- [x] **4.1 `squark test`.** — done as decided below. `assert` also takes a message
  (`assert(cond, "why")`) and prints its condition as written (comments dropped, one line);
  each test runs in the package's directory with its own `TMPDIR`. `examples/jsonq` has tests,
  the tour's errors chapter a lesson. tests: `TestSquarkTest`, `TestSquarkTestProgram`,
  `TestSquarkTestErrors`, `TestAssertMessages`, `TestTestDirs`, `TestExampleTests`,
  `TestCLITest`, `testdata/run/assert.sq`. Decided:
  - Tests are `fn test_NAME()` or `fn test_NAME() -> !` at package level in the package's
    `*_test.sq` files (same header as the package, so a test sees its unexported names). A module
    is tested as well as a program; a program's `main` isn't run.
  - A test fails if it returns an error, panics, or hangs past `-timeout` (1m). Each test runs in
    its own process: a panic ends only that test, package variables start fresh every time, and
    tests can't leak tasks or sockets into each other. A squark binary starts in a millisecond, so
    this costs little, and `-p N` runs N tests at once.
  - `assert(cond)` is a builtin (usable anywhere, not only in tests): it panics with
    `assertion failed: <the condition's source>`, and for a comparison it prints both operands'
    values (strings quoted), so `assert(len(v) == 3)` says what `len(v)` was.
  - `std/testing`: `expect_panic(text)` (the test passes only if it panics with a message
    containing text: the "this must panic" tests doxasq keeps in a shell script) and
    `skip(reason)`. They talk to the runner over file descriptor 3, not stdout.
  - Test builds force one worker and one thread, skip the memory budget (test code isn't the
    program's), and keep each test function out of line so `-v` can report each test's proven
    stack.
  - Flags: `-run regexp`, `-v` (every test, its output and stack), `-asan`, `-timeout d`, `-p N`.
    `squark test dir/...` tests every package under dir with test files.
  - Output: `ok  dir  N tests (time)`; a failure prints the test's output, indented.
  - Tests: `TestSquarkTest` in `internal/driver` (pass, fail by error, by panic, by assert,
    expect_panic, skip, timeout, -run, a module, a program with `threads` and a budget);
    `testdata/run` for `assert`'s messages.
- [x] **4.2 `squark fmt`.** — done: `squark fmt [-l] [-w] [file|dir|dir/...]`. Decided: it works
  on the tokens, not the tree (the AST has no end positions or comments): the line breaks and
  comments stay as written, and it normalizes indentation (blocks, brackets, continued
  statements, `case` level with `switch`, labels one out), blank lines (at most one, none at a
  block's ends), spacing (forced where only one form is right, the source's kept where both are,
  as in `a*b + c` and `T{1}` against `if x {`) and alignment (struct fields, grouped
  declarations and enum members by their names, literal keys, one-line function bodies,
  trailing comments; a comment or blank line starts a new section, as in gofmt). It refuses a
  file that doesn't parse and checks that the output has the input's tokens. std, the examples
  and the tour's programs are formatted. tests: `TestFormatCases` (each rule),
  `TestFormatIdempotent` (std, examples, testdata and doxasq: a fixed point after one pass),
  `TestFormatted`, `TestLessonsFormatted`, `TestCLIFmt`.
- [x] **4.3 A language server.** — done: `squark lsp` (`internal/lsp`, the standard library
  only): diagnostics as you type (the loader takes an overlay of unsaved buffers; each package is
  checked as a test build, so a module and its tests are checked too; checks wait 150ms after a
  keystroke), hover (signature with provenance as `squark doc` shows it, doc comment; fields
  in selectors and struct literals), go to definition (the standard library is written out
  read-only to a temporary directory so its files open), formatting with `squark fmt`.
  `editors/vscode` 0.2.0 starts it with its own small JSON-RPC client (`rpc.js`), not
  Microsoft's `vscode-languageclient`, so the extension has no dependencies. tests:
  `TestServer`, `TestModuleDiagnostics`, `TestPositions` (UTF-16 columns),
  `editors/vscode/test/lsp.test.mjs` (the extension's client against the real server).

## 5. Performance follow-ups (2026-10-20)

Postponed from the SIMD work (2026-10-17).

- [ ] **5.1 AVX2 timing on real x86.** Rosetta and qemu run `-cpu x86-64-v3` binaries but their
  timings don't reflect real hardware: needs an x86-64 machine.
- [ ] **5.2 Loops with other strides** (`i += 2`, `for i := 0; i < n; i += k` with a constant
  k): versioned like stride-one loops.
- [ ] **5.3 Binary growth from loop copies** (about 12%): version only loops whose unchecked copy
  clang vectorizes or that index enough to matter.
- [ ] **5.4 Field and package slices in loops that call functions**: prove the call can't change
  the slice (its callees store nothing into that field or variable).
- [ ] **5.5 Refresh `bench/results/latest.md`** with a full `bench/run.sh`.

---

## Progress log

- 2026-10-17: roadmap written. Starting with 1.1.
- 2026-10-17: 1.1 done. Suite passes on macOS and on Linux (Docker, clang and gcc). Also fixed
  `cmd/squark/main_test.go`, whose test programs were still in Go syntax (broken since fd0638b;
  `go test ./internal/...` doesn't run it). Nothing committed.
- 2026-10-17: 1.2 done (macOS, Linux in Docker). Nothing committed.
- 2026-10-17: 1.3 done; section 1 closed. Full suite passes on macOS (with ASan) and Linux
  (Docker; clang, and gcc for the new tests). Next: section 2, whose open questions need deciding
  first. Nothing committed.
- 2026-10-17: the user delegated section 2's design questions; decisions are recorded per item.
  Section 1 committed (41ade0c). 2.1 done (218a759; fuzz 5000 clean). 2.2 and 2.3 done: full
  suite on macOS (ASan) and Linux, fuzz 5000 clean.
- 2026-10-17: 2.4 and 2.5 done; section 2 closed. Full suite on macOS (ASan) and Linux, fuzz 5000
  clean. Section 3's plan refined (3.1 metrics through runtime-owned atomics, 3.3 an admin port).
- 2026-10-17: 3.1-3.3 done (suite on macOS and Linux, fuzz 3000 clean). Order from here: 3.4
  static builds, then 3.7 the showcase (it exercises everything so far), then 3.5 TLS and 3.6
  Postgres, the two largest.
- 2026-10-17: 3.4 and 3.7 done.
- 2026-10-17: 3.6 done. Left: 3.5 TLS, which needs a decision on vendoring a TLS library (BearSSL
  is in Debian but not Homebrew, so macOS builds would need its source in the repo).
- 2026-10-20: 3.5 TLS deferred: no vendored libraries; a mesh or sidecar handles TLS for now.
- 2026-10-20: 3.5 TLS dropped (not needed). Sections 4 (tooling) and 5 (performance follow-ups)
  added; starting with 4.1.
- 2026-10-20: 4.1-4.3 done (suite on macOS with ASan, fuzz 5000 clean after 4.1). Next: section 5.
