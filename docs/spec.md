# squark language reference

This is the precise reference for writing squark. It assumes you know Go: squark is Go-shaped, and
this document lists every place where it differs. Anything not mentioned here behaves as in Go. The
standard library API is in [stdlib.md](stdlib.md), which is generated from the compiler and always
current.

## 0. The ten rules that matter most

1. **There is no heap.** Memory is static (package level), on a task's stack, or in an `arena`.
   `new`, run-time-sized `make`, string `+` and copying conversions are only allowed in an arena:
   `x := new(T) in a`.
2. **References may not outlive their memory.** The compiler checks this across function calls.
   Returning `&local`, or `buf[:]` of a local array, is an error: take the storage as a parameter
   instead.
3. **Callers provide buffers.** APIs take a `[]byte`/`[]T` to fill and return a view of it:
   `strconv.AppendInt(dst, n)`, `d.String(buf)`, `stmt.ColumnText(i, buf)`.
4. **Capacities are fixed.** `append` past capacity panics (unless `... in arena`), maps have a
   declared capacity (`map[K]V cap N`), and channels have one (`chan T cap N`).
5. **No recursion**, direct or indirect, because every stack size is proven at compile time. Use a
   loop with an explicit stack such as a `[..N]T` vector.
6. **No closures, method values, interfaces or generics.** Use named functions, func-typed values of
   named functions, `switch`, and concrete types.
7. **`go` only starts a `task`:** `task f(args) pool N { ... }`. Tasks have no results. Arguments to
   `go` and values sent on channels must not reference stack or arena memory.
8. **Errors are values with a static message:** `const ErrX = error(404, "not found")`. They can't
   be formatted with dynamic text; use the code, or log separately.
9. **`string(b)` is a zero-copy view of `b`.** `[]byte(s)` is an error without an arena.
10. **Cooperative within a thread; threads share nothing mutable.** A thread's goroutines switch
    only at blocking operations (I/O, channel operations, `time.Sleep`, `go` on a full pool,
    `runtime.Gosched()`) and in the loops of tasks marked `preemptible`. So data shared between a
    thread's goroutines needs no locks as long as you don't block mid-update. With `threads N` (or
    `workers N`) each thread runs its own copy of `main` with its own package variables; threads
    share only read-only data and shared channels. No lock is ever needed. A busy loop in an
    ordinary task holds up its thread: mark CPU-heavy tasks `preemptible`, and the runtime warns
    about a goroutine that holds its thread for 100ms while others wait.

## 1. Lexical elements

- **Keywords:** Go's keywords except `fallthrough`, `goto` and `interface`, which are not
  supported. Added keywords: `arena`, `extern`, `in`, `task`, `try`. `shared` and `thread` mark
  a package-level `var` (§4) but stay ordinary identifiers elsewhere; so does `select`, which
  starts a select statement only when followed by `{`.
- **Contextual words** (identifiers elsewhere):
  - `pool` and `stack` after a signature.
  - `from` and `static` after results.
  - `shared` before a package-level `var`.
  - `static` before a parameter type, and `into` after one.
  - `cap` after a `map`/`chan` type.
  - `budget`, `pool`, `workers`, `threads`, `link`, `cflags` and `ldflags` as top-level directives.
- **Size literals:** a decimal integer immediately followed by `K`, `M` or `G` is multiplied by 2^10,
  2^20 or 2^30: `64K == 65536`.
- **`..`** appears only in vector types: `[..N]T`.
- **Pragmas:** `//squark:noinline` before a function stops clang from inlining it. No other pragmas
  exist.
- Comments, string/rune/number literals and semicolon insertion are as in Go.

## 2. Program structure

```go
package main

import (
    "http"          // standard library packages are imported by name
    "./mylib"       // packages in your project by relative path
)

// top-level directives come after the imports
budget 128M                 // package main only: fail the build if worst-case memory exceeds this
pool http.serveConn 512     // package main only: resize a task's pool (pkg.task or task)
workers 4                   // package main only: run 4 processes (or auto: one per core); see §9
threads auto                // package main only: a scheduler thread per core (or N); see §9
link "sqlite3"              // link a C library (any package)
cflags "-I/opt/include"     // C compiler flags for this package's .c files
ldflags "-L/opt/lib"        // linker flags

const ...
var ...
type ...
func ...
task ...
extern ...
arena ...
```

