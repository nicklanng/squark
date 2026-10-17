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
  - Tests: a run test with a fake DNS server on 127.0.0.1 (a squark goroutine answering UDP),
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

- [ ] **2.1 Deadlines.** One mechanism for request timeouts, outbound call timeouts and shutdown.
  ```squark
  with deadline 2s {
      resp := client.get("/stock")!       // net.Timeout once 2s have passed
  }
  ```
  - A per-goroutine deadline stack; nested blocks take the earlier one.
  - Every blocking operation that has an error result (net reads, writes, dial and DNS; http
    client; `sqlite` busy waits) returns `net.Timeout` once the deadline passes. The runtime arms a
    timer on the goroutine's existing timer heap.
  - On shutdown (1.3), every goroutine's deadline becomes at most the remaining grace period.
  - `runtime.deadline_left() -> time.Duration`.
  - Open: channel operations have no error result. Proposal: deadlines don't affect `<-ch`, and
    `select` gains no new case; code that must time out on a channel already uses `select` with a
    timeout, which can read `deadline_left()`. Alternative: a `ch.recv_by(deadline)` method
    returning `(T, bool)`.
  - Open: `blocking` externs run on a helper thread and can't be interrupted; they only check
    the deadline before starting.
  - Spec: §4 (`with deadline`), §9 runtime behaviour. Tests: run tests for nesting and expiry,
    errors tests for misuse, an http test where a slow upstream gets cut off.

- [ ] **2.2 Overload that's an error, not a crash.**
  - `start t(args) else { ... }` runs the block instead of waiting when the pool is full. The
    http server uses it to answer `503` with `Retry-After` and close.
  - Allocation that can fail: `x := new(Node) in a else { return ... }` (also `make`, `append`,
    `+`, conversions). This is the open "try ... in a" item from the README.
  - Without `else`, behaviour is unchanged (wait, or panic).
  - Open: is `else` the right word, given that `!` is how errors propagate? Alternative:
    `new(Node) in a!` with a `ArenaFull` fault, which reads as "can fail" everywhere else. `else`
    allows recovery without a fault value; `!` composes with existing error handling.
  - Spec: §4, §5, §7 error table. Tests: run tests for both; http test that floods a pool of 2.

- [ ] **2.3 The `program` block as a resource contract.**
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

- [ ] **2.4 Count the memory the budget misses, so `limits.memory = budget` is true.**
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

- [ ] **2.5 Isolated tasks.** `task serve_conn(...) pool 512 isolate`: a panic ends only that task
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

- [ ] **3.1 Metrics.** Everything worth watching is known at compile time: pools, arenas, stacks,
  the budget. `runtime` exposes it (pool in use / max, arena high-water marks, observed stack
  depth against the proof, connections idle, budget). `http.handle_metrics("/metrics")` serves it
  in Prometheus text format. Users add counters and gauges as package variables.
- [ ] **3.2 Structured logs.** `std/log`: JSON lines to stdout, fixed-size records, levels;
  `log.info("served", "path", r.path, "ms", ms)`. Never allocates; truncates instead.
- [ ] **3.3 Probe endpoints that don't starve.** `http.handle_probe("/healthz", fn)` serves
  probes from a small separate task pool, so a full `serve_conn` pool doesn't fail liveness.
- [ ] **3.4 Static builds.** `squark build -static` links statically (musl on Linux; libsqlite3
  static too), so the image can be `FROM scratch`. `examples/` gets a Dockerfile.
- [ ] **3.5 TLS for outbound calls.** BearSSL (no malloc, caller-provided buffers) vendored as a
  C library with a declared stack; `http.dial_tls`. Inbound TLS later; inside a cluster a mesh
  often terminates it.
- [ ] **3.6 Postgres client.** The wire protocol in squark, over `net`, with fixed buffers and a
  per-thread connection like `sqlite`.
- [ ] **3.7 A showcase service.** `examples/svc`: two services calling each other by name, with
  deadlines, metrics, probes, `-manifest` output and Kubernetes manifests; a script that runs it
  in kind.

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
