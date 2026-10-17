# squark language reference

This is the precise reference for writing squark. squark grew out of Go, and its semantics are
still Go's wherever this document doesn't say otherwise (integer arithmetic, slices, strings,
`switch`, `select`, `defer`, method sets...). Its syntax is its own: this document describes all of
it. The standard library API is in [stdlib.md](stdlib.md), which is generated from the compiler and
always current.

## 0. The ten rules that matter most

1. **There is no heap.** Memory is static (package level), on a task's stack, or in an `arena`.
   `new`, run-time-sized `make`, string `+` and copying conversions are only allowed in an arena:
   `x := new(T) in a`.
2. **References may not outlive their memory.** The compiler checks this across function calls.
   Returning `&local`, or `buf[:]` of a local array, is an error: take the storage as a parameter
   instead.
3. **Callers provide buffers.** APIs take a `u8[]`/`T[]` to fill and return a view of it:
   `strconv.append_int(dst, n)`, `d.string(buf)`, `stmt.column_text(i, buf)`.
4. **Capacities are fixed.** `append` past capacity panics (unless `... in arena`), maps have a
   declared capacity (`V[K; N]`), and channels have one (`T chan[N]`).
5. **No recursion**, direct or indirect, because every stack size is proven at compile time. Use a
   loop with an explicit stack such as a `T[..N]` vector.
6. **No closures, method values, interfaces or generic types.** Use named functions, func-typed
   values of named functions, `switch`, and concrete types. For code that works on several types,
   a function's parameters can be `anytype` (§4): each call makes a copy for its argument types.
7. **`start` only starts a `task`:** `task f(args) pool N { ... }`. Tasks have no results.
   Arguments to `start` and values sent on channels must not reference stack or arena memory.
8. **Errors are values with a constant message:** `fault NotFound 404 "not found"`. They can't be
   formatted with dynamic text; use the code, or log separately. A `!` after a call returns its
   error from the enclosing function: `f := os.open(path)!`.
9. **`str(b)` is a zero-copy view of `b`.** `u8[](s)` is an error without an arena.
10. **Cooperative within a thread; threads share nothing mutable.** A thread's goroutines switch
    only at blocking operations (I/O, channel operations, `time.sleep`, `start` on a full pool,
    `runtime.gosched()`) and in the loops of tasks marked `preemptible`. So data shared between a
    thread's goroutines needs no locks as long as you don't block mid-update. With `threads N` (or
    `workers N`) each thread runs its own copy of `main` with its own package variables; threads
    share only read-only data and shared channels. No lock is ever needed. A busy loop in an
    ordinary task holds up its thread: mark CPU-heavy tasks `preemptible`, and the runtime warns
    about a goroutine that holds its thread for 100ms while others wait.

## 1. Lexical elements

- **Keywords:** `and`, `arena`, `break`, `case`, `chan`, `const`, `continue`, `default`, `defer`,
  `else`, `enum`, `extern`, `fault`, `fn`, `for`, `if`, `in`, `not`, `or`, `pub`, `return`,
  `struct`, `switch`, `task`, `var`, `while`.
- **Contextual words** (identifiers elsewhere):
  - `program`, `module` and `use` at the start of a file; `as` in a `use`.
  - `thread` and `shared` before a package-level variable.
  - `start` before a call (`start f(x)`), `loop` before `{`, `with` before `arena` or
    `deadline`, `select` before `{`.
  - `pool`, `stack`, `blocking` and `preemptible` after a signature; `c"symbol"` after an extern's.
  - `static` after `in` in a signature.
  - `budget`, `pool`, `workers`, `threads`, `link`, `cflags` and `ldflags` in the file header.
- **Member names may be spelled like keywords.** After a `.`, as a struct field, as a method's
  name, as an enum member and as a key in a struct literal, any word is a name: `r.arena`,
  `Pair{in: 1}`, `Kind.int`.
- **Operators** beyond the arithmetic and comparison ones: `and`, `or` and `not` (logic), `!` after
  an expression (propagate its error, §5), `->` (results), `..` (ranges and vector types), `<-`
  (channels), `in` (allocate in an arena; provenance). `&&`, `||` and a prefix `!` are errors that
  name the word to write.
- **Size literals:** a decimal integer immediately followed by `K`, `M` or `G` is multiplied by 2^10,
  2^20 or 2^30: `64K == 65536`.
- **Pragmas:** `//squark:noinline` before a function stops clang from inlining it. No other pragmas
  exist.
- **Semicolons** are inserted at the end of a line that ends in an identifier, a literal, `)`, `]`,
  `}`, `!`, `chan`, `++`, `--`, `return`, `break` or `continue`, as in Go. Comments, string, rune and
  number literals are as in Go.
- **Names:** by convention functions, methods, fields, variables and constants are `snake_case`;
  types and faults are `PascalCase`. Visibility doesn't depend on the spelling (see `pub`, §2).

## 2. Program structure

```squark
program {                     // the main package; a library file starts with `module name`
    budget 128M               // program only: fail the build if worst-case memory exceeds this
    pool http.serve_conn 512  // program only: resize a task's pool (pkg.task or task)
    workers 4                 // program only: run 4 processes (or auto: one per CPU); see §9
    threads auto              // program only: a scheduler thread per CPU (or N); see §9
    link "sqlite3"            // link a C library (any package)
    cflags "-I/opt/include"   // C compiler flags for this package's .c files
    ldflags "-L/opt/lib"      // linker flags
}

use http, math/rand, ./mylib  // std packages by path, project packages by relative path
use strings as s              // a different name for an import

const ...
enum ...
fault ...
thread ... / shared ...
struct ... / Name: T
fn ... / task ... / extern fn ...
arena ...
```

- A package is a directory of `.sq` files. Every file starts with the same header: `program` for
  the main package, `module NAME` for a library (its name, as importers use it). The `{ ... }`
  block of directives is optional and may be in any of the package's files. `.c` files in the
  same directory are compiled and linked as C shims.
- `use` lists imports: unquoted paths (`math/rand`, `./lib`, `../util`), or quoted ones for paths
  with other characters. `use ( ... )` takes one per line.
- `fn main()` in the program is the entry point; when it returns, the program exits.
- There are no `init` functions. Package-level initializers run in declaration order, packages in
  import order. An initializer that reads a variable not initialized yet (a later one, or itself
  through a function, `thread x = f()` with `f` reading `x`) sees its zero value.
- **`pub`** makes a package-level declaration (`pub fn`, `pub type`, `pub const`, `pub fault`,
  `pub thread`, `pub arena`...) or a struct field (`pub name str`) visible to importers; in a
  group, `pub` goes before each spec it applies to (or before the group for all of them).
  Nothing imports the program, so its declarations need no `pub`, and the standard library's
  `anytype` functions can use the fields and methods of the program's types.

## 3. Types

Types are written with their modifiers after the element type, read left to right:
`u8[64]` is "u8, 64 of them". A pointer `*` comes first and binds tighter than the brackets, as
in C: `*Node[]` is a slice of `*Node`; a pointer to an array is `*(u8[64])`.

