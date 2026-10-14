# Bug tracker

Findings from a whole-project review (2026-10-14). Every item was reproduced against the working
tree at commit `511d59e` + uncommitted changes. Work top to bottom; tick the box and add a one-line
note (what changed, which test covers it) when an item is done.

## How to work an item

1. Turn the repro into a regression test first and watch it fail:
   - wrong runtime behaviour → `testdata/run/<name>.sq` + `<name>.out` (prefix `panic_` if it must
     exit 2);
   - missing/wrong diagnostic → `testdata/errors/<name>.sq` with `// ERROR "substring"` on the line;
   - driver/CLI/harness → a Go test in `internal/driver/driver_test.go`;
   - stdlib networking (http/net) → a run test is hard; a Go test that starts the binary is fine.
2. Fix, then `go build -o bin/squark ./cmd/squark && go test ./internal/...`.
3. Tick the box, write `— fixed: <what>; test: <file>` after the title.
4. If a fix changes documented behaviour, update `docs/spec.md` (and `docs/gen.sh` for stdlib docs).

**Kinds:** `bug` = crash, memory unsafety, C compiler failure, compiler panic, silent wrong value,
or contradicts squark's own spec. `go-diff` = only differs from Go. squark is Go-*inspired*, and
deviations are fine. But `docs/spec.md:4` says "anything not mentioned here behaves as in Go", so
each `go-diff` is resolved either by fixing it or by adding one line to the spec. The recommended
choice is given for each.

---

## P0: memory safety, crashes, silent miscompiles

- [x] **L1. Map keys inserted by `m[k]++` / `m[k] op= v` escape unchecked** (bug) — fixed: `lifetime.mapStore` shared by `store`, IncDec and op-assign; test: `testdata/errors/escape_mapkey.sq`
  `internal/compiler/lifetime.go:1196` (IncDecStmt), `:1305` (op-assign). Only plain `=` goes
  through `store()`, which records "key outlives the map". `squark doc` shows `w string into m` for
  `m[w] = 1` but nothing for `m[w]++`. The spec's §3 example and `testdata/run/capacity.sq:17` use
  this pattern.
  ```go
  var seen map[string]int cap 16
  func count(m map[string]int, w string) { m[w]++ }
  func add(word string) {
  	var buf [8]byte
  	n := copy(buf[:], word)
  	count(seen, string(buf[:n]))
  	seen[string(buf[:n])] += 10
  }
  func clobber() { var junk [64]byte; for i := range 64 { junk[i] = 'X' }; println(string(junk[:4])) }
  func main() { add("hello"); add("world"); clobber(); for k, v := range seen { println(k, v) } }
  ```
  `check` passes; keys print as NUL bytes. Expected: `cannot escape into static memory`.
  Fix: factor a `mapKeyStore(ix)` out of `store()` and call it from IncDec and op-assign.

- [x] **L2. Map/chan storage nested in a struct or array can be copied** (bug) — fixed: `storageIn`/`checkNestedStorageCopy` in assignable, `:=`, `var`, globals, range values; fresh values (`T{}`, calls) still allowed; spec §3 + error table updated; tests: `testdata/errors/storage_nested.sq`, `storage_nested_global.sq`
  `internal/compiler/expr.go:175` `isStorageCopy` checks only the top-level type. Also
  `declStmt` (`stmt.go`) and `globalVarSpec` (`checker.go`) skip the check for `var x = m` (no declared type).
  Copies alias the original's slots; a returned copy points into a dead frame.
  ```go
  type Reg struct{ m map[int]int cap 4 }
  //squark:noinline
  func mk(k int) Reg { var a Reg; a.m[k] = 111; b := a; return b }
  //squark:noinline
  func clobber() { var junk [64]int; for i := range 64 { junk[i] = -1 }; println(junk[7]) }
  func main() { r := mk(1); clobber(); println(r.m[1]) }   // prints 0, want compile error
  ```
  Also: `b := a` on `[2]map[int]int cap 4` then `b[0][1] = 99` changes `a`; `var m2 = m` is accepted.
  Fix: make `isStorageCopy` recurse through Struct, Array and VecType like `hasRefs`; call it in
  `declStmt`/`globalVarSpec` when the type is inferred.

- [x] **C6. Local variable C name can shadow a function, so the wrong function is called** (bug) — fixed: local C names never contain `__` (`newVarName`); funcs/methods/globals reserved via `gen.uniqueCName` with externs pre-seeded; test: `testdata/run/cnames.sq`
  `checker.go:872` (locals → `name_N`), `cgen.go:86-88,116` (`pkg__F`, `pkg__T__M`, `G_pkg__v`).
  ```go
  func k_1() int { return 7 }
  func other() int { return 8 }
  func main() { main__k := other; println(k_1(), main__k()) }   // prints 8 8, want 7 8
  ```
  Also `func (T) M()` + `func T__M(T)` → C "redefinition of main__T__M".
  Fix: separate namespace for locals (e.g. `l<N>_name`) and an injective scheme for globals/methods
  (length-prefixed parts), or detect collisions and add a suffix.

- [x] **T1. Compiler panics (Go panic) on constant float ÷ 0 and non-int constant string index** (bug) — fixed: div-by-zero guard for any numeric constant divisor; string-index fold only for integer indexes; no cascading "is not constant"; tests: `testdata/errors/const_fold.sq`, `const_fold_global.sq`
  `expr.go:1150` (div-by-zero guard is integer-only), `expr.go:850` (`constant.Int64Val` on a
  non-int).
  Repros: `const f = 1.0 / 0`; `var x float64 = 2.0 / 0.0`; `x := "abc"[1.0]`; `x := "abc"["k"]`.
  Fix: guard on `constant.Sign(y.Const) == 0` for any constant division; fold the string index only
  when the index is an integer constant (use `constant.ToInt` + ok).

- [x] **T3. `var a, b T = f()` never checks f's result types** (bug) — fixed: per-name `assignableType` check in `declStmt` tuple form; test: `testdata/errors/var_tuple.sq`
  `stmt.go:656-670` declStmt tuple branch.
  ```go
  func f() (int, int) { return 4096, 8192 }
  func main() { var p, q *int = f(); println(*p + *q) }   // compiles, nil-deref at run time
  ```
  Fix: when `typ != nil`, check `assignableType(ts[i], typ)` for each name.

