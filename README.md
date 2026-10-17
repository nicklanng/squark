# squark

> **Experimental.** squark is an experimental language exploring programming without a heap. It
> grew out of Go and keeps much of its semantics, with a syntax of its own. Expect breaking
> changes; it isn't meant for production use.

A language where memory is something you can see: there is no heap, every byte a program can use
is accounted for at compile time, and the compiler proves that no reference outlives the memory it
points to.

```squark
program {
    budget 128M              // the build fails if the worst case can exceed this
    pool http.serve_conn 512 // at most 512 connections served at once
}

use http

// GET /hello?name=world
fn hello(w *http.ResponseWriter, r *http.Request) {
    w.text(200, "hello, " + r.query_param("name") + "\n" in r.arena) // per-request arena
}

fn main() {
    http.handle_func("GET /hello", hello)
    if err := http.listen_and_serve(":8080", nil); err != nil {
        panic(err)
    }
}
```

(This is `examples/greet`, without its header comment.)

The compiler is written in Go, emits C, and builds with clang -O3. It produces small native binaries
(the SQLite notes server is about 100 KiB) with an M:1 goroutine scheduler on kqueue (epoll on Linux).

**Docs:** [language reference](docs/spec.md) (precise; written so people and LLMs can write correct
squark) · [standard library](docs/stdlib.md) (generated) · [design rationale](docs/design.md)

## Quick start

```sh
go build -o bin/squark ./cmd/squark
bin/squark run examples/hello
bin/squark build -v -o bin/notes examples/notes   # -v prints the memory breakdown
bin/squark doc examples/notes strings             # functions with their reference provenance
```

Requires Go 1.27+ and clang. The `sqlite` package links Homebrew's libsqlite3; edit the `link`,
`cflags` and `ldflags` lines in `std/sqlite/sqlite.sq` for other setups.

Editor support: `editors/vscode` is a VS Code extension with syntax highlighting (see its README to
install).

## Where memory lives

There are four kinds of memory, and every value lives in one of them:

| | lifetime | example |
|---|---|---|
| static | the whole program | `thread table u8[1M]`, `shared jobs Job chan[16]`, `arena words 8M` |
| a task's stack | a function call | `var buf u8[4K]`, `var seen int[str; 64]` |
| an arena | a `with arena` block | `n := new(Node) in scratch` |
| C libraries | their own business | SQLite's page cache |

Anything that would need a heap in Go, like returning a pointer to a local or storing a reference
somewhere that outlives it, is a compile error that tells you where the memory should come from:

```
main.sq:13:9: cannot return reference to variable x (declared at main.sq:12): it lives in this
function's stack frame (Go would move it to the heap); have the caller pass in the storage instead
main.sq:25:7: a reference to variable inner (declared at main.sq:24) does not live long enough to be
stored in variable outer (declared at main.sq:22) (link stores argument 2 inside argument 1)
main.sq:30:12: a reference to variable local (declared at main.sq:28) does not live long enough to
be stored in arena a (declared at main.sq:14)
```

## Language tour

The basics are familiar from Go: packages, structs, methods, pointers, slices of arrays, multiple
results, `defer`, `switch`, labeled `break`/`continue`, constants, variadic functions,
goroutines and channels, with the same semantics. The syntax is squark's own:

```squark
fn cut(s, sep str) -> (str, str, bool) in s {   // results after ->; `in s`: they point into s
    i := strings.index(s, sep)
    if i < 0 {
        return s, "", false
    }
    return s[:i], s[i+len(sep):], true
}

struct Stack { items int[..64] }

impl Stack {                                      // methods, in impl blocks
    fn push(&self, x int) { self.items.push(x) }  // &self: a pointer receiver; self: a copy
}

for x in xs { }                                   // elements; `for i, x in xs` for indexes too
for i in 0..n { }                                 // ranges
while busy() { }
loop { }
if not done and (a or b) { }
```