- A package is a directory of `.sq` files. `.c` files in the same directory are compiled and linked
  as C shims.
- `func main()` in package `main` is the entry point; when it returns, the program exits.
- There are no `init` functions. Package-level `var` initializers run in declaration order, packages
  in import order.
- Exported means capitalized, as in Go.

## 3. Types

| type | notes |
|---|---|
| `bool`, `string`, `int` (64-bit), `int8`…`int64`, `uint` (64-bit), `uint8`…`uint64`, `uintptr`, `float32`, `float64` | as Go; `byte` = `uint8`, `rune` = `int32` |
| `rawptr` | untyped C pointer (`void*`) for extern calls; converts to and from pointers and `uintptr`, and is not lifetime-checked |
| `error` | a value: integer code + static message. Methods `Error() string`, `Code() int`. Zero value is `nil` |
| `arena` | a reference to arena storage. Methods `Used()`, `Left()`, `Cap()` (all `int`) |
| `[N]T` | array (storage) |
| `[]T` | slice (view) |
| `[..N]T` | vector: inline storage for N elements plus a length (storage) |
| `map[K]V cap N` | map storage with fixed capacity N. K is an integer, string, bool or pointer type |
| `map[K]V` | map view |
| `chan T cap N` | channel storage with an N-element buffer (`cap 0` = unbuffered) |
| `chan T` | channel view (no direction types) |
| `*T`, `struct{...}`, `func(...) ...` | as Go. A func value can only be a named top-level function |

Not supported: interfaces, generics, complex numbers, type aliases, embedded fields, local type
declarations, directional channel types.

### Storage and views

A storage value (`[N]T`, `[..N]T`, `map[K]V cap N`, `chan T cap N`) contains its elements inline.
Declaring it is the allocation, and its zero value is ready to use:

```go
var seen map[string]int cap 1024
seen["x"]++                 // no make needed
var jobs chan Job cap 16    // package level: shared by all goroutines (on its thread, with threads N)
var line [..256]byte
```

Where a view type is expected (assignment, argument, return), addressable storage converts to a view
implicitly. Indexing, `range`, `len`, `delete`, `close`, sends and receives work on storage directly:

```go
func count(m map[string]int, w string) { m[w]++ }
count(seen, "x")            // seen converts to map[string]int
for k, v := range seen {}
```

- The view points into the storage, so the storage must outlive it (rule 2).
- Map and channel storage **cannot be copied** (`m2 := m` is an error), because they point into
  themselves. Pass a view. The same holds for a struct, array or vector that contains map or
  channel storage: copying one out of a variable is an error (use a pointer), while a fresh value
  (`T{}`, a call result) is fine because its storage is still unused.
- Arrays and vectors copy by value, like Go arrays.

### Vectors `[..N]T`

```go
var v [..8]int
v.Push(1)            // panics if len(v) == 8
ok := v.TryPush(2)   // false if full
x := v.Pop()         // panics if empty
v.Clear()            // len(v) = 0
len(v); cap(v)       // cap is the constant 8
v[i]                 // bounds-checked against len(v)
v[:]                 // a []int view with len == cap == len(v)
for i, x := range v {}
w := [..4]Point{{1, 2}, {3, 4}}   // literal: len 2, cap 4
```

`Push`, `TryPush`, `Pop` and `Clear` need an addressable vector.

## 4. Declarations

### Functions, tasks, externs

```go
func name(params) results { ... }

task name(params) pool N { ... }               // a goroutine entry point; no results
task name(params) pool N stack 64K { ... }     // also assert a stack upper bound
task name(params) pool N preemptible { ... }   // its loops may yield after a time slice

extern func sqrt(x float64) float64 stack 1K               // C function of the same name
extern "sq_read" func read(fd int, b []byte) int stack 8K  // C symbol differs from the name
extern "slow_query" func query(q string) int stack 128K blocking // runs on a helper thread
```