- [x] **I1. Imports resolve by directory-name suffix and by a cache key that can collide** (bug) — fixed: loader records `ImportSpec.Pkg`; cache keyed `local:<abs dir>` / `std/<path>`; local `Package.Path` is the abs dir (unique type keys); duplicate import names → `X redeclared in this block`; test: `TestImports` in `internal/driver/driver_test.go`
  `checker.go:112` (`strings.HasSuffix(d.Dir, TrimPrefix(imp.Path, "./"))`), `checker.go:125`
  (duplicate import names silently overwrite), `package.go:128,223,226` (local packages cached
  under `filepath.Clean(dir/imp)`, stdlib under its bare path).
  - `import ("./mylib"; lib "./lib")` → `lib` bound to mylib (prints `mylib mylib`).
  - `import "./lib"` alongside `./mylib` → `undefined: lib`.
  - `import "../util"` → always `unresolved import`.
  - Main dir `.` importing `"./io"` → std `os` gets the user's `io` (`undefined: io.EOF`); works
    when run from the parent directory. `./main` collides with `main` the same way.
  - std `strings` + a local package named `strings` → later one wins silently.
  Fix: loader records the resolved `*Package` on each ImportSpec; checker uses it. Key local
  packages by absolute path, stdlib by `"std/"+path`. Error `X redeclared` on duplicate names.

- [x] **R1. `std/http`: any response body > 16384 bytes panics and kills the server** (bug) — fixed: a chunk's closing CRLF leads the next write (`ResponseWriter.crlf`); `finish` flushes first when the terminator doesn't fit; test: `TestHTTP` (`internal/driver/driver_test.go`, server in `testdata/http/`)
  `std/http/http.sq:198` (`flushChunk` appends `"\r\n"` to a full `w.buf`), `:229` (`finish()` does
  the same with the chunk terminator).
  Repro: handler `w.Write(big[:20000])` with `var big [20000]byte`; `curl` it →
  `panic: append exceeds capacity (need 16386, capacity 16384)`.
  Fix: don't append the trailer to `w.buf`; send it as a prefix of the next chunk header or the tail.

- [x] **R2. `std/http`: huge Content-Length overflows the size check and panics** (bug) — fixed: compare `ContentLength > len(rbuf)-end`; test: `TestHTTP`
  `std/http/http.sq:348` `total := end + req.ContentLength`.
  Repro: `printf 'POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 9223372036854775807\r\n\r\n' | nc 127.0.0.1 PORT`
  → `slice bounds out of range`, server dead. Expected: 413.
  Fix: `if req.ContentLength > len(rbuf)-end { 413 }` before adding. (Also see R6.)

- [x] **R3. Deleting from a map while ranging over it skips entries** (bug) — fixed: tombstone deletes (trailing tombstones cleared), in-place `map_compact` only on inserting a new key when tombstones > slots/8; insert-during-range caveat added to spec §6; test: `testdata/run/map_churn.sq` (also clean under ASan/UBSan)
  `runtime/sq_rt.c:777` `sq_map_delete` (backward-shift deletion), `:803` `sq_map_next`.
  ```go
  var m map[int]int cap 1000
  func main() {
  	for i := range 1000 { m[i] = i }
  	visited := 0
  	for k := range m { visited++; if k%2 == 0 { delete(m, k) } }
  	println("visited", visited, "len", len(m))   // 873 564, want 1000 500
  	for k := range m { delete(m, k) }
  	println(len(m))                              // 58, want 0
  }
  ```
  Fix: tombstones (a ctrl value lookups skip), or tombstones only while an iteration is live.

- [x] **C3. `append` writes elements before evaluating later arguments** (bug) — fixed: elements go to temporaries before the capacity check and stores (both paths); test: `testdata/run/append_order.sq`
  `cgen_expr.go:796-801` (and the `in arena` path). Emits `s.p[s.len++] = arg` per argument, and the
  capacity panic fires before any argument is evaluated.
  `s := []int{1, 2}; s = append(s[:0], s[1], s[0]); println(s[0], s[1])` → `2 2`, want `2 1`.
  Fix: evaluate all args into temporaries, then check capacity, then store.

- [x] **C5. `Push`/`TryPush` check capacity before evaluating the argument; `Push` is unsequenced C** (bug) — fixed: receiver, then argument into a temporary, then check/store; test: `testdata/run/append_order.sq`
  `cgen_expr.go:831-834`. `p->a[p->len++] = <expr that changes len>` is undefined behaviour in C.
  Full `var w [..2]int`: `w.Push(w.Pop())` panics `vector is full`; `ok := v.TryPush(v.Pop())` →
  `false` and the Pop never runs.
  Fix: `({ E v = arg; T *p = &vec; if (p->len >= N) panic; p->a[p->len++] = v; })`.

## P1: wrong results, C compiler failures, broken stdlib

- [x] **L3. `vec.Clear` receiver expression is never lifetime-checked** (bug) — fixed: receiver analysed via `vecOf1`; the defer half is moot (only close/print/println/panic can be deferred); test: `testdata/errors/escape_vecclear.sq`
  `lifetime.go:1123` (`case "vec.Clear": return nil`); `deferStmt` also skips builtin-method receivers.
  ```go
  var g *int
  var gv [..4]int
  func keep(p *int) *[..4]int { g = p; return &gv }
  //squark:noinline
  func f() { x := 1234; (*keep(&x)).Clear() }
  func main() { f(); clobber(); println(*g) }   // dangling read; want "keep keeps argument 1 in static memory"
  ```
  Fix: `l.vecOf1(se.X)` before returning; same for builtin-method receivers in `deferStmt`.

- [x] **C10/T9. `len`/`cap` of an array-typed call is constant-folded, so the call never runs** (bug) — fixed: fold only when `!hasCall(x)` (arrays, *array, vector cap); cgen evaluates x then yields N; test: `testdata/run/len_calls.sq`
  `call.go:380` (and vector `cap` fold ~`:371`), `cgen_expr.go:727-730,740-741`.
  ```go
  var n int
  func f() [3]int { n++; return [3]int{} }
  func main() { x := len(f()); println(x, n) }   // 3 0, want 3 1
  ```
  Fix: fold only when the operand contains no call/receive; otherwise emit `({ (void)(x); N; })`.

- [x] **C11. `defer panic("msg")` prints `panic: deferred panic`** (bug) — fixed: shared `panicC` used by direct and deferred panic; test: `testdata/run/panic_defer.sq`
  `cgen_stmt.go:399`. Fix: dispatch on the stored arg type as `builtinCall`'s `panic` case does.