Types read left to right with their size after them (`u8[64]`, `int[str; 1024]`), errors propagate
with a `!` after the call, and names are `snake_case` for values and `PascalCase` for types, made
visible to other packages with `pub`. Numbered names are enums, not `iota`: `enum Dir { north,
east, south, west }` is a type with members `Dir.north`... The rest of this section is what
squark adds to Go's model.

### Arenas: scoped dynamic allocation

```squark
with arena scratch 64K {
    words := make(str[], n) in scratch          // run-time sizes are fine in an arena
    root := new(Node) in scratch                // zeroed *Node
    path := dir + "/" + name in scratch         // string concatenation
    words = append(words, w) in scratch         // grows inside the arena instead of panicking
    owned := str(buf[:n]) in scratch            // copying conversions copy into the arena
}                                               // all of it is gone here
```

- `EXPR in A` means allocations made by that expression come from arena `A`. Without an arena, the
  same expression is a compile error with a hint.
- A local arena is part of its stack frame, so the stack proof counts it. Pages are only touched as
  they're used, and allocation is a pointer bump.
- Package-level arenas (`arena words 8M`) are static and bump-only: a place for data that lives
  forever, such as interned strings.
- Arenas can be passed in: `fn build(a arena, depth int) -> *Node` allocates in the caller's arena.
- A full arena panics with its name and size, unless the allocation is written
  `p, ok := new(Node) in a`: then `ok` is false. `a.used()`, `a.left()` and `a.cap()` report usage.
- Every HTTP request gets `r.arena` (16 KiB), which is gone once the response is written.

### Capacity in types: storage and views

A size in the brackets means storage; empty brackets (or none) mean a view of it:

| storage (inline, a value) | view (a reference) |
|---|---|
| `T[N]` | `T[]` |
| `T[..N]`: length varies, capacity N | `T[]` |
| `V[K; N]` | `V[K]` |
| `T chan[N]` | `T chan` |

```squark
var line u8[..256]
line.push('x')                 // panics when full; line.try_push(c) returns false instead
last := line.pop()

var seen int[str; 4096]        // declaring the storage is the allocation
count(seen, "x")               // storage converts to a view (int[str]) implicitly

shared jobs Job chan[16]       // package-level: one channel for every scheduler thread
```

`make` exists only inside arenas. Map and channel storage can't be copied, because they point into
themselves; pass views instead.

### Provenance in signatures

The compiler infers, for every function, where its results point and which arguments it stores
where. You can write that down, and the compiler checks the body against it:

```squark
fn cut(s, sep str) -> (str, str, bool) in s    // results point into s
fn link(a *Node, b *Node in a)                 // b may be stored inside a
fn new(msg str in static) -> !                 // msg must be static memory
fn name() -> str in static                     // the result is static
```

Declarations name which parameters are related, and the body determines the exact pointer levels.
Leaving provenance out is fine: `squark doc` shows what was inferred. Declaring it turns that into
a checked interface, so a later change to the body can't quietly add a new way for references to
escape.

### Tasks: goroutines with proven stacks

```squark
task serve_conn(c net.Conn, h Handler) pool 10000 stack 256K {
    ...
}

start serve_conn(c, h)
```

Only tasks can be started. `isolate` on a task makes a panic in one of its goroutines run that
goroutine's deferred calls and end only it, not the program; `http.serve_conn` is isolate, so a
handler that panics closes its own connection and the server goes on. The pool is the maximum
number running at once: `start` waits for a
free slot instead of growing, and `start f(x) else { ... }` runs the block instead of waiting. The compiler proves each task's stack size from clang's frame sizes
and the call graph; recursion is banned so that this is possible. `stack N` asserts an upper bound,
and a program can resize a library's pool with `pool http.serve_conn 512`.

### Using every core

```squark
program {
    threads auto   // this program is written for threads: one scheduler per CPU it may use
}
```

Each scheduler thread runs its own copy of `main`, with its own package variables and goroutines,
so nothing needs locks. A package variable says which it is: `thread db sqlite.DB` is one per
thread, `shared table = make_table()` is one copy, read-only after start-up, and `shared jobs Job
chan[64]` is a channel between threads. `SQUARK_THREADS=N` overrides the count at run time, like
`GOMAXPROCS`. Without the directive a program runs on one thread.