| type | notes |
|---|---|
| `bool`, `str`, `int` (64-bit), `i8` `i16` `i32` `i64`, `uint` (64-bit), `u8` `u16` `u32` `u64`, `uintptr`, `f32`, `f64` | as in Go; `rune` is `i32` |
| `rawptr` | untyped C pointer (`void*`) for extern calls; converts to and from pointers and `uintptr`, and is not lifetime-checked |
| `error` | a value: integer code + constant message (§4). Methods `message() str`, `code() int`. Zero value is `nil`, whose `message()` is `""` and `code()` is 0 (not a panic: an error isn't an interface) |
| `arena` | a reference to arena storage. Methods `used()`, `left()`, `cap()` (all `int`) |
| `T[N]` | array (storage) |
| `T[]` | slice (view) |
| `T[..N]` | vector: inline storage for N elements plus a length (storage) |
| `f32x4`, `u8x16`, … | SIMD vector: N numbers that operators work on lane by lane (below) |
| `V[K; N]` | map storage with fixed capacity N (`cap(m)` is N, also through a view; 0 for a nil map). K is an integer, string, bool or pointer type |
| `V[K]` | map view |
| `T chan[N]` | channel storage with an N-element buffer (`chan[0]` = unbuffered) |
| `T chan` | channel view (no direction types) |
| `*T`, `struct { ... }`, `fn(int) -> bool` | pointer, struct, function. A func value can only be a named top-level function |

`T[x]` is an array when `x` is a constant and a map when `x` names a type: `int[n]` with `const n
= 4` is an array, `int[str]` a map. A function or channel-view element is parenthesized:
`(fn() -> int)[]`, `(Job chan)[4]` (brackets right after `chan` are its capacity:
`Req chan[8][4]` is four channels of eight).

Not supported: interfaces, generic types (functions can take `anytype` parameters, §4), complex
numbers, type aliases, embedded fields, local type declarations, directional channel types, and
named types that refer to themselves only through pointers, slices, arrays and func types
(`State: fn() -> State`, `L: L[]`): put a struct in between
(`struct State { next fn() -> State }`).

### Storage and views

A storage value (`T[N]`, `T[..N]`, `V[K; N]`, `T chan[N]`) contains its elements inline. Declaring
it is the allocation, and its zero value is ready to use:

```squark
thread seen int[str; 1024]
seen["x"]++                  // no make needed
shared jobs Job chan[16]     // one channel for every scheduler thread (§4)
var line u8[..256]
```

Where a view type is expected (assignment, argument, return), addressable storage converts to a view
implicitly. Indexing, `for ... in`, `len`, `delete`, `close`, sends and receives work on storage
directly:

```squark
fn count(m int[str], w str) { m[w]++ }
count(seen, "x")             // seen converts to int[str]
for k, v in seen {}
```

- The view points into the storage, so the storage must outlive it (rule 2).
- Map and channel storage **cannot be copied** (`m2 := m` is an error), because they point into
  themselves. Pass a view. The same holds for a struct, array or vector that contains map or
  channel storage: copying one out of a variable is an error (use a pointer), while a fresh value
  (`T{}`, a call result) is fine because its storage is still unused. This covers every copy:
  `copy`, `append(s, xs...)`, a vector's `append`, an `append` that grows into an arena, calling a
  method that takes `self` (take `&self` instead), and a named result (it is a
  variable that is copied out on return; return an unnamed result built by a literal instead).
- Arrays and vectors copy by value, like Go arrays.

### Vectors `T[..N]`

```squark
var v int[..8]
v.push(1)            // panics if len(v) == 8
ok := v.try_push(2)  // false if full
v.append(xs)         // appends an int[]; panics if it doesn't all fit
ok = v.try_append(xs) // false (and appends nothing) if it doesn't all fit
x := v.pop()         // panics if empty
v.clear()            // len(v) = 0
len(v); cap(v)       // cap is the constant 8
v[i]                 // bounds-checked against len(v)
v[:]                 // an int[] view with len == cap == len(v)
for i, x in v {}
w := Point[..4]{{1, 2}, {3, 4}}   // literal: len 2, cap 4
```

A byte vector's `append` and `try_append` take a string too: a `u8[..N]` holds text by value, so
it can go on a shared channel to another thread (`str(v[:])` reads it back).

`push`, `try_push`, `append`, `try_append`, `pop` and `clear` need an addressable vector. `len`,
`cap`, indexing and `for ... in` also work on a vector that isn't in a variable (a call's
result), reading a copy; its elements can't then be assigned or have their address taken. A
pointer to a vector works like the vector (`p.push(x)`, `len(p)`, `p[i]`), as a pointer to an
array does. Vectors aren't comparable (`==`), nor is a struct or array containing one.

### SIMD vectors

A SIMD type holds a fixed number of lanes of one number type and compiles to SIMD instructions
(NEON on arm64, SSE/AVX on x86-64). There is one for each integer and float element type, 128
and 256 bits wide:

| element | 128-bit | 256-bit |
|---|---|---|
| `i8`, `u8` | `i8x16`, `u8x16` | `i8x32`, `u8x32` |
| `i16`, `u16` | `i16x8`, `u16x8` | `i16x16`, `u16x16` |
| `i32`, `u32` | `i32x4`, `u32x4` | `i32x8`, `u32x8` |
| `i64`, `u64` | `i64x2`, `u64x2` | `i64x4`, `u64x4` |
| `f32` | `f32x4` | `f32x8` |
| `f64` | `f64x2` | `f64x4` |

A 256-bit vector is two 128-bit registers where the CPU has no wider ones (arm64, x86-64
without AVX).

```squark
a := f32x4{1, 2, 3, 4}         // a literal; missing lanes are 0
b := f32x4(x)                  // broadcast: x (an f32) in every lane
c := f32x4(xs[i:])             // load xs[i], …, xs[i+3]; panics if len(xs[i:]) < 4
u := u8x16(s[i:])              // a byte vector also loads from a string
d := f32x4(i32x4{1, 2})        // lane by lane conversion: same lane count
a*b + c                        // operators work lane by lane
a * 2                          // an untyped constant is broadcast
m := a > b                     // a mask: i32x4, -1 where a > b, 0 elsewhere
m.select(a, b)                 // a where the mask is nonzero, b elsewhere
m.any(); m.all()               // some / every lane nonzero
m.first(); m.last()            // index of the first / last nonzero lane, or -1
min(a, b); max(a, b, c)        // lane by lane
a.sum(); a.min(); a.max()      // across the lanes, giving an f32
a.store(ys[i:])                // write the lanes to ys[i:]; panics if len(ys[i:]) < 4
a[2]; a[2] = 7; len(a)         // a lane (bounds-checked); len is the constant 4
a.swizzle(3, 2, 1, 0)          // rearrange lanes: lane i of the result is a[index i]
a.shuffle(b, 0, 4, 1, 5)       // the same from a and b: indexes 0-3 are a's, 4-7 are b's
t := u8x16("0123456789abcdef") // a table, loaded from a constant
t.lookup(x >> 4)               // lane i is t[(x>>4)[i]]: x a u8x16 (or u8x32)
```