- [x] **T6. Recursive types through vector/map/chan reach the C compiler; `type A B; type B A` accepted** (bug) — fixed: `checkValueCycle` covers VecType/MapStore/ChanStore; `resolveNamed` reports a named-to-named cycle; test: `testdata/errors/type_cycle.sq`
  `checker.go:525` `checkValueCycle` lacks `*VecType`, `*MapStore`, `*ChanStore`; `checker.go:516`
  `resolveNamed`.
  `type T struct{ x int; v [..2]T }` (likewise `map[int]T cap 4`, `chan T cap 4`) → C "incomplete
  element type". `type A B; type B A` → compiles as `int`.
  Fix: add the three cases; in `resolveNamed`, `state == 1` → `invalid recursive type`.

- [x] **T7. Negative constant index in an array literal reaches the C compiler** (bug) — fixed: error in `indexedElts`, `[...]` length pass skips it the same way; test: `testdata/errors/lit_index.sq`
  `expr.go:584` `indexedElts` checks only `idx >= length` (also the `[...]T` length path).
  `a := [4]int{-1: 5}` → C "array designator value '-1' is negative". Fix: `idx < 0` error.

- [x] **T11. Empty struct / zero-length array zero values fail in C** (bug) — fixed: zero values and empty literals emit `((T){})`; test: `testdata/run/empty_types.sq`
  cgen emits `(T){0}`. `type E struct{}; var e E` and `var a [0]int` → C "initializer for
  aggregate with no elements requires explicit braces". Fix: emit `{}` for empty aggregates.

- [x] **T8. Float constants are not range-checked: `1e400` silently becomes 0** (bug) — fixed: `representable` rounds to float32/float64 and rejects ±Inf; a failed untyped default no longer cascades; tests: `testdata/errors/float_const.sq`, `testdata/run/float_const.sq`
  `checker.go:1004` `representable` float case always ok.
  `x := 1e400; println(x > 1)` → `false`; `var f float32 = 1e300` → `+Inf`.
  Also float32 constants aren't rounded (`const f float32 = 16777217; f == 16777216` → false).
  Fix: `constant.Float32Val/Float64Val`, error on Inf, store the rounded value.

- [x] **P1. Invalid escape sequences are accepted and produce garbage bytes** (bug: spec §1 says literals are as in Go) — fixed: `escDigits` checks each digit and leaves a non-digit unread; octal > 255 and invalid code points rejected; test: `testdata/errors/literals.sq`, `testdata/run/literals.sq`
  `lexer.go:354-385` `escape`; `hexval` maps non-hex to 0, octal > 255 wraps, short escapes
  swallow following chars including the closing quote.
  `"\xZZ|\400|\uZZZZ|\08|\U00110000"` compiles (`\08|` eats the `|`); `'\xg1'` → 1;
  `"a\0"` → "unterminated string literal".
  Fix: base-aware digit check that stops without consuming; error on octal > 255, surrogates,
  > 0x10FFFF.

- [x] **P2. Malformed rune literals are accepted** (bug) — fixed: empty/unterminated/multi-character rune literals reported, no cascade after a bad escape; test: `testdata/errors/literals.sq`
  `lexer.go:431-438` `char`. `'''` → 39; quote, newline, quote → 10.
  Fix: after the opening quote, error on `'`, `\n`, EOF.

- [x] **P3. A type named `stack`/`pool`/`from`/`static` can't be a result type** (bug: spec §1 says these are identifiers elsewhere) — fixed: parser gained one-token `peek`; `pool`/`stack` are a clause only before a size expression, `from` only before an identifier. Left as is: a bare `static` right after the parameters is still read as the (misplaced) clause; write `(static)` for a type of that name; test: `testdata/run/clause_names.sq`
  `parser.go:247,272` `atClause`.
  `type stack struct{ items [..8]int }; func newStack() stack { ... }` → `expected expression, found {`;
  `var f func() stack` → `expected ;, found identifier "stack"`.
  Fix: no clause check in func *types*; in decls treat `pool`/`stack` as a clause only when the
  next token can start a size expression.

- [x] **R4. `std/http`: a second `http.Serve` never accepts connections** (bug) — fixed: one shared `wakeIdle` task; idle connections carry a server tag through the runtime (`ParkIdle(tag)`, `NextIdle() (Conn, int)`; kqueue udata / epoll data word) so each resumes on its own handler; up to `http.MaxServers` (16), else `ErrTooManyServers`; stdlib.md regenerated; test: `TestHTTP` (two servers, connections parked and resumed). Linux/epoll verified in Docker (whole suite passes). The tag is packed into the existing queue entries (tag<<24 | fd), so no extra static memory
  `std/http/http.sq:271` — every `Serve` does `go wakeIdle(h)`; `wakeIdle` is `pool 1` and never
  exits, so the second `Serve` parks forever. (`sq_next_idle` would also panic with two waiters.)
  Repro: two tasks each calling `ListenAndServe` on different ports; the second times out.
  Fix: start `wakeIdle` once, guarded by a package flag.

- [x] **R5. Partial writes return a nil error** (bug) — fixed: `io.ErrShortWrite` when n < len in `os.File.WriteString` and `net.Conn.Write/WriteString/Write2`; test: `TestShortWrite` (os path; net uses the same three lines)
  `std/os/os.sq:96` (`File.WriteString`), `std/net/net.sq:117,125,134`; runtime `sq_rt.c:1119,1141`
  return partial counts on error. Only `os.File.Write` checks.
  Repro: write 4 MB with `WriteString` under `ulimit -f 100` (trap XFSZ) → `n=102400 err=nil`.
  Consequence: http `flushChunk`/`finish` miss truncated responses.
  Fix: return `io.ErrShortWrite` when `n < len(p)` in all four.

- [x] **R6. `strconv.ParseInt`/`Atoi` accept invalid input** (bug) — fixed: base must be 2..36 (`ErrBase`), no `_` (no base 0 in squark); stdlib.md regenerated; test: `testdata/run/strconv_parse.sq`
  `std/strconv/strconv.sq:102,107`. `Atoi("_")` → `0,nil`; `Atoi("1_000")` → `1000,nil`;
  `Atoi("-_")` → `0,nil`; `ParseInt("10", 37)` → `37,nil`. Affects http Content-Length, net
  ports, `json.Int`.
  Fix: drop `_` (or only with base 0 + prefix), require 2 ≤ base ≤ 36 and at least one digit.