- `pool N`: at most N instances run at once. `go` on a full pool waits for a slot.
- `preemptible` on a task: once one of its goroutines has run for a time slice (about 10ms)
  without blocking, it yields at the next loop iteration, in its own code or any function it
  calls, so a long computation doesn't hold up its thread. Those functions get a check at each
  loop iteration (a load and a branch, too cheap to measure); called from other goroutines they
  never yield. Only a preemptible task can be interrupted mid-loop, so keep it from leaving state
  that other goroutines read half-updated across a loop iteration (or update it in one step).
- `stack N` on an extern is required: it's the C function's stack budget used in the stack proof.
  On a task it is an assertion, and the build fails if the proven size is larger.
- Extern parameter types map to C as follows: `int`/`int64` → `int64_t`, `int32` → `int32_t`, and so
  on; `string` → `struct { const uint8_t *p; int64_t len; }`; `[]byte` → `struct { uint8_t *p;
  int64_t len, cap; }`; `*T`/`rawptr` → pointers.
- Extern results are trusted to point to static memory unless provenance is declared.
- `blocking` on an extern runs each call on a helper thread while the calling goroutine waits, so
  a C call that blocks (a slow query, a lock wait, a read from a slow disk) doesn't stop the other
  goroutines of its scheduler thread. Each call costs two thread handoffs (microseconds), so mark
  only calls that always wait. Arguments may point into the goroutine's stack (it stays put while
  the goroutine waits). The C function's `stack N` budget applies to the helper threads (two per
  scheduler thread, counted in the memory report), not to goroutine stacks.
- C code that is usually fast but sometimes slow can move itself instead: `sq_detach()` (in
  `sq_rt.h`), called in the middle of a C call, moves the running goroutine (its stack, mid-call)
  to a helper thread and lets its scheduler thread go on; the C code carries on there.
  `sq_reattach()`, called before the C function returns, moves it back. Fast calls cost nothing
  extra. Between the two, the C code runs on another OS thread, so it must not call into the
  squark runtime or rely on thread-local state or thread-owned locks taken before `sq_detach`.
  `sqlite` does this from SQLite's progress handler (a statement running over 100µs) and busy
  handler (a lock wait). The stack stays the goroutine's, so `stack N` still counts there.
- While a goroutine waits or runs elsewhere, others on its thread run, so C state they share
  needs a lock: the runtime's `sq_gmutex` (in `sq_rt.h`) parks waiting goroutines instead of
  blocking the thread.

### Provenance (optional)

```go
func Cut(s, sep string) (string, string, bool) from s   // results may point into what s refers to
func link(a *Node, b *Node into a)                      // b may be stored inside a
func remember(msg static string)                        // msg must point to static memory
func Name() string static                               // the result points to static memory
```

- The compiler infers all of this anyway (see `squark doc`). Declaring it makes it a checked
  contract: the build fails if the body does something the declaration doesn't allow, and the error
  says which clause to add.
- On functions with a body, the declaration names which parameters and results are related, and the
  compiler works out the pointer levels.
- On externs, the declaration is the only information the compiler has.

### Arenas

```go
arena interned 8M            // package level: static, bump-only, never freed

func f() {
    arena scratch 64K {      // statement: storage in this stack frame
        p := new(Node) in scratch
    }                        // everything allocated in scratch is gone here
}

func build(a arena, n int) *Node { return new(Node) in a }   // allocate in the caller's arena
```

- Sizes are constants.
- A full arena panics: `panic: arena scratch is full (capacity 65536, used …, need …)`.
- Every HTTP request carries one: `r.Arena`.

### Constants, variables, types

As in Go, plus two kinds of constant data:

- Error constants: `const ErrNotFound = error(404, "not found")`, compared with `==`.
- Constant composites: arrays and structs whose elements are constants (numbers, strings, bools,
  errors, or arrays and structs of them):

  ```go
  const primes = [5]int{2, 3, 5, 7, 11}
  const defaults = Config{Port: 8080, Name: "notes"}
  ```