- Operators: `+ - *` on all vectors, `/` on float vectors only (no CPU divides integer vectors:
  divide the lanes one at a time), `& | ^ &^` and unary `^` on integer vectors, unary `-`.
  Integer lanes wrap. `<<` and `>>` shift every lane of an integer vector by one count, as in
  Go: a count of at least the lane width gives 0 (or the sign for `>>` of a signed lane), and a
  negative count panics.
- Both operands have the same type. An untyped constant is broadcast; a typed scalar must be
  broadcast with a conversion (`v * f32x4(k)`).
- Comparisons give a mask, a signed integer vector as wide as the lanes (`f64x2` compares to
  `i64x2`), not a `bool`. Compare whole vectors with `(a == b).all()`. So SIMD vectors can't be
  map keys and a struct holding one can't be compared with `==`.
- A lane of a variable can be assigned (`v[i] = x`, `v[i] += x`, `v[i]++`), but not addressed
  (`&v[i]` is an error). A pointer to a SIMD vector works like the vector (`p[i]`, `p.sum()`).
- `swizzle` and `shuffle` take one constant index per lane; they compile to the CPU's shuffle
  instructions (reversing an `i32x4` is two arm64 instructions).
- `lookup` is a table lookup with variable indexes, on a 16-lane byte vector (`u8x16` or
  `i8x16`), the table. It takes 16 or 32 byte indexes (`u8x16`, `u8x32` or the signed ones) and
  gives as many lanes of the table's type: lane `i` is `t[idx[i]]`, or 0 where `idx[i]` is 16 or
  more (a negative signed index is 128 or more). It is one instruction on arm64 (`tbl`) and
  x86-64 from `-cpu x86-64-v2` (`pshufb`); baseline x86-64 has none and looks the lanes up one at
  a time. Decoders use it to map nibbles to classes or digits (hex, base64, UTF-8 validation).
- `sum` adds the upper half of the lanes to the lower half until one lane is left, so the order of
  float additions is fixed, the same on every CPU. `min` and `max` pick lanes the way the builtin
  `min` and `max` do.
- Converting float lanes to integer lanes saturates, as scalar conversions do (§5).
- `any`, `all`, `first`, `last` and `select` are methods of integer vectors; `select` takes two
  vectors with the mask's lane count and lane width (an `i32x4` selects between `f32x4`s or
  `u32x4`s).
- `println(v)` prints `[a b c d]`.
- SIMD types can't be constants. They have no references, so they go anywhere a number can:
  struct fields, arrays, channels, `start` arguments.
- A named type can have a SIMD type (`Vec4: f32x4`) and keeps its operators and methods; its
  own methods can't be named like a built-in one.

Most loops vectorize without these types (§9). Use them where clang can't: float sums (it keeps
the source order), searches that stop early, and data-dependent selects.

## 4. Declarations

### Functions, methods, tasks, externs

```squark
fn name(a int, b, c str) -> int { ... }      // parameters as in Go: a name list, then the type
fn f() { ... }                               // no results
fn f() -> (int, bool) { ... }                // several results
fn f() -> (n int, err error) { ... }         // named results
fn open(path str) -> File ! { ... }          // ! adds an error result: (File, error)
fn check() -> ! { ... }                      // just an error
fn divmod(a, b int) -> (int, int) ! { ... }  // (int, int, error)

struct Stack {
    items int[..64]
}

impl Stack {                                 // methods: in impl blocks, self or &self first
    fn push(&self, x int) { ... }            // &self: a pointer to the Stack (*Stack)
    fn len(self) -> int { ... }              // self: a copy of it
}
impl Stack {                                 // another impl block (in any file of the package)
    fn pop(&self) -> int { ... }
}

task name(params) pool N { ... }             // a goroutine entry point; no results
task name(params) pool N stack 64K { ... }   // also assert a stack upper bound
task name(params) pool N preemptible { ... } // its loops may yield after a time slice

extern fn sqrt(x f64) -> f64 stack 1K                        // the C function of the same name
extern fn read(fd int, b u8[]) -> int stack 8K c"sq_read"    // the C symbol differs from the name
extern fn query(q str) -> int stack 128K blocking c"slow_query" // runs on a helper thread
```

- A method is declared in an `impl T { ... }` block, for a struct, enum or named type of the same
  package; a type may have any number of impl blocks, in any of the package's files. Its first
  parameter is `self`, a copy of the value (type `T`), or `&self`, a pointer to it (type `*T`), and
  `self` is an ordinary variable in the body (`self.n += 1`). It is called `x.m(...)`; as in Go, a
  `&self` method can be called on an addressable value (its address is taken) and a `self` method
  on a pointer.
- Each method's name belongs to its type, so two types can both have a `close`.
- `pool N`: at most N instances run at once. `start` on a full pool waits for a slot.
- `preemptible` on a task: once one of its goroutines has run for a time slice (about 10ms)
  without blocking, it yields at the next loop iteration, in its own code or any function it
  calls, so a long computation doesn't hold up its thread. Those functions get a check at each
  loop iteration (a load and a branch, too cheap to measure); called from other goroutines they
  never yield. Only a preemptible task can be interrupted mid-loop, so keep it from leaving state
  that other goroutines read half-updated across a loop iteration (or update it in one step).
- `stack N` on an extern is required: it's the C function's stack budget used in the stack proof.
  On a task it is an assertion, and the build fails if the proven size is larger.
- Extern parameter types map to C as follows: `int`/`i64` → `int64_t`, `i32` → `int32_t`, and so
  on; `str` → `struct { const uint8_t *p; int64_t len; }`; `u8[]` → `struct { uint8_t *p; int64_t
  len, cap; }`; `*T`/`rawptr` → pointers.
- Extern results are trusted to point to static memory unless provenance is declared.
- `blocking` on an extern runs each call on a helper thread while the calling goroutine waits, so
  a C call that blocks (a slow query, a lock wait, a read from a slow disk) doesn't stop the other
  goroutines of its scheduler thread. Each call costs two thread handoffs (microseconds), so mark
  only calls that always wait. Arguments may point into the goroutine's stack (it stays put while
  the goroutine waits). The C function's `stack N` budget applies to the helper threads (two per
  scheduler thread, counted in the memory report, each with at least 64 KiB of stack), not to
  goroutine stacks.
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

### anytype parameters

```squark
fn slice(s anytype, less anytype) { ... }    // sort.slice(entries, by_count)
fn write_string(w anytype, s str) { ... }    // json.write_string(&buf, s), json.write_string(w, s)
```

