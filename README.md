# squark

> **Experimental.** squark is an experimental language based on Go, exploring programming without a
> heap. Expect breaking changes; it isn't meant for production use.

A Go-shaped language where memory is something you can see: there is no heap, every byte a program
can use is accounted for at compile time, and the compiler proves that no reference outlives the
memory it points to.

```go
package main

import "http"

budget 128M             // the build fails if the worst case can exceed this
pool http.serveConn 512 // at most 512 connections served at once

func handle(w *http.ResponseWriter, r *http.Request) {
    greeting := "hello, " + r.QueryValue("name") + "\n" in r.Arena // per-request arena
    w.WriteString(greeting)
}

func main() {
    if err := http.ListenAndServe(":8080", handle); err != nil {
        panic(err)
    }
}
```

(This is `examples/greet`.)

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

## Where memory lives

There are four kinds of memory, and every value lives in one of them:

| | lifetime | example |
|---|---|---|
| static | the whole program | `var table [1M]byte`, `var jobs chan Job cap 16`, `arena words 8M` |
| a task's stack | a function call | `var buf [4K]byte`, `var seen map[string]int cap 64` |
| an arena | an `arena` block | `n := new(Node) in scratch` |
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

Everything Go has and that doesn't need a heap works as in Go: packages, structs, methods, pointers,
slices of arrays, multiple returns, `defer`, `switch`, labeled `break`/`continue`, `range` over ints,
strings, slices, maps and channels, constants and `iota`, variadic functions, goroutines and channels.
The rest of this section is what squark adds or changes.

### Arenas: scoped dynamic allocation

```go
arena scratch 64K {
    words := make([]string, n) in scratch       // run-time sizes are fine in an arena
    root := new(Node) in scratch                // zeroed *Node
    path := dir + "/" + name in scratch         // string concatenation
    words = append(words, w) in scratch         // grows inside the arena instead of panicking
    owned := string(buf[:n]) in scratch         // Go's copying conversions copy into the arena
}                                               // all of it is gone here
```

- `EXPR in A` means allocations made by that expression come from arena `A`. Without an arena, the
  same expression is a compile error with a hint.
- A local arena is part of its stack frame, so the stack proof counts it. Pages are only touched as
  they're used, and allocation is a pointer bump.
- Package-level arenas (`arena words 8M`) are static and bump-only: a place for data that lives
  forever, such as interned strings.
- Arenas can be passed in: `func build(a arena, depth int) *Node` allocates in the caller's arena.
- A full arena panics with its name and size. `a.Used()`, `a.Left()` and `a.Cap()` report usage.
- Every HTTP request gets `r.Arena` (16 KiB), which is gone once the response is written.

### Capacity in types: storage and views

Go's `[N]T` (storage) and `[]T` (a view of it) pattern, applied to everything:

| storage (inline, a value) | view (a reference) |
|---|---|
| `[N]T` | `[]T` |
| `[..N]T`: length varies, capacity N | `[]T` |
| `map[K]V cap N` | `map[K]V` |
| `chan T cap N` | `chan T` |

```go
var line [..256]byte
line.Push('x')                 // panics when full; line.TryPush(c) returns false instead
last := line.Pop()

var seen map[string]int cap 4096    // declaring the storage is the allocation
count(seen, "x")                    // storage converts to a view (map[string]int) implicitly

var jobs chan Job cap 16            // package-level: shared by all goroutines (per thread with threads N)
```

`make` exists only inside arenas. Map and channel storage can't be copied, because they point into
themselves; pass views instead.

### Provenance in signatures

The compiler infers, for every function, where its results point and which arguments it stores
where. You can write that down, and the compiler checks the body against it:

```go
func Cut(s, sep string) (string, string, bool) from s          // results point into s
func link(a *Node, b *Node into a)                             // b may be stored inside a
func New(msg static string) error                              // msg must be static memory
func Name() string static                                      // the result is static
```

Declarations name which parameters are related, and the body determines the exact pointer levels.
Leaving provenance out is fine: `squark doc` shows what was inferred. Declaring it turns that into
a checked interface, so a later change to the body can't quietly add a new way for references to
escape.