Both are read-only data in the binary, shared by every scheduler thread (`threads N`). Reading,
indexing, ranging and copying them work as for variables; assigning to them or their elements,
taking their address, slicing them and calling pointer methods on them are errors. Copy into a
variable first (`t := primes`) to get something writable.

For data computed when the program starts, `shared var` declares one copy for all scheduler
threads:

```go
shared var crcTable = makeTable()   // may call functions, read files, query SQLite
```

Its initializer runs once, on the first scheduler thread and in order with the other package
initializers, before any other thread starts. Afterwards it is read-only, with the same rules as
constants. Its type is limited to numbers, strings, bools, errors and arrays or structs of them,
because nothing a shared slice, map or pointer refers to could be kept read-only.

A plain `var` is one copy per scheduler thread: under `threads N` a counter counts one thread's
work and a cache holds one thread's entries. `squark build`, `run` and `check` list the package
variables a threaded program changes after start-up, since their values can differ between
threads:

```
note: with threads, each scheduler thread has its own copy of these package variables:
  main.sq:12: hits: changed in handle
```

`thread var` declares one copy per thread on purpose (`thread var db sqlite.DB`, each thread's own
connection); it behaves exactly like `var` and is left out of the list. So the list shows only the
variables nobody has decided about: make each `thread var`, or move it to a shared channel.
Standard library packages aren't listed; their per-thread state is per thread by design.

A shared channel connects the scheduler threads: `shared var jobs chan Job cap 64` (declared
without a value) is one channel for all of them. Send, receive, `close`, `len` and `range` work
as for any channel; a goroutine blocked on it is woken by whichever thread completes the
operation. Its elements may not hold references (no strings, slices, maps or pointers), since a
reference could view one thread's own memory; send numbers, bools, errors, or arrays and structs
of them (an index or ID instead of a string).

## 5. Expressions

| expression | squark |
|---|---|
| `x in a` | allocations made by `x` come from arena `a`. Lowest precedence: `a + b + c in ar` is one allocation |
| `new(T) in a` | zeroed `*T` in the arena. `new(T)` without an arena is an error |
| `make([]T, n[, c]) in a` | run-time sizes are OK. `make(map[K]V, n) in a` also works. `make` without an arena is an error, as is `make(chan ...)` |
| `append(s, xs...)` | panics if the result exceeds `cap(s)`. `append(s, x) in a` grows into the arena instead |
| `s + t` (strings) | only if both are constants, or `in a` |
| `string(b)` | zero-copy view of `b`'s bytes. `string(b) in a` copies |
| `[]byte(s)` | only `[]byte(s) in a` (copies). Otherwise `copy(buf[:], s)` |
| `string(r)` (rune) | only for constants; use `utf8.AppendRune` |
| `try f(...)` | if `f`'s last result (an `error`) is non-nil, return it from the enclosing function (whose last result must be `error`), with zero values for the other results. Otherwise yields the remaining results |
| `error(code, msg)`, `error(msg)` | builds an error. `msg` must be static memory (a constant or package-level data) |
| `&T{...}` | address of a temporary that lives until the end of the enclosing block |
| `[]T{...}` | the backing array lives until the end of the enclosing block |
| map literals | not supported; declare storage and assign |
| func literals, method values, method expressions | not supported |
| `v.(T)`, type switches | not supported (no interfaces) |

Integer arithmetic wraps on overflow, division by zero panics, shifts by at least the width
yield 0 (or -1 for negative signed `>>`), and a negative shift count panics, as in Go.

**Order of evaluation.** Calls, method calls and channel receives run left to right as in Go, and
`a, b = ...` evaluates every index and pointer operand on the left before storing anything. The
differences, all about side effects within one statement:

- `a[i] = f()`: `f()` runs before the index `i` is evaluated (Go: `i` first).
- `getm()[key()]`, read or write: the key is evaluated before the map.
- `getch() <- v`: `v` is evaluated before the channel.
- Keyed composite literals (`T{b: f(), a: g()}`, `[3]int{2: f(), 0: g()}`) evaluate their elements
  in field or index order, not source order.

If the order matters, compute the operands into variables first.