- [x] **R7. `strconv.AppendFloat` is wrong for large values and high precision** (bug)
  `std/strconv/strconv.sq:45,49-61`.
  `AppendFloat(1e19, 2)` → `+Inf`; `(-1e300, 2)` and -Inf → `-+Inf`; `(1.5, 20)` → `2.000…`;
  `(1.5, 25)` → index-out-of-range panic.
  Fix: handle ±Inf before the sign; clamp `prec` or emit digits iteratively; large magnitudes need
  a real algorithm (Ryu or digit-by-digit). (Rounding half-up, e.g. `0.125`→`0.13`, is a go-diff;
  see G-list.)
 — fixed: exact conversion with fixed-size big integers (no libc printf, which may allocate), rounded half to even; added `math.Float64bits`/`Float64frombits`; test: `testdata/run/strconv_float.sq`, expected output generated by Go's `strconv.FormatFloat` (504 cases, identical)
- [x] **R8. `json.Decoder.String` drops the character after a lone high surrogate** (bug)
  `std/json/json.sq:220-228`. `"\uD800Ax"` → `EF BF BD 78` (the `A` is lost);
  `"\uD800𐀀"` → two U+FFFD instead of U+FFFD + U+10000.
  Fix: if `lo` isn't DC00–DFFF, emit RuneError and process `lo` as its own rune.
 — fixed: rewind when the second escape isn't a low surrogate; lone low surrogates also become U+FFFD; test: `testdata/run/json_strings.sq` (expected output from Go's encoding/json)
- [x] **R9. `json.Decoder.String` returns ok=true after a `\u` syntax error** (bug)
  `std/json/json.sq:220` + `hex4`. `"\u12G4"` → `ok=true`, `d.Err` set, value `"�4"`.
  Fix: `if d.Err != nil { return "", false }` after each `hex4()`.
 — fixed: return failure right after a failing `hex4`; test: `testdata/run/json_strings.sq`
- [x] **R11. `make(map, huge) in a` spins forever** (bug) — fixed: `cap > INT64_MAX/4` → arena panic before sizing; test: `testdata/run/panic_mapcap.sq`
  `runtime/sq_rt.c:741` — `slots <<= 1` overflows to 0, `while (slots < target)` never ends.
  Repro: `make(map[int]int, n) in a` with `n := 1 << 62` (non-constant) → 100% CPU.
  Fix: `if (cap > (INT64_MAX/2)/2) sq_panic_arena(...)` before the loop.

