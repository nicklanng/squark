# Writing squark

squark is a language without a heap, with its own syntax and semantics (it looks a little like Go:
don't write Go syntax, and don't assume Go's behaviour). Before writing or changing `.sq` code, read:

- `docs/spec.md`: the language reference. It covers the syntax, the memory rules, and a table of
  compiler errors with their fixes.
- `docs/stdlib.md`: the standard library API with reference provenance (generated; don't edit by
  hand, run `docs/gen.sh`).

Check your work with `bin/squark check <dir>` (types and lifetimes) and `bin/squark run <dir>`. Build
the compiler with `go build -o bin/squark ./cmd/squark`. Compiler errors name the fix; the spec's
error table maps each one to the usual rewrite.

The tour (`squark tour`) lives in `tour/`: one Markdown file per chapter in `tour/lessons` (the
format is described in `internal/tour/content.go`) and the page in `tour/site`. `go test
./internal/tour` compiles and runs every lesson and checks each goal against its solution
(`TOUR=chapter` runs one chapter, `TOUR_SHOW=1` prints the output); change a lesson when the
language changes under it.

Working on the compiler itself: `go test ./internal/...` runs the error-annotation tests
(`testdata/errors`) and the run tests (`testdata/run`, `.sq` + expected `.out`), and the run tests
again under AddressSanitizer (`TestRunASan`: a report means the lifetime checker let a reference
outlive its memory; `-short` skips it). After changing the checker or codegen, fuzz:
`SQUARK_FUZZ=5000 go test ./internal/driver -run TestFuzzSoundness -v -timeout 1h` mutates the test
programs and runs the accepted ones under ASan, reporting ASan errors, compiler crashes and C the
compiler generates but clang rejects (findings land in a temp dir the log names). On Linux (epoll,
glibc, gcc): `scripts/test-linux.sh` runs the suite in Docker (`-short`, `CC=gcc`, `FUZZ=N`,
`PLATFORM=linux/amd64`; see the script).