**Floating-point `min` and `max`** don't implement Go's special cases: with a NaN operand the result
is not necessarily NaN, and `min(0.0, -0.0)` may return either zero.

### Builtins

`len`, `cap`, `append`, `copy` (also `copy(dst []byte, src string)`), `delete`, `close`, `min`,
`max`, `panic(x)` (x is a string, integer, bool or error), `print`/`println` (write to **stdout**;
accept numbers, strings, bools, pointers, errors and `[]byte`), `new`/`make` (arena only).

## 6. Statements

As in Go, except:

- No `goto` or `fallthrough`. Labels are allowed only on `for`, `switch` and `select`, for
  labeled `break`/`continue`.
- `select` works as in Go: it waits until one of its sends or receives can proceed (a random one
  if several can), `default` runs if none can, nil channels are never ready, and `break` leaves
  it. Cases may mix per-thread and shared channels. For a timeout, `case <-time.After(d):` runs
  if no other case is ready within d; since there is no heap to make a channel per call,
  `time.After` is allowed only there, without a received value (`time.Now()` gives the time).
- `defer` is not allowed inside loops. Its arguments must live until the function returns.
- `go f(args)`: `f` must be a task. Arguments must not reference stack or arena memory.
- Channel sends: the value must not reference stack or arena memory.
- `arena NAME SIZE { ... }` is a statement.
- `for i := range 10` works (Go 1.22 style). `range` over a string yields runes.
- `range` over an array value iterates the array itself, not a copy: changes made in the loop are
  seen by later iterations. (Copying would put a second array on the stack.) Range over a copy
  explicitly if you need Go's behaviour.
- Unused variables and unused imports are not errors.
- Deleting map entries inside a `range` over the map works as in Go. Inserting a *new* key inside
  the loop may make it skip or repeat entries (to reuse deleted slots, a map with many deletions
  compacts itself on insert); collect the keys first if you need both.

## 7. The memory rules (what the checker enforces)

Every reference (pointer, slice, string, map/chan/arena reference, or a struct/array containing one)
must not outlive the memory it points to:

- **Return:** a function may only return references to static memory or to memory reachable from
  its parameters.
- **Store:** storing a reference into a variable, field, element or map requires the referenced
  memory to outlive the storage. Package variables accept only static memory.
- **Calls:** the compiler knows each callee's flows ("argument 2 is stored inside argument 1", "the
  result points into argument 1") and checks them against your arguments.
- **Arenas:** memory from `arena x` lives until the end of that block. A package arena is static.
- **Goroutines and channels:** only static memory crosses goroutines.

### Error messages and fixes

| error contains | fix |
|---|---|
| `cannot return reference to variable buf` | take `buf []byte` as a parameter and return a view of it, or allocate in a caller-provided arena |
| `cannot return reference to arena x` | the arena dies with its block; take `a arena` as a parameter instead |
| `cannot escape into static memory (assigned to G)` | package variables can only point to static data: make the referenced data package-level too, or copy it into a package arena (`string(b) in pkgArena`) |
| `does not live long enough to be stored in variable v` | declare the referenced variable in an outer block (it must live at least as long as v) |
| `f stores argument 2 inside argument 1` | the second argument must live at least as long as the first |
| `make needs an arena` | declare storage (`var buf [N]T`, `var v [..N]T`, `var m map[K]V cap N`), or `make(...) in a` |
| `new(T) needs memory` | `var x T` and use `&x`, or `new(T) in a` |
| `string concatenation needs memory` | `a + b in r.Arena`, or append into a buffer |
| `converting s to []byte copies it` | `copy(buf[:], s)`, or `[]byte(s) in a` |
| `recursion is not allowed` | loop with an explicit stack (`var stack [..64]T`) |
| `go needs a task` | declare the function `task f(...) pool N` |
| `goroutine … can outlive this function` | pass only static data, or values without references (ints, structs of ints) |
| `cannot copy map[...] cap N storage` | pass a view: `func f(m map[K]V)` and call `f(store)` |
| `cannot copy x: T contains … storage` | pass or keep a pointer (`*T`); range by index and use `&xs[i]` |
| `append exceeds capacity` (runtime panic) | larger storage, or `append(...) in a` |
| `map capacity exceeded` (runtime panic) | a larger `cap N` |
| `memory budget exceeded` | lower pool sizes (`pool pkg.task N`), fewer `workers`, or raise the budget |
| `workers auto can't be checked against the budget` | `workers N` with the number of cores you deploy on |
| `threads auto can't be checked against the budget` | `threads N` with the number of cores you deploy on |
| `shared var X: T can't be shared` | share numbers, strings, bools, errors or arrays/structs of them; keep everything else per thread |
| `a shared channel's elements can't hold references` | send an index, ID or fixed-size array instead of a string or slice |
| `time.After can only be used as a select case` | `select { case <-time.After(d): ... }`; to just wait, `time.Sleep(d)` |
| `only tasks can be preemptible` | mark the task the work runs in; functions it calls yield too |
| `only extern functions can be blocking` | drop `blocking`; squark code waits by parking already (channels, I/O, sleeps) |
| `cannot take address of …` / `cannot slice …` / `cannot assign to …` (a constant) | constants (error constants, constant tables) are read-only and shared by every thread: copy into a variable first, `t := table; p := &t[0]` |
| `defer inside a loop` | move the loop body into a function and defer there |