### Memory budgets

```squark
program {
    budget 128M
}
```

```
$ squark build examples/notes
memory: worst case 97.1 MiB of 128.0 MiB budget
  code                          112.0 KiB
  static data                   576.0 KiB   package variables, arenas, channel buffers, goroutine tables
  task http.serve_conn           96.0 MiB   = 512 × 192.0 KiB stack (proven 187952 B)
  main goroutine                144.0 KiB   = 1 × 144.0 KiB stack (proven 132432 B)
  task http.wake_idle            16.0 KiB   = 1 × 16.0 KiB stack (proven 9328 B)
  blocking-call helpers         320.0 KiB   = 2 × 160.0 KiB stack: threads that run blocking and detached C calls
  C libraries                 not counted   sqlite3 manages its own memory
```

`connections N` and `sockbuf N` add the kernel's socket buffers to it, and `heap sqlite3 N` SQLite's
heap (enforced, 16M by default), so the worst case covers what the kernel charges a container.
If the worst case is over budget, the build fails and prints this breakdown. The binary knows its
worst case too: on Linux it refuses to start in a cgroup (a container, a pod) whose memory limit
is lower, rather than being killed later at its busiest. `squark build -manifest k8s` prints the
matching pod `resources` and `terminationGracePeriodSeconds` (`-manifest systemd` for a unit).

### Errors, `!`, extern C, size literals

```squark
fault NotFound 404 "not found"                   // errors are values: code + static message

fn load(path str, buf u8[]) -> int ! {
    f := os.open(path)!                          // returns early with the error
    defer f.close()
    return f.read(buf)
}

extern fn read(fd int, b u8[]) -> int stack 8K c"sq_read"   // C binding with a stack budget

thread table u8[64K]                             // 4K, 16M, 1G size literals
```

### SIMD

```squark
// sum adds xs with two 4-lane accumulators: SIMD instructions on arm64 and x86-64
fn sum(xs f64[]) -> f64 {
    var a, b f64x4
    i := 0
    for ; i+8 <= len(xs); i += 8 {
        a += f64x4(xs[i:])     // load 4 lanes
        b += f64x4(xs[i+4:])
    }
    s := (a + b).sum()
    for ; i < len(xs); i++ {
        s += xs[i]
    }
    return s
}
```

`f32x4`, `u8x16`, `f64x4` and the other SIMD types take operators lane by lane; comparisons give
masks (`(v == c).any()`, `m.select(a, b)`), and `t.lookup(idx)` reads a 16-byte table at 16
indexes at once (hex digits of 16 bytes in two lookups). Plain loops over slices are vectorized by
clang already (see Loops below); SIMD types are for what it can't do, such as float sums, which it
keeps in source order, and searches that stop early. Both run 6 to 7 times faster than scalar code
in `bench/squark/kernels`. On x86-64, `squark build -cpu x86-64-v3` (or `native`) uses AVX2, as
`GOAMD64=v3` does for Go; the default is SSE2, which every x86-64 has.

### What's deliberately missing

There's no `new` or `make` without an arena, no string `+` without an arena, and no recursion. There
are no closures or method values, which would need captured state somewhere. There are no
interfaces or generic types; for code over several types, a parameter can be `anytype` (Zig-style:
each call makes a copy of the function for its argument types, as `sort.slice` does).

## How the checking works

**Lifetimes.** This is region inference in the style of Rust's borrow checker, without ownership.
Every value holding references gets one region variable per pointer level.

- Copies are covariant at the top level and invariant below it.
- Creating storage requires its contents to outlive it.
- A function's parameters and results are regions owned by the caller.

A program is rejected when the constraints force a short-lived region to outlive a longer one.
Because recursion is banned, functions are checked callees-first, and each gets a summary of how its
parameters' and results' regions relate. Call sites instantiate that summary, so no annotations are
needed.