### Tasks: goroutines with proven stacks

```go
task serveConn(c net.Conn, h Handler) pool 10000 stack 256K {
    ...
}

go serveConn(c, h)
```

Only tasks can be started with `go`. The pool is the maximum number running at once: `go` waits for a
free slot instead of growing. The compiler proves each task's stack size from clang's frame sizes
and the call graph; recursion is banned so that this is possible. `stack N` asserts an upper bound,
and a program can resize a library's pool with `pool http.serveConn 512`.

### Using every core

```go
threads auto   // this program is written for threads: one scheduler per core
```

Each scheduler thread runs its own copy of `main`, with its own package variables and goroutines,
so nothing needs locks. Threads share only constants, `shared var` data (read-only after start-up)
and shared channels (`shared var jobs chan Job cap 64`). `SQUARK_THREADS=N` overrides the count at
run time, like `GOMAXPROCS`. Without the directive a program runs on one thread.

Since every package variable is per thread, the compiler lists the ones a threaded program
changes, so a counter that quietly counts a quarter of the requests doesn't go unnoticed. Declare
them `thread var` once that's what you mean:

```go
thread var db sqlite.DB   // each thread has its own connection
```

### Memory budgets

```go
budget 128M
```

```
$ squark build examples/notes
memory: worst case 96.7 MiB of 128.0 MiB budget
  code                           80.0 KiB
  static data                   464.0 KiB   package variables, arenas, channel buffers, goroutine tables
  task http.serveConn            96.0 MiB   = 512 × 192.0 KiB stack (proven 185664 B)
  main goroutine                144.0 KiB   = 1 × 144.0 KiB stack (proven 132288 B)
  task http.wakeIdle             16.0 KiB   = 1 × 16.0 KiB stack (proven 9280 B)
  C libraries                 not counted   sqlite3 manages its own memory
```

If the worst case is over budget, the build fails and prints this breakdown.

### Errors, `try`, extern C, size literals

```go
const ErrNotFound = error(404, "not found")     // errors are values: code + static message

func load(path string, buf []byte) (int, error) {
    f := try os.Open(path)                       // returns early with the error
    defer f.Close()
    return f.Read(buf)
}

extern "sq_read" func read(fd int, b []byte) int stack 8K   // C binding with a stack budget
link "sqlite3"

var table [64K]byte                               // 4K, 16M, 1G size literals
```

### What's deliberately missing

There's no `new` or `make` without an arena, no string `+` without an arena, and no recursion. There
are no closures or method values, which would need captured state somewhere. There are no
interfaces or generics yet; both can be done without a heap and are next.

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

**Idle connections cost nothing.** A keep-alive connection that is quiet for 100 ms moves to the
runtime's idle set: it stays registered with kqueue, its task exits, and the stack pages go back to
the OS.

## Benchmarks: squark vs Go

`bench/run.sh` builds both versions of everything and runs `bench/harness`. Results are in
`bench/results/latest.md`, along with the caveats below.

- The squark server is single-threaded; Go's uses every core. The load generator runs on the same
  machine. With `workers 3` on Linux (Docker, 6 cores shared with the load generator) notes serves
  about 800k req/s on `/health` and 630k on `/notes/1`, against 450k and 295k for one process and
  about 470k and 265k for Go with `GOMAXPROCS=3`; `threads 3` (one process) is close behind
  `workers 3`. See `bench/workers-linux.sh`.
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
docs/design.md      the language design and its rationale
```

`go test ./internal/...` runs the test suite.

## Not done yet

- Interfaces and generics.
- Provenance contracts on function types. Indirect calls use whole-program inference instead.
- `select`.
- Blocking C calls stall their scheduler thread (SQLite queries, mainly); moving them to helper
  threads is next (see [MULTICORE.md](MULTICORE.md)).
- Arena exhaustion panics. A `try`-able allocation form would make it recoverable.

## License

MIT; see [LICENSE](LICENSE).