## 8. Idioms: translating Go

```go
// fmt.Sprintf("id=%d name=%s", id, name)
var buf [64]byte
line := strconv.AppendInt(append(buf[:0], "id="...), id)
line = append(append(line, " name="...), name...)
os.Stdout.Write(line)
// or, in an arena: s := "name=" + name in a

// os.ReadFile(path): read into caller storage
var data [1M]byte
f := try os.Open(path)
n, _ := f.ReadFull(data[:])

// a function returning a new slice: take the output buffer
func Squares(n int, out []int) []int {
    out = out[:0]
    for i := 0; i < n; i++ {
        out = append(out, i*i)
    }
    return out
}

// recursive tree walk: explicit stack
var stack [..64]*Node
stack.Push(root)
for len(stack) > 0 {
    n := stack.Pop()
    for _, c := range n.Children {
        stack.Push(c)
    }
}

// building a tree: allocate in an arena
arena tree 256K {
    root := new(Node) in tree
    root.Children = append(root.Children, new(Node) in tree) in tree
}

// interface-like dispatch: switch on a kind, or a func-typed field holding a named function
type Shape struct{ Kind int; W, H float64 }
func Area(s Shape) float64 {
    switch s.Kind {
    case Circle:
        return math.Pi * s.W * s.W
    }
    return s.W * s.H
}

// errors with context: use codes and constant messages
const ErrNoUser = error(404, "user not found")
if err != nil {
    println("lookup failed for", name, ":", err.Error())
    return ErrNoUser
}

// HTTP handler
func handle(w *http.ResponseWriter, r *http.Request) {
    w.SetHeader("Content-Type", "application/json")
    w.WriteHeader(201)
    w.WriteString(`{"ok":true}`)
}
```

## 9. Runtime behaviour

