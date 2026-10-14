# squark 2: language design

Status: **implemented** (2026-10-14). Where the implementation differs from or adds to the original
proposal, it is noted in the "As built" section at the end.

squark 1 is "Go minus the things that need a heap". squark 2 makes memory something you can see and
write down: where it lives (static, a task's stack, an arena), how big it is (capacity in types),
how long references are valid (provenance in signatures), and how much the program can ever use
(budgets). Go's syntax stays where it serves this; it changes where it gets in the way.

The guiding rule: **every byte a program can use is accounted for at compile time.**

## 1. Arenas: scoped dynamic allocation

squark 1 bans run-time sizes, `new`, string `+` and growing slices, because they need memory from
somewhere. squark 2 allows them when you name where the memory comes from.

```go
arena scratch 64K {
    words := make([]string, n) in scratch    // run-time size
    root := new(Node) in scratch             // zeroed *Node
    path := dir + "/" + name in scratch      // string concatenation
    words = append(words, w) in scratch      // may grow: reallocates inside scratch
}                                            // everything in scratch is gone here
```

- `arena NAME SIZE { ... }` reserves SIZE bytes in the current frame. The stack proof counts them
  and the budget includes them. Allocation is a pointer bump.
- `EXPR in A` means "allocations made by this expression come from A". Without `in`, an expression
  that needs memory is still a compile error, with a hint to add one.
- References to arena memory have the arena's region. They cannot outlive the `arena` block, which
  the lifetime checker already knows how to enforce.
- Running out of arena space panics with the arena's name and size. `A.Used()` and `A.Left()`
  report usage.
- Arenas can be parameters (`func parse(a arena, src string) *Tree from a`). Allocations then have
  the region of the caller's arena.
- Package-level arenas (`arena interned 8M`) are static and bump-only. They are never reset, so
  their references are static. This is how wordfreq's word store would be written.
- `http` gives every request an arena (`r.Arena`, size set on the server) that is reset after the
  response. Handlers get scoped dynamic allocation with no GC and no leaks.

## 2. Capacity in types: storage vs views

Go already has the pattern of `[N]T` (storage) and `[]T` (a view of it). squark 2 applies it to
everything that holds a variable number of elements:

| storage (value, inline) | view (reference, region-tracked) |
|---|---|
| `[N]T` fixed array | `[]T` |
| `[..N]T` bounded vector: dynamic length, capacity N | `[]T` |
| `map[K]V cap N` | `map[K]V` |
| `chan T cap N` | `chan T` |

```go
var line [..256]byte                 // len 0, cap 256, storage inline
line.Push('x')                       // panics past 256; line.TryPush(c) returns bool
var seen map[string]int cap 4096     // no make(): declaring it is the storage
var jobs chan Job cap 16             // package-level channel with a 16-element buffer

func count(m map[string]int, w string) { m[w]++ }   // takes a view of any capacity
count(seen, "x")                                    // storage converts to a view implicitly
```

- Storage values used where a view is expected convert implicitly (like `&` on method receivers);
  the view's region is the storage's.
- `make` disappears outside arenas: declaring storage is allocating it.
- Maps and channels are no longer nil-able reference types that need `make`. A view can still be
  nil.

## 3. Provenance in signatures

The checker already infers, for every function, where results point and which arguments get stored
where. squark 2 lets you write that down, and the compiler checks it against the body:

```go
func prefix(b []byte) []byte from b              // result points into b
func link(a *Node, b *Node into a)               // b may be stored inside a
func newError(msg static string) error           // msg must be static
func intern(s string) string static              // result is static
```

Inference still works when you leave it out. Writing it is required in exactly the places inference
can't see a body: function types, extern C functions, and (later) interface methods. `squark doc`
shows the inferred provenance of every exported function.

## 4. Memory budgets

```go
budget 64M
```

A top-level declaration in package main. The build fails if the worst case can exceed it. The worst
case is static data, plus every task pool's proven stack times its size, plus package arenas. The
build always prints the breakdown:

```
memory budget 64.0 MiB, worst case 41.3 MiB
  static data            1.2 MiB   (sqlite statements, interned words, ...)
  task http.serveConn   38.4 MiB   = 256 × 153.6 KiB (stack 120.0 KiB incl. request arena 32 KiB)
  task main              1.5 MiB
  ...
```

Budgets count squark's memory. C libraries (SQLite) are reported as a separate, unchecked line using
their declared limits where possible (for example `sqlite3_hard_heap_limit64`).

## 5. Tasks instead of pragmas

Goroutine entry points become a declaration, because they carry the information the compiler
needs: a pool size and a proven stack.

```go
task serveConn(c net.Conn, h Handler) pool 10000 {
    ...
}

go serveConn(c, h)          // only tasks can be started with go
```

- `pool N` is the maximum number of concurrently running instances. `go` waits when the pool is full.
- A task's stack size is proven and shown by `squark build -v`. `stack N` asserts an upper bound,
  which is a compile error if exceeded and useful for budgeting.
- Arguments to `go` must be static or be values without references, as today.

## 6. Extern C and build settings become syntax

```go
extern func sqrt(x float64) float64 stack 1K
extern "sq_read" func read(fd int, b []byte) int stack 2K
extern "sqlite3_step" func step(st rawptr) int stack 128K

link "sqlite3"
cflags "-I/opt/homebrew/opt/sqlite/include"
```

`stack` is required on externs: the stack proof depends on it, so it should be visible.

## 7. Errors: constants and `try`

`error` stays a value (code plus static message), which makes errors constants:

```go
const ErrNotFound = error(404, "not found")
const ErrTooBig = error(413, "body too large")
```

`try` propagates an error result, returning zero values for the other results:

```go
func load(path string, buf []byte) (int, error) {
    f := try os.Open(path)
    defer f.Close()
    n := try f.Read(buf)
    return n, nil
}
```

`if err != nil` still works; `try` is for the common case.

## 8. Small things

- **Size literals:** `4K`, `64K`, `16M`, `1G` are integer constants (×1024ⁿ). They are everywhere in a
  memory-explicit language.
- **`print`/`println`** stay as builtins (they need no memory).

## What stays exactly like Go

Packages and imports, exported-means-capitalised, structs, methods and pointer receivers, `if`,
`for`, `switch`, `range`, `defer`, multiple returns, constants and `iota`, operators, slices of
arrays, goroutine-style blocking I/O, channels, and gofmt-style layout.

## Not in this round

Interfaces (fat pointers with provenance, after §3), generics (monomorphised), `select`, string
interpolation into arenas, and multiple OS threads.

## Build order

1. Syntax foundation: size literals, `task`, `extern`/`link`/`cflags`, error constants, `try`.
   Port stdlib, examples and tests (mechanical).
2. **Arenas**, including the per-request arena in `http`.
3. Budgets and the memory report.
4. Capacity types (`[..N]T`, `map … cap N`, `chan … cap N`) and storage→view conversion.
5. Provenance signatures, `squark doc`.

Each step keeps the test suite and benchmarks green. Step 2 adds an arena-based variant of the notes
server to compare.

## As built

- **Region inference was rebuilt.** The first checker tracked one lifetime per value and could not
  express "a temporary slice of pointers into an arena". It is now region inference with a region
  per pointer level: covariant at the top, invariant below, and "contents outlive their storage"
  when storage is created.
- **`from` / `into` name relationships, not levels.** On functions with bodies, the declaration says
  which parameters and results are related, and the compiler infers the exact pointer levels from
  the body (`from b` on `func (b *Buffer) Bytes() []byte` means "into what b points to"). Callers
  use the precise inferred summary. Extern functions use the declaration level-aligned. Contracts
  on function types are not implemented yet.
- **Views of vectors have cap == len.** A `[]T` view of a `[..N]T` cannot append into the vector's
  spare capacity behind its back.
- **Map and channel storage cannot be copied** (they point into themselves). Vectors can be copied.
- **`pool pkg.task N`** in package main resizes a library task's pool, so a program can fit its
  budget without editing the library.
- **`stack N` on a task** is checked against the proof.
- **Idle keep-alive connections** have no task at all: after 100 ms of quiet the connection moves to
  the runtime's idle set and its stack is returned to the OS.
- **Arena exhaustion panics.** A recoverable form (`try ... in a`) is still open.