- [x] **R12. Reading stdin blocks every goroutine** (bug: contradicts spec rule 10 "switch at I/O")
  fds 0/1/2 and `OpenFile` files are never non-blocking. Repro: main loop `time.Sleep`-ticking
  while a task reads `os.Stdin` from an idle pipe — ticks stall until stdin has data.
  Fix: make those fds non-blocking and park on the poller like sockets (or document as a
  limitation for regular files, which can't be polled).
 — fixed: `sq_read` waits for readability through the poller first for blocking pipes and terminals (classified once per fd, reset on close); no O_NONBLOCK on shared descriptors; regular files read directly; test: `TestStdinDoesNotBlock` (also passes on Linux in Docker). Blocking *writes* to a full stdout pipe still stall everything (not changed)
- [x] **D5. Memory budget check passes silently when `size -m` fails** (bug)
  `internal/driver/driver.go:263-266` — code/static sizes fall back to 0. Always the case on Linux
  (GNU `size` has no `-m`, no `__TEXT`/`__DATA`).
  Repro: `budget 16M` + `var big [64<<20]byte` with a failing `size` on PATH → builds, report says
  "static data 0 B".
  Fix: error when a budget is declared and sizes can't be read; add an ELF path (`debug/elf`).
 — fixed: sizes read with `debug/macho` / `debug/elf` (PT_LOAD: writable = static) instead of `size -m`; unreadable sizes fail a build that declares a budget; test: `TestBudget` (ELF path verified on Linux in Docker)
- [x] **D8. Error-test harness accepts extra diagnostics on annotated lines** (bug, tests)
  `internal/driver/driver_test.go:70-74` — unexpected errors are checked per line, not per message.
  `testdata/errors/contracts.sq:23` has one annotation but three diagnostics; one, "stores a inside
  b" for `a.next = b`, looks like a **false positive** — investigate it as part of this item.
  Fix: per line, diagnostic count must equal annotation count, each annotation matching one.

## P2: lesser bugs
 — fixed: one diagnostic per annotation, no extras. The contracts.sq:23 extras were not a soundness false positive (the full contract checks clean) but follow-on noise suggesting wrong fixes; contract checking now assumes each suggested fix before checking further flows (stores before results); tests: strict `TestErrors`, `testdata/errors/contracts_multi.sq`
- [x] **L4. `for` post-statement temporaries die each iteration (latent C UB)** (bug)
  `cgen_stmt.go:610` emits `s.Post` inside the `for(;;)` body; lifetime.go analyses it at the
  init depth. `for p := &start; p.n < 3; p = &(T{n: p.n + 1}) { ... }` reads `_t5` after its
  lifetime ends. Works by accident under clang.
  Fix: analyse Post at body depth (reject), or hoist its temporaries into the enclosing block.
 — fixed: post statement generated first, its hoisted temporaries declared before `for (;;)` (matching the lifetime analysis); test: `testdata/run/for_post_temp.sq` (+ generated C checked)
- [x] **T10b. Typed unsigned constant shift overflow silently becomes 0** (bug)
  `expr.go:1268` masks SHL to width. `const u uint8 = 1; v := u << 8` → 0; want
  `constant 256 overflows uint8`. Fix: drop the masking; let `representable` report.
 — fixed: SHL masking removed, `representable` reports the overflow; test: `testdata/errors/shift_const.sq`
- [x] **T10c. `a, b := 1, 2` rejected when `a` is an existing `int8`** (bug)
  `stmt.go:257` defaults the RHS to `int` before noticing `a` exists → `cannot use 1 (type int) as
  type int8`. Fix: `defaultUntyped` only for names that are actually new.
 — fixed: only new names take the default type; tests: `testdata/run/redeclare_untyped.sq`, `testdata/errors/redeclare_untyped.sq`
- [x] **T12. Duplicate `case` constants and duplicate labels are accepted** (bug)
 — fixed: constant case values tracked per tagged switch; labels per function; test: `testdata/errors/duplicates.sq`
- [x] **T13. Methods on a named vector type can't be called** (bug)
  `type Stack [..8]int` + `func (s *Stack) Top() int` declares fine but `s.Top()` →
  `s.Top undefined (vectors have Push, ...)`. Either allow the call or reject the declaration.
 — fixed: only Push/TryPush/Pop/Clear take the builtin path; other names use normal method lookup; test: `testdata/run/vec_methods.sq`
- [x] **T14. Call statement with `in a` is rejected** (bug?)
  `show("hi " + name) in a` → `expr is not used`. Check the spec for whether `in` applies to a
  call statement; fix or document.
 — fixed: expression statements look through `in a` (spec §5 defines `x in a` for any x); `exprString` prints `x in a`; tests: `testdata/run/in_call_stmt.sq`, `testdata/errors/in_unused.sq`
- [x] **T15. A package-level `var strings` silently shadows `import "strings"`** (bug)
  Fix: `strings redeclared in this block`.
 — fixed: package names that clash with a file's import names are reported (end of `collect`); test: `testdata/errors/import_shadow.sq`
- [x] **P4. Invalid UTF-8 in an interpreted string is rewritten to U+FFFD** (bug: spec says literals as in Go) — fixed: `newLexer` rejects invalid UTF-8 anywhere in the file (strings, raw strings, comments), as Go does; test: `TestSourceEncoding`
  `lexer.go:426`. Source byte `0xff` inside `"..."` → 3 bytes; raw string keeps 1. Fix: error
  "illegal UTF-8 encoding" when the decoded rune is RuneError with size 1.

- [x] **P6. `1e` / `1_.5` give "constant unknown overflows float64"** (bug) — fixed: `exponent has no digits`, go/scanner's `invalidSep` rule, FLOAT literal Unknown guard; test: `testdata/errors/literals.sq`
  `lexer.go:318-326` accepts an exponent with no digits; `expr.go:86` FLOAT case doesn't check
  `constant.Unknown`. Fix both.

- [x] **P7. Hex float literals (`0x1p4`) don't lex** (bug: spec says number literals as in Go) — fixed: hex mantissa with `.` and required `p` exponent; test: `testdata/run/literals.sq`
  `lexer.go:294-305`. Fix: accept `.` and `p[+-]digits` in the hex branch. (Or document.)

- [x] **R10. `json.AppendString` passes invalid UTF-8 through, producing invalid JSON** (bug)
  `std/json/json.sq:22`. `"a\xffb"` → bytes `22 61 FF 62 22`. Fix: decode runes ≥ 0x80, emit
  `�` for bad bytes (Go also escapes U+2028/U+2029).
 — fixed: invalid bytes become U+FFFD (raw, as Go writes them), U+2028/2029 escaped; added `utf8.DecodeRuneInString`; test: `testdata/run/json_strings.sq`
- [x] **R13. `println(0.0)` prints `+0.e+000`** (bug)
  `runtime/sq_rt.c:350-358` — `fmt_uint` emits 1 digit for 0, inconsistent with the 8-digit format.
 — fixed: digits left-padded to nine; test: `testdata/run/print_float.sq`
- [x] **R14. `bytes.Buffer.Truncate(n > Len)` re-exposes stale bytes** (bug)
  `std/bytes/bytes.sq:33`. Should panic (or clamp).
 — fixed: panics like Go; test: `testdata/run/panic_truncate.sq`
- [x] **D3. `squark run` exits 255 when the program dies from a signal** (bug)
  `cmd/squark/main.go:75-76` — `ExitCode()` is -1 for signals. Fix: `128+signal` (or re-raise).
 — fixed: exits 128+signal; test: `TestRunSignals` (`cmd/squark/main_test.go`)
- [x] **D4. Ctrl-C during `squark run` leaks the temp dir** (bug)
  `cmd/squark/main.go:55-74`, `driver.go:97`. Fix: ignore/forward SIGINT/SIGTERM while the child
  runs, as `go run` does.
 — fixed: SIGINT/SIGTERM forwarded to the program while squark waits, then the temp dir is removed; test: `TestRunSignals`
- [x] **D6. Missing or multi-word `CC` → empty "this is a squark compiler bug" error** (bug)
  `driver.go:125,130,241` — `err` dropped; `CC="ccache clang"` passed as one program name.
  Fix: `strings.Fields(CC)`, `exec.LookPath`, include `err`.
 — fixed: CC split into words, looked up first (`C compiler "x" not found`), the exec error is included; test: `TestPackageC`
- [x] **D7. Package `.h` files are never written to the work dir; user C failures blamed on squark** (bug)
  `package.go:192-199`, `driver.go:142-150`. `shim.c` with `#include "shim.h"` → "file not
  found (this is a squark compiler bug)". Fix: write headers to a per-package dir + `-I`; word the
  error differently for user C.
 — fixed: each package's .c/.h go to `work/cpkgN_name/` under their own names with `-I` for it; failures in a local package's C name the package; test: `TestPackageC`
- [x] **D9. `TestStdlibDocCurrent` uses `strings.Contains`, so removed API isn't caught** (bug, tests)
  `driver_test.go:136`. Fix: compare exactly against the fenced block in `docs/stdlib.md`.
 — fixed: exact comparison with the ```go block; test: itself (checked by removing a doc line)
- [x] **D10. Default output name can be the source directory** (bug)
  `driver.go:233-235`. `squark build sleeper` from the parent → `ld: ... errno=21`. Fix: error
  clearly or add a suffix, as `go build` does.
 — fixed: error `build output "x" already exists and is a directory (use -o)`, as go build does; test: `TestCLIArgs`
- [x] **R18. `std/http`: HEAD responses larger than the write buffer send a chunked body** (bug, found while fixing R1)
  `w.head` is honoured only in the non-chunked branch of `finish`; `flushChunk` sends data for HEAD.
  Fix: in `Write`/`WriteString`/`WriteByte`, drop the bytes (but count them) when `w.head`; never
  switch a HEAD response to chunked.

- [x] **R19. First HTTP response heap-allocated on macOS** (bug, found 2026-10-14 checking the no-heap claim)
  `sq_httpdate` called `gmtime_r`, whose first call on macOS sets up time-zone state and
  notifications: 49 allocations, once. — fixed: UTC date by arithmetic (`utc_date`, checked
  against `gmtime_r` on 43k timestamps); test: `TestNoHeapAtRuntime`
- [x] **R20. Every failed syscall heap-allocated on Linux** (bug, same check)
  `os.Errno` builds its message with `strerror`, and glibc's translates it, allocating twice per
  call. — fixed: `strerrordesc_np` on glibc ≥ 2.32 (static, untranslated); test:
  `TestNoHeapAtRuntime` (fails with 1000 extra allocations if reverted)

- [x] **R21. `os.Errno` of an unknown errno heap-allocated on macOS** (bug, found auditing every libc call)
  macOS `strerror` formats "Unknown error: N" into a buffer it allocates on first use; `os.Errno(n)`
  accepts any n. — fixed: errnos beyond `ELAST` give a static "unknown error" (as on Linux). Side
  effect: the SDK's `ELAST` is 106, so errno 107 ("Capabilities insufficient" on newer macOS)
  prints "unknown error". Audit: every libc function the runtime/std import was classified;
  the non-syscall ones were measured on first call with all argument shapes used (errno 0..199,
  libm edge cases): 0 allocations on macOS and Linux. `TestNoHeapAtRuntime` now also covers idle
  parking with stack release, failed Dial/Listen, writes to closed sockets, unknown errnos, stdin
  through a pipe, and 200 tasks over a channel

## P3: minor / polish
 — fixed: `spill` counts and drops HEAD body bytes instead of chunking; Content-Length = dropped + buffered; test: `TestHTTP` (HEAD then GET on the same connection)
- [x] **P9. Error positions point at `(` / the operator instead of the expression start**
  `ast.go:243` `CallExpr.Pos()`, `IndexExpr`, `AssignStmt.P`. `aaaa, bbbb, cccc := f()` →
  `7:23` (Go: `7:2`). Fix: `CallExpr.Pos()` = `Fun.Pos()`; `AssignStmt.P` = `Lhs[0].Pos()`. — fixed: CallExpr/IndexExpr/SliceExpr/AssignStmt `Pos()` = start of the expression (as go/ast; operator-specific errors still use the operator); assignment-mismatch errors at the left side; test: `TestErrorPositions`
- [x] **D11. Flags after the dir are silently ignored** for `build`/`check`/`emit-c`
  (`cmd/squark/main.go:35-39`): `squark build examples/hello -o myout` writes `./hello`.
  Error on stray args. — fixed: build/check/emit-c reject extra arguments, doc allows a package; test: `TestCLIArgs`
- [x] **D12. `squark doc std nosuchpkg` prints nothing, exits 0** (`driver.go:424`). — fixed: `no package X (have …)` error; test: `TestDocUnknownPackage`
- [x] **D13. Only 2 of 6 examples are run-tested** (hello, pipeline). Add count, echo, greet, notes
  (+ `bench/squark` build-only). — fixed: `TestExamplesBuild` builds every example and bench program; `TestNotes` drives the notes API end to end (free port, temp db). count/greet/echo listen on fixed ports, so they are build-only
- [x] **R15. `docs/stdlib.md` shows `math.Pi = 3.14159`, `E = 2.71828`** — values are full
  precision at run time; the doc generator truncates. — fixed: doc prints float constants as their shortest float64 form; stdlib.md regenerated (covered by `TestStdlibDocCurrent`)
- [x] **R16 (unverified, Linux only).** epoll path in `netpoll` passes `g->wait_fd` as the fd,
  defeating the stale-event check in `fd_event`, so a stale event could wake a goroutine in
  `time.Sleep`. Needs a Linux run to confirm. — defensive fix, NOT reproduced: on Linux (Docker, arm64) the stale event never reached epoll_wait in my repro, with or without the fix, and I did not find out why. Kept: `sq_wait_idle` disarms the epoll registration on timeout (by code reading, epoll's stale check in `fd_event` compares the fd with itself). `TestStaleFdEvent` passes on macOS and Linux with or without the disarm, so it is only a regression guard. Also found via Docker: the runtime did not compile on glibc at all (`sigstack` clashed with glibc's function); renamed `sq_sigstack`. The whole suite now passes on Linux
- [x] **L5 (unverified).** Extern `from` contracts are level-aligned; possibly unsound for an
  extern returning one pointer level deeper than its argument (`***int` → `**int`). No std extern
  uses `from` today. Needs a C shim to confirm. — not pursued: documented behaviour, no std extern uses `from`, and a repro needs a contrived extern; revisit if `from` on externs gets used

---

## G: Go divergences: fix or document

Squark may differ from Go here. For each: either fix it, or add a line to `docs/spec.md` so the
"anything not mentioned behaves as in Go" rule stays true. Rec = recommendation.

- [x] **C1. Deferred calls can't change named results.** `cgen_stmt.go:486-492`, `cgen_expr.go:1056-1071`
  (`try`). `func f() (res int) { defer set(&res); return 1 }` → 1 (Go 42); bare `return` and `try`
  likewise. Rec: **fix**. Assign results to the named vars, run defers, return from the vars. It's
  the only way a defer can adjust an error result. — fixed: with defers and named results, `return v` / bare return / `try` assign the named results, run the defers, return the named results; test: `testdata/run/defer_named.sq`
- [x] **C2. Parallel assignment evaluates LHS index operands after earlier stores.**
  `cgen_stmt.go:196-213`, `multiAssign`. `i, x[i] = 2, 7` writes `x[2]`; `p, *p = q, 1` likewise.
  Rec: **fix** (take addresses first, then evaluate RHS, then store). — fixed: `lhsTargets` evaluates index/pointer/map operands of every target first (Go's two phases), in parallel assignment and in tuple/comma-ok assignment; test: `testdata/run/assign_order.sq` (output identical to Go)
- [x] **C7. Evaluation order differs:** `a[tr(1)] = tr(2)` runs 2,1; `getm()[key()]` evaluates key
  first; `getch() <- tr(5)` evaluates value first; keyed literals evaluate in field/index order,
  not source order; `m["a"] += len(m)` inserts the slot before evaluating the RHS (gives 1, Go 0).
  Sites: `cgen_stmt.go:150-162,293-297,59`, `cgen_expr.go:213-214,224,465-476,516-529`.
  Rec: **fix the map op-assign one** (observable without side-effecting calls); document
  "operands are evaluated left to right, except …" for the rest, or fix all with temporaries. — map op-assign fixed (RHS before the entry is created; test `testdata/run/map_opassign.sq`, identical to Go); the remaining differences documented in spec §5 "Order of evaluation"
- [x] **C4. `range` over an array iterates the live array, not a copy.** `cgen_stmt.go:682-689`.
  Rec: **document**. Copying a large array onto the stack each loop is expensive in a no-heap language. — documented in spec §6
- [x] **C8. Negative shift count yields 0 instead of panicking.** `cgen_expr.go:393-407`.
  Rec: **fix** (one branch for signed non-constant counts). Spec line 227 claims shifts are "as in Go". — fixed: signed non-constant counts are checked (`shiftC`), also in map op-assign; test: `testdata/run/panic_negshift.sq`; spec §5 updated
- [x] **C9. Float `min`/`max` ignore NaN and ±0 ordering.** `cgen_expr.go:896-916`. Rec: document,
  or fix with `isnan`/`signbit`. — documented in spec §5
- [x] **T10a. `1 << n` with untyped 1 is forced to `int`,** so `var mask uint64; mask |= 1 << n` and
  `var b uint8 = 1 << n` are rejected. `expr.go:1250`. Rec: **fix**; it's a very common idiom. — fixed: a shift of an untyped constant by a variable stays untyped and `convertUntyped` types it from context (also through arithmetic and unary ops), int by default; test: `testdata/run/shift_untyped.sq` (output identical to Go)
- [x] **T16. Unused variables and imports aren't reported** (`Var.Used` tracked, never checked).
  Rec: document, or add the error. — documented in spec §6
- [x] **T17. Small rejections:** `arena == nil` rejected though `f(nil)` is allowed; `min(3, 2.5)`
  rejected; `var b MyBool = x > 1` rejected. Rec: fix `MyBool` (untyped bool) and `min`; decide on `arena == nil`. — fixed: comparisons yield untyped bool (assignable to named bool types); min/max of untyped constants take the widest kind and fold to a constant; `arena == nil` allowed (arenas compare only with nil, like funcs); test: `testdata/run/untyped_misc.sq`
- [x] **P5. Unnamed method receivers `func (T) M()` are rejected.** `parser.go:228`. Rec: fix (trivial). — fixed: parser gives an unnamed receiver the blank name (`func (T)`, `func (*T)`); test: `testdata/run/unnamed_recv.sq`
- [x] **P8. A leading UTF-8 BOM is rejected.** Rec: fix (skip it in `newLexer`). — fixed: skipped in `newLexer`; test: `TestSourceEncoding`
- [x] **R7b. `AppendFloat` rounds half-up** (`0.125`,2 → `0.13`; `2.5`,0 → `3`; Go rounds to
  nearest-even on the exact binary value). Rec: document, unless R7's rewrite fixes it anyway. — fixed by R7's rewrite
- [x] **R17. stdlib edge cases:** `strings.Split(s, "", out)` fills `out` with empties (Go: one
  per rune); `strings.Count("héllo", "")` → 7 (Go: 6, runes+1); `utf8.RuneLen(0xD800)` → 3 (Go: -1).
  Rec: fix `Split`/`Count` (likely just wrong), fix `RuneLen`. — fixed: `Split` with empty sep splits UTF-8 sequences (SplitN semantics), `Count(s, "")` = runes+1, `RuneLen` of a surrogate = -1; test: `testdata/run/strings_edge.sq` (identical to Go)

---

## E: found by writing the examples (2026-10-15)

Five new examples (`examples/kv`, `life`, `chat`, `jsonq`, `jobs`) were written to use the
language as a user would. Repros below are minimal; the examples carry workarounds with comments.

### Runtime and compiler bugs

- [x] **E1. Closing a conn doesn't wake a goroutine blocked in `Read` on it** (bug) — fixed: a per-shard fd table (mmap'd, sized by RLIMIT_NOFILE) records each fd's reader and writer; `sq_close` wakes them and `wait_fd` returns false, so Read/Write/Accept return EBADF without reusing the number (closing a listener now ends a blocked Accept); test: `testdata/run/fd_waiters.sq`
  A goroutine parked in `c.Read` stays parked forever after another goroutine calls `c.Close()`,
  and the deadlock check never fires (it still counts as an fd waiter). Go returns an error from
  the Read. Fix: `sq_close` wakes goroutines waiting on that fd (with an error).
- [x] **E2. epoll: reading and writing one fd from two goroutines loses a wakeup** (bug, Linux,
  confirmed: the reader stayed parked) — fixed: one epoll registration per fd for both directions, data = fd, events dispatched to the table's reader and writer and the other direction re-armed; test: `testdata/run/fd_waiters.sq` `arm_fd` uses one epoll registration per fd, carrying one goroutine;
  a writer waiting on a socket replaces the reader parked on it, which is never woken. kqueue keys
  by (fd, filter) and is fine.
- [x] **E3. `string(s) in a` doesn't copy when `s` is already a string** (bug: spec says it
  copies) — fixed: the checker attaches the arena to string→string conversions too (lifetime and codegen already handled it); test: `testdata/run/arena_string_copy.sq` The result still views the original bytes. Workaround: `string([]byte(s) in a)`.
- [x] **E4. A pointer to a vector doesn't dereference** (bug: arrays do) — fixed: `derefVec` wraps the operand of index, slice, len/cap, range and the vector methods in an implicit `*`; examples/jsonq uses it; test: `testdata/run/vec_access.sq` With `v *[..4]int`,
  `v.Push(x)`, `len(v)` and `v[i]` are errors; `(*v).Push(x)` works.
- [x] **E5. `len`, index and `range` reject a non-addressable vector** (bug: spec says they work on
  storage) — fixed: `toViewRead` lets these read a temporary copy (hoisted for the statement); its elements aren't addressable, and other uses still need a variable; tests: `testdata/run/vec_access.sq`, `testdata/errors/vec_temp.sq` `len(fresh().hist)`, `fresh().hist[0]`, `range fresh().hist` fail with "must be stored
  in a variable to be used as a []int"; arrays in the same place work.
- [x] **E6. `len` of a package-level array isn't constant at package level** (go-diff) — fixed: a package variable used before the variable declarations are resolved gets its declared type on demand (`declaredVarType`); test: `testdata/run/const_len_global.sq` `var a
  [5]int; const N = len(a)` and `task t() pool len(a)` fail with "used before its type is known
  (initialization order)".
- [x] **E7. `squark doc` shows a `shared var` as `const NAME = <initializer>`** (bug) — fixed: shared and thread vars have their own cases; test: `TestDocVarKinds`
- [x] **E8. Arrays and structs of shared channels are rejected, with a self-contradicting error**
  (missing feature + bug) — fixed: shared vars may hold channels in arrays and structs (set up eagerly by shard 0, element by element); the error names the field holding a reference (`Msg.Body is a string`); examples/chat uses an array of relay channels; tests: `testdata/run/shared_chan_array.sq` (also under TSan), `testdata/errors/shared_chan_array.sq` `shared var reqs [4]chan Req cap 8` says "chan Req cap 8 can't be shared
  (... or a channel ...)". Needed for a request channel per thread or a reply channel per worker.
- [x] **E9. `json.Decoder` accepts invalid JSON** (bug) — fixed: numbers follow the JSON grammar (a fraction or exponent is ErrType for Int, consumed), literals must end at a delimiter, Skip accepts only values; new `Null()`; test: `testdata/run/json_strict.sq` `Skip` accepts `@@@` and `nul`; `Int`
  accepts `007`, and on `1.5`/`1e3` returns `(1, true)` with the error surfacing later as a syntax
  error; `Bool` accepts `truex`.
- [x] **E10. Errors cascade from invalid types** (polish) — fixed: once an error is reported, messages mentioning the invalid type (always a consequence) are dropped, and a failed range still declares its variables; the sort.Slice(&v) case now compiles (E4); test: `testdata/errors/cascade.sq` One mistake reports many: `sort.Slice(&v,
  less)` gives 17 errors inside `sort.sq`; `undefined: strings.CutPrefix` is followed by
  "cannot use ... as type invalid type"; a failed `range` reports `undefined: h`. Errors whose
  operands already have the invalid type should be dropped.

### Error messages

- [ ] **E11.** Name the culprit: the shared-channel element error should name the field
  (`Result.Name string`), "cannot be compared" should name the vector field that prevents it; a
  `shared var` is called "a constant" in "cannot slice t.P (t is a constant ...)"; `name string
  static` gives "expected ), found identifier" (want: write `name static string`); "append exceeds
  capacity" could suggest `in a`; a `thread var`/`var` map without `cap` only fails at run time
  (hint: `map[K]V cap N`); the budget error lists no fixes.

### Standard library gaps

- [ ] **E12.** A line reader over a `net.Conn`/`os.File` (every line protocol hand-writes one).
- [ ] **E13.** Text that can cross threads: `append`/`Append(s)` for byte vectors (`[..N]byte` can be
  sent on a shared channel but filled only byte by byte).
- [x] **E14.** (done except shortest-float formatting and `ParseFloat`, which need a correctly rounding algorithm: libc's may allocate; `Itoa` can't return memory without a heap, `FormatInt(buf, v)` is it; Fields overflow left as documented) — added `math.Round/Trunc/Mod/Log2/Log10/Tan/Atan/Atan2/Hypot/Inf/NaN/IsNaN/IsInf/MaxInt/MinInt/MaxFloat64/SmallestNonzeroFloat64`, `strconv.AppendIntBase/AppendUintBase/AppendIntPad`, `strings.CutPrefix/CutSuffix/AppendRepeat`, `os.ReadFile`, `io.ErrUnexpectedEOF/ErrTooLarge` (ReadFull returns ErrUnexpectedEOF on a short read, as Go); test: `testdata/run/stdlib_more.sq`. Was: `strconv.Itoa`, integer formatting in other bases (hex) and padding, shortest float
  formatting, `ParseFloat`; `math.Round`, `Trunc`, `Mod`, `Inf`, `NaN`, `IsNaN`, `MaxFloat64`;
  `strings.Repeat`, `CutPrefix`, `CutSuffix`; `os.ReadFile`; `strings.Fields` overflow detection.
- [x] **E15.** — added `net.Conn.CloseWrite`, `json.Decoder.Offset/Line` (and `Null`, E9); a write to a zero `bytes.Buffer` panics naming the fix; recovery after a decode error left out (the stream position is mid-value); test: `testdata/run/panic_stdlib_e15.sq`. Was: `net.Conn.CloseWrite` (half close); a zero `bytes.Buffer` silently drops writes;
  `json.Decoder` has no position, no `Null()`, no recovery after an error.

### Docs

- [ ] **E16.** Spec/stdlib gaps: vectors aren't comparable; arena growth policy (append reserves cap
  4, outgrown slices stay); listening on port 0 with threads gives each thread its own port;
  `select {}`; `try` zeroes named results; the memory report prints whenever a budget is declared;
  helper stacks are at least 64 KiB; ~350 KiB of runtime data per thread; a `net.Conn` (an fd) may
  cross threads; using one fd from two goroutines; `stdlib.md` drops the doc comments in `std/`.

## Progress log

Append one line per work session: date, items closed, anything left half-done.

- 2026-10-14: tracker created from the review. Nothing fixed yet.
- 2026-10-14: all P0 items closed: L1, L2, C6, T1, T3, I1, R1, R2, R3, C3, C5, plus R11 (P1). New
  item R18 (HEAD + chunked) logged. Next up: P1, starting at L3. Nothing committed yet; the
  working tree also holds the user's own uncommitted changes.
- 2026-10-14: all P1 items closed (L3, C10, C11, T6, T7, T11, T8, P1, P2, P3, R4, R5, R6, R7, R8, R9,
  R12, D5, D8) plus P4, P6, P7, P8, R7b, R10, D9 from lower sections. New APIs: `math.Float64bits`,
  `math.Float64frombits`, `utf8.DecodeRuneInString`, `strconv.ErrBase`, `http.MaxServers`,
  `http.ErrTooManyServers`; `net.ParkIdle`/`NextIdle` changed signature. Next up: P2 (L4, T10b, …).
- 2026-10-14: all P2 items closed (L4, T10b, T10c, T12, T13, T14, T15, R13, R14, R18, D3, D4, D6,
  D7, D10) plus T10a (G list) and D11 (P3). New: `cmd/squark/main_test.go` (CLI signal/arg tests).
  Next up: P3 (P9, D12, D13, R15; R16/L5 need a Linux box / a C shim), then the G list.
- 2026-10-14: P3 and the G list closed. P9, D12, D13, R15 fixed; R16 defensive fix (not
  reproducible); L5 not pursued. Docker run found the runtime didn't compile on glibc
  (`sigstack`), fixed; the whole suite passes on Linux arm64. G list: C1, C2, C8, P5, R17, T17
  fixed; C7 partly fixed (map op-assign), rest documented with C4, C9, T16 in spec §5/§6.
  All items closed. Nothing committed.
- 2026-10-14: checked the no-heap claim at run time with an injected allocation counter: R19, R20
  found and fixed; `TestNoHeapAtRuntime` (macOS + Linux) now guards it. Exception, as documented:
  the `sqlite` package's C library allocates.
- 2026-10-14: audited every libc call (both OSes); R21 found and fixed; no-heap test widened.
- 2026-10-15: five examples written by separate agents found E1–E16 (section E). Examples
  committed with workarounds.