- One OS thread by default. Goroutines are cooperative and switch at blocking operations (and in
  preemptible tasks' loops). Netpoll is kqueue on macOS and epoll on Linux.
- A watchdog thread asks a goroutine that has run for a time slice to yield (only preemptible
  tasks do), and prints a warning naming the task when a goroutine has held its thread for 100ms
  while other goroutines waited on it. Both count the CPU time the thread actually got, so an
  overloaded machine that doesn't run the thread doesn't make its goroutine look slow. A program
  with nothing else to run isn't warned about.
  `runtime.Gosched()` yields by hand.
- `threads N` (1 to 1024, or `auto` for one per core) runs N scheduler threads (shards) in one
  process. The directive says the program is written for threads; without it a program runs one
  thread. `threads auto` is the usual choice: one thread per core, and `SQUARK_THREADS=N` in the
  environment overrides the count at run time (like `GOMAXPROCS`). `SQUARK_THREADS` also
  overrides `threads N`, except that with a `budget` it can't go above N, the count the budget
  was proven for. It has no effect on programs without the directive. Each shard runs `main`
  with its own package variables (see `thread var`, §4), goroutine pools, timers and poller;
  goroutines never move between shards, so rule 10 holds within each and nothing needs locks. Error constants are shared (they're read-only). Listening on an address from every shard
  works: on Linux each shard gets its own socket (`SO_REUSEPORT`) and the kernel spreads
  connections; on macOS the shards share one socket. `runtime.Shard()` and `runtime.Shards()`
  report where a goroutine runs. The process exits when every shard's `main` has returned (a
  shard whose `main` returns stops its goroutines), at the first panic, or at `os.Exit`. The
  memory budget counts static data and stacks once per shard (and `threads auto` can't be
  combined with `budget`). `threads` and `workers` combine: workers × threads shards in all.
  Shards share only constants, `shared var` data and shared channels. A shard whose goroutines
  all wait on shared channels sleeps until another shard wakes them, so a deadlock across shards
  hangs instead of reporting `all goroutines are asleep`. Externs are called from whichever shard
  runs the goroutine (or a helper thread, for `blocking` externs), so C libraries must be
  thread-safe (libc and SQLite are; each shard opens its own SQLite connection, since package
  variables are per shard: `thread var db sqlite.DB`).
- `workers N` (1 to 1024, or `auto` for one per core) starts N processes before any squark code
  runs; each has its own package variables, goroutines and connections. Listening sockets use
  `SO_REUSEPORT`, so the kernel spreads connections across the workers (on Linux; macOS sends them
  all to one). The starting process supervises: SIGINT, SIGTERM and SIGHUP are passed on to every
  worker, and if one exits with a non-zero status the others are stopped and that status is the
  program's. On Linux workers also stop if the supervisor is killed. The memory budget counts code
  once and everything else once per worker; `workers auto` can't be combined with `budget`. SQLite
  connections wait up to 5 seconds for another worker's lock.
- Each task pool has N stacks of a size proven at compile time, each with a guard page. A C function
  that exceeds its `stack` budget hits the guard: `fatal error: stack overflow in goroutine [pool]`.
- Panics print `panic: <message>` with the goroutine and source position, then exit with status 2.
  There is no `recover`. A nil dereference becomes `panic: runtime error: invalid memory address or
  nil pointer dereference`.
- When every goroutine is blocked with nothing to wake it: `fatal error: all goroutines are asleep -
  deadlock!`.
- `SQ_STACKCHECK=1` reports, per pool, the proven and the deepest observed stack use at exit.
- C libraries (e.g. SQLite) can use malloc. squark code, the runtime and the shims never do, and the
  build verifies this.

## 10. Toolchain

```sh
squark build [-o out] [-v] [-work dir] [dir]   # -v prints the memory breakdown
squark run [-v] dir [args...]
squark check [dir]                              # type and lifetime check only
squark doc [dir] [pkg]                          # API with provenance; `squark doc std` for the stdlib
squark emit-c [dir]                             # the generated C
```

## 11. A complete program

```go
package main

import (
    "http"
    "sqlite"
    "strconv"
)

budget 64M
pool http.serveConn 128

var db sqlite.DB
var countStmt sqlite.Stmt

func handle(w *http.ResponseWriter, r *http.Request) {
    if r.Path != "/count" {
        w.WriteHeader(404)
        return
    }
    defer countStmt.Reset()
    row, err := countStmt.Step()
    if err != nil || !row {
        w.WriteHeader(500)
        return
    }
    var buf [24]byte
    w.Write(strconv.AppendInt(buf[:0], countStmt.ColumnInt(0)))
}

func setup() error {
    d, err := sqlite.Open("app.db")
    if err != nil {
        return err
    }
    db = d
    try db.Exec("CREATE TABLE IF NOT EXISTS items(id INTEGER PRIMARY KEY)")
    countStmt = try db.Prepare("SELECT count(*) FROM items")
    return nil
}

func main() {
    if err := setup(); err != nil {
        println("setup:", err.Error())
        return
    }
    if err := http.ListenAndServe(":8080", handle); err != nil {
        panic(err)
    }
}
```