**Stacks.** `squark build` compiles the generated C with `clang -fstack-usage`, walks the call graph,
and sizes every task's stacks. That includes budgets for C calls (`stack N` on externs) and a
margin. `SQ_STACKCHECK=1` paints stacks at run time and reports the deepest use observed, to check
the proofs; under load the notes server used 97 KB of its proven 186 KB. Every stack has a guard
page, and the build fails if any squark-compiled object references `malloc` or `free`.

**Loops.** clang vectorizes loops, but a bounds check that can panic mid-loop stops it. For an
innermost loop that counts by one (up or down), the C generator emits a second copy of the loop
without the checks on indexes like `x[i+k]` and `x[2*i+1]`. It runs that copy only when a test before the loop shows
every index the counter can reach is in range, so a loop that would panic still panics at the
same iteration. It also reads slice headers into locals: a 24-byte slice parameter is passed in
memory, and clang can't otherwise tell that a store through its pointer leaves its length alone.
`TestVectorize` checks clang's remarks for the loops in `testdata/vectorize`.

**Stopping drops no request.** An HTTP server catches SIGTERM and SIGINT: it stops accepting,
finishes the requests in progress and returns from `serve`, within a grace period
(`SQUARK_SHUTDOWN`, 10s by default). That's what a container stop expects. Other programs
still end at once unless they call `runtime.stop_gracefully()`.

**Idle connections cost nothing.** A keep-alive connection that is quiet for 100 ms moves to the
runtime's idle set: it stays registered with kqueue, its task exits, and the stack pages go back to
the OS.

## Benchmarks: squark vs Go

`bench/run.sh` builds both versions of everything and runs `bench/harness`. Results are in
`bench/results/latest.md`, along with the caveats below.

- The squark server here runs one thread; Go's uses every core. The load generator runs on the
  same machine. On Linux (Docker, 6 cores shared with the load generator), `threads 3` serves
  about 800k req/s on `/health`, 550k on `/notes/1` and 250k on the write-heavy mix, against
  450k, 290k and 170k for one thread and about 450k, 250k and 95k for Go with `GOMAXPROCS=3`;
  `workers 3` (processes) is similar. See `bench/workers-linux.sh`.
- `ONLY=kernels bench/run.sh` runs only the loop kernels (`bench/squark/kernels`: saxpy, byte
  xor, row-wise add and others). Use it to watch vectorization. Results go to
  `bench/results/kernels.md`. Go doesn't vectorize, so its times are the scalar baseline. A
  kernel named `…/simd` is written with SIMD types; Go runs its scalar version.
- `bench/compare.sh [REF]` builds the same kernels with another revision's compiler (default
  `main`) and with the working tree's, and prints both (`bench/results/compare.md`).
- The JSON benchmark compares hand-written append code with Go's reflection-based `encoding/json`.
- wordfreq streams the file and interns words in an arena, while Go's version reads the whole file,
  as idiomatic Go does.

## Layout

```
cmd/squark          build, run, check, doc, emit-c
internal/compiler   lexer, parser, type checker, region inference, call graph, C generator, doc
internal/driver     clang, stack proofs, pools, memory budget, no-heap check, tests
runtime/            scheduler, context switch (arm64/x86-64), netpoll, channels, maps, arenas
std/                standard library (squark + C shims), embedded in the compiler
testdata/           compile-error tests (// ERROR "...") and run tests (.sq + .out)
bench/              Go baselines, load generator, process sampler, harness
scripts/            test-linux.sh: the test suite on Linux in Docker
editors/vscode/     VS Code syntax highlighting (TextMate grammar, tested with VS Code's tokenizer)
docs/design.md      the language design and its rationale
```

`go test ./internal/...` runs the test suite.

## Not done yet

- Interfaces and generic types: left out so far; `anytype` parameters cover the cases found in the
  standard library and examples.
- Provenance contracts on function types. Indirect calls use whole-program inference instead.
- One SQLite connection per thread: a slow query no longer stalls its thread, but the thread's
  other goroutines that need the database wait for it (a small pool per thread would let them in).

## License

MIT; see [LICENSE](LICENSE).