A parameter of type `anytype` takes its type from each call, as in Zig: the compiler makes a copy
of the function for each combination of argument types it is called with (an instance), and
checks, proves and compiles each copy like any other function. The body may use the parameter
however its type allows (index it, call it, call its methods); a type that doesn't fit is an error
at the call, reported where the body uses it, with the instance named ("in sort.slice with
s main.Entry[], called at main.sq:40"). Nothing happens at run time and nothing is boxed; each
instance is code, as if written by hand for those types. Storage passed as `anytype` (a map or
channel storage, or a vector `T[..N]`) becomes its view (a vector becomes the slice `T[]` of its
elements, so the body can't `push`; pass `&v` for that), and untyped constants their default type.

Functions and methods take `anytype` parameters (tasks and externs don't), not as their variadic
parameter, and results have ordinary types (`bufio.Reader.read_line(src anytype)` reads from a
conn or a file alike). A function with `anytype` parameters can only be called, not used as a
value.

### Provenance (optional)

```squark
fn cut(s, sep str) -> (str, str, bool) in s    // results may point into what s refers to
fn link(a *Node, b *Node in a)                 // b may be stored inside a
fn remember(msg str in static)                 // msg must point to static memory
fn name() -> str in static                     // the result points to static memory
```

- The compiler infers all of this anyway (see `squark doc`). Declaring it makes it a checked
  contract: the build fails if the body does something the declaration doesn't allow, and the error
  says which clause to add.
- On functions with a body, the declaration names which parameters and results are related, and the
  compiler works out the pointer levels.
- On externs, the declaration is the only information the compiler has.

### Arenas

```squark
arena interned 8M              // package level: static, bump-only, never freed

fn f() {
    with arena scratch 64K {   // statement: storage in this stack frame
        p := new(Node) in scratch
    }                          // everything allocated in scratch is gone here
}

fn build(a arena, n int) -> *Node { return new(Node) in a }   // allocate in the caller's arena
```

- Sizes are constants.
- Every declared size (a type, an arena, a capacity, the budget) is at most 1T (2^40 bytes or
  elements), and a task pool at most 2^24 goroutines.
- A full arena panics: `panic: arena scratch is full (capacity 65536, used …, need …)`.
- `append(s, x) in a` grows a slice to twice its capacity (at least what it needs, and at least 4
  elements). A slice that is the arena's latest allocation grows in place; any other is copied,
  and the old elements stay allocated until the arena ends. Size arenas for that, or collect into a
  stack buffer or vector first and copy into the arena at the exact size.
- Every HTTP request carries one: `r.arena`.

### Constants and faults

`const` declares constants as in Go (`const n = 4`, `const ( a = 1; b = 2 )`; every constant
has a value: there is no `iota` and no implicit repetition, an `enum` names a set of numbers),
plus constant composites: arrays and structs whose elements are constants (numbers, strings, bools, errors, or
arrays and structs of them):

```squark
const primes = int[5]{2, 3, 5, 7, 11}
const defaults = Config{port: 8080, name: "notes"}
```

`fault` declares an error constant: a name, an optional integer code and a constant message.

```squark
pub fault NotFound 404 "not found"
fault Closed "closed"                 // code 0
fault (
    Malformed -40 "http: malformed response"
    TooLarge -41 "http: too large"
)
if err == NotFound { ... }            // faults compare with ==
```

Both are read-only data in the binary, shared by every scheduler thread (`threads N`). Reading,
indexing, copying and `for ... in` work as for variables; assigning to them or their elements,
taking their address, slicing them and calling pointer methods on them are errors. Copy into a
variable first (`t := primes`) to get something writable. `error(code, msg)` and `error(msg)`
build an error value anywhere (§5).

### Enums

```squark
enum Dir { north, east, south, west }        // 0, 1, 2, 3
pub enum Status u16 {                        // an integer type other than int
    ok = 200
    created                                  // 201: one more than the member before it
    not_found = 404
}

d := Dir.east                                // members are reached through the type
d = .west                                    // or with just .name where a Dir is expected
switch s {
case .ok, .created:
}
turn(.north)                                 // fn turn(d Dir): the parameter says which enum
if d == .south { }
impl Dir {                                   // enums have methods like any named type
    fn right(self) -> Dir { return Dir((int(self) + 1) % 4) }
}
var hits int[Dir; 4]                         // a map keyed by the enum
```

An enum is a named integer type (`int` unless another integer type follows the name), and its
members are constants of that type: the first is 0 unless written, each other one the member before
plus one unless written. Members are separated by commas or newlines. Being its own type, an enum
doesn't mix with plain integers without a conversion (`int(d)`, `Dir(3)`), and it can have methods
like any named type. A member is visible to importers when the enum is `pub`. Enums are declared at
package level.

Where the type the context expects is an enum, `.name` is that enum's member: in an argument, an
assignment or typed declaration (`var d Dir = .north`, `const home Dir = .south`), a return value,
a struct or slice literal element, a map key, a channel send, a `case` of a `switch` on an enum,
and either side of a comparison with an enum. Elsewhere (`x := .north`, an array index) write
`Dir.north`.

### Variables

Inside a function, `var x T`, `var x = e` and `x := e` declare variables as in Go. A package-level
variable says whether each scheduler thread has its own copy:

```squark
thread hits int                   // one copy per scheduler thread (the only copy without threads N)
thread db sqlite.DB               // each thread's own connection
shared crc_table = make_table()   // one copy for all threads, read-only after start-up
thread (
    a int
    b = 2
)
```

A `thread` variable is ordinary mutable state: under `threads N` a counter counts one thread's
work and a cache holds one thread's entries. Without `threads N` there is one thread, so `thread`
just means a package variable.

A `shared` variable's initializer runs once, on the first scheduler thread and in order with the
other package initializers, before any other thread starts. Afterwards it is read-only, with the
same rules as constants. A string in it must be constant data (as for error messages, §5): a
string viewing a package variable would change when that thread changes the variable, so copy it
into a package arena instead (`return str(buf[:n]) in pkg_arena`). Its type is limited to numbers,
strings, bools, errors and arrays or structs of them, because nothing a shared slice, map or
pointer refers to could be kept read-only.

A shared channel connects the scheduler threads: `shared jobs Job chan[64]` (declared without a
value) is one channel for all of them. Send, receive, `close`, `len` and `for ... in` work as for
any channel; a goroutine blocked on it is woken by whichever thread completes the operation. Its
elements may not hold references (no strings, slices, maps or pointers), since a reference could
view one thread's own memory; send numbers, bools, errors, or arrays and structs of them (an index
or ID instead of a string). Arrays and structs of shared channels work too: `shared inbox Req
chan[8][4]` gives each thread its own request channel and `shared replies int chan[1][64]` each
client its own reply channel; other fields of such a struct are read-only data.

### Types

```squark
struct Point {
    x, y int
    pub label str      // pub: visible to importers
}
Celsius: f64           // any other named type: Name: T
pub Handler: fn(r *Request) -> int
```

`struct Name { ... }` declares a struct type and `Name: T` (at package level) a named type with any
other underlying type; each is a type of its own, as in Go: a `Celsius` doesn't mix with an `f64`
without a conversion. Methods are declared with
`impl` blocks (above): `impl Celsius { fn fahrenheit(self) -> f64 { return f64(self)*9/5 + 32 } }`.
An unnamed struct type is written
`struct { x, y int }` where a type goes.

## 5. Expressions

| expression | squark |
|---|---|
| `x in a` | allocations made by `x` come from arena `a`. Lowest precedence: `a + b + c in ar` is one allocation |
| `.name` | the member `name` of the enum the context expects (§4) |
| `new(T) in a` | zeroed `*T` in the arena. `new(T)` without an arena is an error |
| `make(T[], n[, c]) in a` | run-time sizes are OK. `make(V[K], n) in a` also works. `make` without an arena is an error, as is `make(T chan)` |
| `append(s, xs...)` | panics if the result exceeds `cap(s)`. `append(s, x) in a` grows into the arena instead |
| `s + t` (strings) | only if both are constants, or `in a` |
| `str(b)` | zero-copy view of `b`'s bytes. `str(b) in a` copies into the arena, and so does `str(s) in a` for a string `s` (to keep a string beyond the memory it views) |
| `u8[](s)` | only `u8[](s) in a` (copies). Otherwise `copy(buf[:], s)` |
| `str(r)` (rune) | only for constants; use `utf8.append_rune` |
| `f(...)!` | if `f`'s last result (an `error`) is non-nil, return it from the enclosing function (whose last result must be `error`), with zero values for the other results (named results too, even if already assigned). Otherwise yields the remaining results. It binds like a call: `f()!.x`, `a.b()!` |
| `error(code, msg)`, `error(msg)` | builds an error. `msg` must be constant data: a string constant, another error's `message()`, a `shared` variable, a C extern's result, or a string copied into a package arena (`str(b) in pkg_arena`). A package variable's contents can change, so it can't be a message |
| `not x`, `x and y`, `x or y` | logic, short-circuiting as in Go |
| `&T{...}` | address of a temporary that lives until the end of the enclosing block |
| `T[]{...}`, `T[N]{...}`, `T[...]{...}`, `T[..N]{...}` | slice, array and vector literals; a slice's backing array lives until the end of the enclosing block |
| `(*T)[]{...}` | a slice of pointers: in an expression `*T[]` would dereference `T[]` |
| map literals | not supported; declare storage and assign |
| `f(g())` with a multi-value `g` | not supported; assign `g`'s results first |
| `T[N](s)`, `(*(T[N]))(s)` (slice to array) | not supported; `copy(arr[:], s)` |
| `(*T)[]{{...}}` (elided `&T`) | not supported; write `&T{...}` |
| function literals, method values, method expressions | not supported |
| type assertions, type switches | not supported (no interfaces) |

In the condition of `if`, `for`, `while` and `switch`, a `{` ends the expression, so a composite
literal there needs parentheses unless its type can't be anything else: `for x in int[]{1, 2}` is
fine, `for x in (int[2]{1, 2})` needs them (`int[2] {` could be an index and a block).

Integer arithmetic wraps on overflow, division by zero panics, shifts by at least the width
yield 0 (or -1 for negative signed `>>`), and a negative shift count panics, as in Go.
Converting a float to an integer type when the value doesn't fit saturates: the result is the
type's minimum or maximum (Go leaves it implementation-defined), and NaN converts to 0.
Dereferencing a nil pointer, or calling a nil func value, panics (§9), whatever the offset.

**Order of evaluation.** Calls, method calls and channel receives run left to right as in Go, and
`a, b = ...` evaluates every index and pointer operand on the left before storing anything. The
differences, all about side effects within one statement:

- `a[i] = f()`: `f()` runs before the index `i` is evaluated (Go: `i` first).
- `getm()[key()]`, read or write: the key is evaluated before the map.
- `getch() <- v`: `v` is evaluated before the channel.
- Keyed composite literals (`T{b: f(), a: g()}`, `int[3]{2: f(), 0: g()}`) evaluate their elements
  in field or index order, not source order.

If the order matters, compute the operands into variables first.

**Floating-point `min` and `max`** don't implement Go's special cases: with a NaN operand the result
is not necessarily NaN, and `min(0.0, -0.0)` may return either zero.

### Builtins

`len`, `cap`, `append`, `copy` (also `copy(dst u8[], src str)`), `delete`, `close`, `min`, `max`,
`panic(x)` (x is a string, integer, bool or error), `print`/`println` (write to **stdout** a line
at a time, so threads' lines don't mix; a partial line is written before other output to stdout,
and every thread's at exit or a panic; accept numbers, strings, bools, pointers, errors and `u8[]`),
`new`/`make` (arena only).

## 6. Statements

```squark
for x in xs { }            // each element (a slice, array, vector, string's runes, map's values)
for i, x in xs { }         // index (map: key) and element
for k, _ in m { }          // just the keys
for v in ch { }            // a channel's values until it is closed
for i in 0..n { }          // 0, 1, …, n-1; n is evaluated once
for i in lo..hi { }        // lo, …, hi-1 (both integers of one type; an untyped one takes the other's)
for _ in 0..3 { }
for i := n - 1; i >= 0; i-- { }   // the three-clause loop, as in Go
while cond { }
loop { }                   // until break or return
outer: for ... { break outer }    // labels on for, while, loop, switch and select
if v, ok := m[k]; ok { } else { }
switch x { case 1, 2: ... default: ... }
select { case v := <-c: ... case d <- 1: ... default: ... }
start worker(i)            // start a task (rule 7)
defer f.close()
with arena scratch 64K { }
with deadline 2 * time.second { }
```

- `for ... in` always declares its variables, fresh for each iteration (assigning one doesn't
  change the iteration). One variable is the element, except over a channel or a range, where it
  is the value. Over an array the loop reads the array itself, not a copy: changes made in the
  loop are seen by later iterations (copying would put a second array on the stack); loop over a
  copy explicitly if you need Go's behaviour.
- A map loop may delete entries as it goes, as in Go. Inserting a *new* key inside the loop may
  make it skip or repeat entries (to reuse deleted slots, a map with many deletions compacts
  itself on insert); collect the keys first if you need both.
- `select` works as in Go: it waits until one of its sends or receives can proceed (a random one
  if several can), `default` runs if none can, nil channels are never ready, and `break` leaves
  it. `select {}` blocks forever. Cases may mix per-thread and shared channels. For a timeout,
  `case <-time.after(d):` runs if no other case is ready within d; since there is no heap to make
  a channel per call, `time.after` is allowed only there, without a received value (`time.now()`
  gives the time).
- `defer` is not allowed inside loops. Its arguments must live until the function returns. The
  builtins it takes are `close`, `panic`, `print` and `println` (defer a function that calls
  `delete`, `copy` or a vector method), and a variadic call needs its arguments as a slice
  (`defer f(xs...)`).
- A panic ends the program at once: deferred calls don't run while panicking (there is no
  `recover`, and the operating system closes the program's files and sockets).
- `start f(args)`: `f` must be a task. Arguments must not reference stack or arena memory.
- Channel sends: the value must not reference stack or arena memory.
- `with deadline d { ... }` (`d` a `time.Duration`): blocking I/O in the block, and in everything
  it calls, returns `net.Timeout` once `d` has passed: reads, writes, `accept`, `dial` and name
  lookups, and so the http client and anything built on `net`. Input already waiting is still
  read, and an expired deadline (`d <= 0`) fails only the calls that would wait. Blocks nest, and
  the sooner deadline wins: an inner block can shorten the outer deadline but not extend it.
  Leaving the block in any way (its end, `return`, `break`, `continue`) restores the outer one.
  The deadline belongs to the goroutine, so a task started inside the block doesn't inherit it.
  It doesn't affect `time.sleep`, channel operations (use `select` with `time.after`, and
  `runtime.deadline_left()` for the time left) or `blocking` C calls, which can't be interrupted.
- There is no `goto` and no `fallthrough`.
- Unused variables and unused imports are not errors.

## 7. The memory rules (what the checker enforces)

Every reference (pointer, slice, string, map/chan/arena reference, or a struct/array containing one)
must not outlive the memory it points to:

- **Return:** a function may only return references to static memory or to memory reachable from
  its parameters.
- **Store:** storing a reference into a variable, field, element or map requires the referenced
  memory to outlive the storage. Package variables accept only static memory.
- **Calls:** the compiler knows each callee's flows ("argument 2 is stored inside argument 1", "the
  result points into argument 1") and checks them against your arguments.
- **Arenas:** memory from `with arena x` lives until the end of that block. A package arena is
  static.
- **Goroutines and channels:** only static memory crosses goroutines.

### Error messages and fixes

| error contains | fix |
|---|---|
| `cannot return reference to variable buf` | take `buf u8[]` as a parameter and return a view of it, or allocate in a caller-provided arena |
| `cannot return reference to arena x` | the arena dies with its block; take `a arena` as a parameter instead |
| `cannot escape into static memory (assigned to g)` | package variables can only point to static data: make the referenced data package-level too, or copy it into a package arena (`str(b) in pkg_arena`) |
| `does not live long enough to be stored in variable v` | declare the referenced variable in an outer block (it must live at least as long as v) |
| `f stores argument 2 inside argument 1` | the second argument must live at least as long as the first |
| `make needs an arena` | declare storage (`var buf T[N]`, `var v T[..N]`, `var m V[K; N]`), or `make(...) in a` |
| `new(T) needs memory` | `var x T` and use `&x`, or `new(T) in a` |
| `string concatenation needs memory` | `a + b in r.arena`, or append into a buffer |
| `converting s to u8[] copies it` | `copy(buf[:], s)`, or `u8[](s) in a` |
| `recursion is not allowed` | loop with an explicit stack (`var stack T[..64]`) |
| `start needs a task` | declare the function `task f(...) pool N` |
| `goroutine … can outlive this function` | pass only static data, or values without references (ints, structs of ints) |
| `cannot copy int[str; 8] storage` | pass a view: `fn f(m int[str])` and call `f(store)` |
| `cannot copy x: T contains … storage` | pass or keep a pointer (`*T`); loop over the indexes and use `&xs[i]` |
| `cannot copy the elements of xs` | keep a slice of pointers (`*T[]`), or of views for map/chan elements |
| `cannot call m on a copy of x` | give m a `&self` receiver: `fn m(&self)` |
| `methods go in an impl block` | `impl T { fn m(&self, ...) { ... } }`, after the type |
| `a method takes self … or &self … first` | `fn m(self, ...)` or `fn m(&self, ...)`; a function without a receiver goes outside the impl |
| `named result r holds … storage` | return an unnamed `T{...}`, or take a `*T` parameter and fill it |
| `append exceeds capacity` (runtime panic) | larger storage, or `append(...) in a` |
| `mismatched types f32x4 and f32` | broadcast the scalar: `v * f32x4(k)` |
| `integer vectors have no division` | divide lane by lane in a loop, or multiply by a reciprocal in a float vector |
| `non-boolean condition` on `a == b` with vectors | comparisons give masks: `(a == b).all()` or `.any()` |
| `map capacity exceeded` (runtime panic) | a larger capacity: `V[K; N]` |
| `memory budget exceeded` | lower pool sizes (`pool pkg.task N`), fewer `workers`, or raise the budget |
| `workers auto can't be checked against the budget` | `workers N` with the number of cores you deploy on |
| `threads auto can't be checked against the budget` | `threads N` with the number of cores you deploy on |
| `cannot be used as constant data` | use a string constant, or copy the string into a package arena (`str(b) in pkg_arena`) |
| `shared x: T can't be shared` | share numbers, strings, bools, errors or arrays/structs of them; keep everything else per thread |
| `a shared channel's elements can't hold references` | send an index, ID or fixed-size array instead of a string or slice |
| `time.after can only be used as a select case` | `select { case <-time.after(d): ... }`; to just wait, `time.sleep(d)` |
| `anytype is only allowed as the type of a function's parameter` | make the function take it as a parameter; results, variables and fields have concrete types |
| `f has anytype parameters: it can only be called` | wrap the call in a named function with concrete parameter types, and pass that |
| `... (in pkg.f with x T, called at file:line)` | the error is in f's body for that argument type: pass a type that supports what f does with it |
| `m is a map with no storage` | declare it `thread m V[K; N]`, or assign it storage (`m = make(V[K], n) in a`) |
| `c is a channel with no storage` | declare it `thread c T chan[N]` |
| `only tasks can be preemptible` | mark the task the work runs in; functions it calls yield too |
| `only extern functions can be blocking` | drop `blocking`; squark code waits by parking already (channels, I/O, sleeps) |
| `cannot take address of …` / `cannot slice …` / `cannot assign to …` (a constant) | constants (faults, constant tables) are read-only and shared by every thread: copy into a variable first, `t := table; p := &t[0]` |
| `defer inside a loop` | move the loop body into a function and defer there |
| `constant x needs a value` | write every constant's value; for numbered names, declare an `enum` |
| `undefined: iota` | an `enum` numbers its members: `enum Kind { a, b, c }` |
| `Kind has no member x` | the members are listed in the enum's declaration (`squark doc` shows them) |
| `can't tell which enum .x belongs to` | write `Kind.x`, or give the variable a type (`var k Kind = .x`) |
| `pkg.x is not pub` / `it isn't pub` | declare it `pub` in its package |
| `write and, not &&` (and `or`, `not x`) | the logical operators are words |
| `results follow ->` | `fn f() -> int`, `fn f() -> (int, bool)`, `fn f() -> T !` |
| `for in a condition is while` | `while cond { }`; `loop { }` for an endless loop |
| `try is gone` | a `!` after the call: `f()!` |
| `a package variable is thread … or shared` | `thread x T` (per thread) or `shared x = ...` (read-only, one copy) |
| `the brackets go after the element type` | `T[]`, `T[N]`, `V[K]` |
| `a struct type is declared struct T { ... }` | `struct T { ... }` |
| `a named type is declared T: U` | `Celsius: f64`, not `type Celsius f64` |

## 8. Idioms

```squark
// formatting: append into a buffer
var buf u8[64]
line := strconv.append_int(append(buf[:0], "id="...), id)
line = append(append(line, " name="...), name...)
os.stdout.write(line)
// or, in an arena: s := "name=" + name in a

// reading a file: into caller storage
var data u8[1M]
f := os.open(path)!
n, _ := f.read_full(data[:])

// command-line flags: the values are kept in the flag package's fixed storage
port := flag.int_flag("port", 8080, "port to listen on")   // names and usage: constant strings
idle := flag.duration_flag("idle", 30*time.second, "idle timeout")
flag.parse()                     // -h prints the usage; a bad flag prints Go's message and exits 2
serve(*port, *idle, flag.args()) // flag.arg(i) and flag.n_arg() for the positional arguments

// a function returning a new slice: take the output buffer
fn squares(n int, out int[]) -> int[] in out {
    out = out[:0]
    for i in 0..n {
        out = append(out, i*i)
    }
    return out
}

// recursive tree walk: explicit stack
var stack (*Node)[..64]
stack.push(root)
while len(stack) > 0 {
    n := stack.pop()
    for c in n.children {
        stack.push(c)
    }
}

// building a tree: allocate in an arena
with arena tree 256K {
    root := new(Node) in tree
    root.children = append(root.children, new(Node) in tree) in tree
}

// interface-like dispatch: switch on a kind, or a func-typed field holding a named function
struct Shape { kind int; w, h f64 }
fn area(s Shape) -> f64 {
    switch s.kind {
    case circle:
        return math.pi * s.w * s.w
    }
    return s.w * s.h
}

// errors with context: use codes and constant messages
fault NoUser 404 "user not found"
if err != nil {
    println("lookup failed for", name, ":", err.message())
    return NoUser
}

// HTTP server: Go 1.22 patterns ("METHOD /path/{param}"), 404 and 405 answered for you
fn get_note(w *http.ResponseWriter, r *http.Request) {
    id, ok := r.path_int("id")       // r.path_value("id") for the string; r.query_param("q") decoded
    if not ok {
        http.send_error(w, 404)      // "404 Not Found"; w.text(code, s) for your own text
        return
    }
    j := w.json()                    // streams into the response: commas, nesting and escapes handled
    j.object()
    j.key("id")
    j.int(id)
    j.end()
}
fn main() {
    http.handle_func("GET /notes/{id}", get_note)
    http.listen_and_serve(http.port(8080), nil)   // nil: the handle_func patterns
}

// JSON request body: strings decoded into the request's arena, no buffers to size (text_max: a
// string over 200 bytes sets json.TooLong in d.err; text has no limit)
d := json.new_decoder_in(r.body, r.arena)
for k, ok := d.key(); ok; k, ok = d.key() {
    if k == "title" { title, _ = d.text_max(200) } else { d.skip() }   // lives until the response is done
}
// json.new_encoder(buf) / new_encoder_in(nil, a) build JSON outside a handler the same way

// HTTP client: responses are read into the caller's buffer, and the connection is kept alive
var buf u8[64K]
c := http.dial("127.0.0.1:8080", buf[:])!
resp := c.get("/notes/1")!       // resp.body and resp.header_value(...) view buf until the next request
```

## 9. Runtime behaviour

- One OS thread by default. Goroutines are cooperative and switch at blocking operations (and in
  preemptible tasks' loops). Netpoll is kqueue on macOS and epoll on Linux. One goroutine may
  read a connection while another writes it. Closing a connection, file or listener wakes the
  goroutines of the same thread blocked on it, whose `read`, `write` or `accept` returns an error
  (`bad file descriptor`); a goroutine of another thread blocked on it isn't woken, so close a
  descriptor on the thread that uses it.
- Addresses are `host:port`, `[ipv6]:port` or `:port`. `net.listen(":port")` takes IPv6 and IPv4
  connections on one socket (IPv4 alone where the system has no IPv6). `net.dial` looks names up
  itself, without the C library (whose resolver allocates and blocks its thread): the hosts file,
  then DNS over UDP (TCP for a long reply) on the poller, with resolv.conf's name servers, search
  domains, `ndots`, `timeout` and `attempts`, so a Kubernetes service name resolves as it does for
  C programs. It then tries each address, IPv4 first, until one connects.
- A watchdog thread asks a goroutine that has run for a time slice to yield (only preemptible
  tasks do), and prints a warning naming the task when a goroutine has held its thread for 100ms
  while other goroutines waited on it. Both count the CPU time the thread actually got, so an
  overloaded machine that doesn't run the thread doesn't make its goroutine look slow. A program
  with nothing else to run isn't warned about. `runtime.gosched()` yields by hand.
- `threads N` (1 to 1024, or `auto` for one per CPU, below) runs N scheduler threads (shards) in
  one process. The directive says the program is written for threads; without it a program runs one
  thread. `threads auto` is the usual choice: one thread per CPU, and `SQUARK_THREADS=N` in the
  environment overrides the count at run time (like `GOMAXPROCS`). `SQUARK_THREADS` also
  overrides `threads N`, except that with a `budget` it can't go above N, the count the budget
  was proven for. It has no effect on programs without the directive. Each shard runs `main`
  with its own `thread` variables (§4), goroutine pools, timers and poller; goroutines never move
  between shards, so rule 10 holds within each and nothing needs locks. Faults are shared (they're
  read-only). Listening on an address from every shard works: on Linux each shard gets its own
  socket (`SO_REUSEPORT`) and the kernel spreads connections; on macOS the shards share one
  socket. Port 0 is never shared: each shard listening on `":0"` gets its own port (`l.port()`),
  so a program that tests itself listens on one shard. A `net.Conn` is a descriptor and can be
  sent to another shard on a shared channel; use it on one shard at a time, and close it on the
  shard that uses it (see above). Each shard also has about 350 KiB of runtime data (output
  buffer, poller and idle-connection tables), counted in static data. `runtime.shard()` and
  `runtime.shards()` report where a goroutine runs. The process exits when every shard's `main`
  has returned (a shard whose `main` returns stops its goroutines), at the first panic, or at
  `os.exit`. The memory budget counts static data and stacks once per shard (and `threads auto`
  can't be combined with `budget`). `threads` and `workers` combine: workers × threads shards in
  all. Shards share only constants, `shared` variables and shared channels. A shard whose
  goroutines all wait on shared channels sleeps until another shard wakes them, so a deadlock
  across shards hangs instead of reporting `all goroutines are asleep` (with one thread, a
  goroutine blocked on a shared channel is reported like any other). Externs are called from
  whichever shard runs the goroutine (or a helper thread, for `blocking` externs), so C libraries
  must be thread-safe (libc and SQLite are; each shard opens its own SQLite connection:
  `thread db sqlite.DB`).
- **CPUs for `auto`:** `threads auto` and `workers auto` count the CPUs the process may use. On
  Linux that is the smaller of the CPUs it may run on (its affinity: `taskset`, cpusets) and its
  cgroup's CPU limit rounded up (a container's `--cpus 1.5` or a Kubernetes pod's `limits.cpu:
  1500m` gives 2), much as Go 1.25 sets `GOMAXPROCS`; elsewhere it is the CPUs online.
- `workers N` (1 to 1024, or `auto` for one per CPU) starts N processes before any squark code
  runs; each has its own package variables, goroutines and connections. Listening sockets use
  `SO_REUSEPORT`, so the kernel spreads connections across the workers (on Linux; macOS sends them
  all to one). The starting process supervises: SIGINT, SIGTERM and SIGHUP are passed on to every
  worker, and if one exits with a non-zero status the others are stopped and that status is the
  program's. On Linux workers also stop if the supervisor is killed. The memory budget counts code
  once and everything else once per worker; `workers auto` can't be combined with `budget`. SQLite
  connections wait up to 5 seconds for another worker's lock.
- Each task pool has N stacks of a size proven at compile time, each with a guard page. A C function
  that exceeds its `stack` budget hits the guard: `fatal error: stack overflow in goroutine [pool]`.
- **Loops and SIMD.** clang vectorizes loops, so a loop over slices often runs as SIMD code. A loop
  gets an unchecked copy, which clang can vectorize, when it counts by one (`for i in lo..hi`,
  `for i, x in s`, `for i := a; i < b; i++` with `<=` too, or `for i := a; i >= b; i--` with
  `>` too). Its body must be at most 40 statements, with no loop, `switch`, `select`, `defer`,
  `start`, `!`, channel operation or allocation (`make`, `new`, `append`, `in`). The copy drops
  the bounds checks of indexes of the form `x[i]`, `x[i+k]` and `x[i-k]`, or with `c*i` in place
  of `i` for a positive constant `c` (`x[2*i+1]`), where `x` is an array, a pointer to an array,
  or a slice or string that the loop can't change, and `k` is an `int` expression of locals and
  constants that the loop doesn't change. A slice in a local variable qualifies if the loop
  doesn't assign it and its address isn't taken. A slice in a field of a local (`p.data` in a
  method on `*T`, `s.data`) or in a package variable qualifies if the loop calls no functions
  (builtins are fine) and stores only into local variables or values without references, so
  nothing in it can change the slice. A test before the loop runs the unchecked copy only when
  every index the counter can reach is in range (and `p` isn't nil), so a loop that would go out
  of range still panics at the same iteration. Floating-point sums stay in source order, so they
  don't vectorize: write them with SIMD types (§3).
- Panics print `panic: <message>` with the goroutine and source position, then exit with status 2.
  There is no `recover`. A nil dereference becomes `panic: runtime error: invalid memory address or
  nil pointer dereference`.
- **Graceful stop.** SIGTERM and SIGINT end a program at once, unless it has called
  `runtime.stop_gracefully()`, as `http.serve` does. Then the first one starts a stop:
  `runtime.stopping()` becomes true, `accept` returns `net.Stopping`, and the program has a grace
  period (`SQUARK_SHUTDOWN`, such as `20s` or `500ms`; 10s by default) to finish and return from
  main, after which it exits with status 1. A second signal ends it at once. During the stop,
  every goroutine's blocking I/O (blocked already or later) times out with `net.Timeout` a
  tenth of the grace period before its end, as if under `with deadline`, so a handler stuck on a
  slow upstream can still answer. An HTTP server stops
  accepting, finishes the requests in progress (answering `Connection: close`), closes its idle
  keep-alive connections and returns nil from `serve`, so a container's stop (Kubernetes,
  `docker stop`, systemd) drops no request. With `workers N` the supervisor passes the signal on
  and each worker stops this way.
- When every goroutine is blocked with nothing to wake it: `fatal error: all goroutines are asleep -
  deadlock!`.
- `SQ_STACKCHECK=1` reports, per pool, the proven and the deepest observed stack use at exit.
- C libraries (e.g. SQLite) can use malloc. squark code, the runtime and the shims never do, and the
  build verifies this.

## 10. Toolchain

```sh
squark build [-o out] [-v] [-work dir] [-cpu c] [dir]   # -v prints the memory breakdown (always printed with a budget)
squark run [-v] [-cpu c] dir [args...]
squark check [dir]                              # type and lifetime check only
squark doc [dir] [pkg]                          # API with provenance; `squark doc std` for the stdlib
squark emit-c [dir]                             # the generated C
```

`-cpu` (or `SQUARK_CPU` in the environment) picks the CPU to build for: `baseline` (the default:
every CPU of the architecture, which on x86-64 means SSE2 only), `native` (the building machine's
CPU; the binary may not run elsewhere), `x86-64-v2`, `x86-64-v3` (AVX2 and FMA: SIMD types and
vectorized loops use 256-bit registers) or `x86-64-v4` (AVX-512) on x86-64, and `armv8.1-a` to
`armv9.5-a` on arm64 (which always has 128-bit NEON). On x86-64 a program built for more than the
baseline checks the CPU when it starts and exits with status 2, naming the missing feature,
instead of crashing on an illegal instruction.

`squark run` passes SIGINT, SIGTERM, SIGHUP, SIGQUIT, SIGALRM, SIGUSR1 and SIGUSR2 on to the program
and exits with its status (128+n if a signal ended it); one that arrives during the build removes
the temporary files and exits with 128+n.

## 11. A complete program

```squark
// count is an HTTP server with SQLite: it reports how many rows a table holds.
//
//    squark run examples/count
//    curl localhost:8080/count
//
// It shows:
//   - a SQLite statement prepared once and reused by every request
//   - !, where an error only propagates
program {
    budget 64M
    pool http.serve_conn 128
}

use http, sqlite, strconv

thread db sqlite.DB
thread count_stmt sqlite.Stmt

// GET /count: how many items the database holds
fn count(w *http.ResponseWriter, r *http.Request) {
    defer count_stmt.reset()
    row, err := count_stmt.step()
    if err != nil or not row {
        http.send_error(w, 500)
        return
    }
    var buf u8[24]
    w.text(200, str(strconv.append_int(buf[:0], count_stmt.column_int(0))))
}

fn setup() -> ! {
    db = sqlite.open("app.db")!
    db.exec("CREATE TABLE IF NOT EXISTS items(id INTEGER PRIMARY KEY)")!
    count_stmt = db.prepare("SELECT count(*) FROM items")!
    return nil
}

fn main() {
    if err := setup(); err != nil {
        println("setup:", err.message())
        return
    }
    http.handle_func("GET /count", count)
    if err := http.listen_and_serve(":8080", nil); err != nil {
        panic(err)
    }
}
```
