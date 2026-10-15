# Writing squark

squark is a Go-shaped language without a heap. Before writing or changing `.sq` code, read:

- `docs/spec.md`: the language reference. It covers how squark differs from Go, the memory rules,
  and a table of compiler errors with their fixes.
- `docs/stdlib.md`: the standard library API with reference provenance (generated; don't edit by
  hand, run `docs/gen.sh`).

Check your work with `bin/squark check <dir>` (types and lifetimes) and `bin/squark run <dir>`. Build
the compiler with `go build -o bin/squark ./cmd/squark`. Compiler errors name the fix; the spec's
error table maps each one to the usual rewrite.

Working on the compiler itself: `go test ./internal/...` runs the error-annotation tests
(`testdata/errors`) and the run tests (`testdata/run`, `.sq` + expected `.out`), and the run tests
again under AddressSanitizer (`TestRunASan`: a report means the lifetime checker let a reference
outlive its memory; `-short` skips it). After changing the checker or codegen, fuzz:
`SQUARK_FUZZ=5000 go test ./internal/driver -run TestFuzzSoundness -v -timeout 1h` mutates the test
programs and runs the accepted ones under ASan, reporting ASan errors, compiler crashes and C the
compiler generates but clang rejects (findings land in a temp dir the log names).
