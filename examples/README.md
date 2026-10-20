# Examples

Each directory is one program: `squark run examples/<name> [args]`.

| example | what it is |
|---|---|
| `hello` | a first program: structs, methods, vectors, variadic functions |
| `greet` | a one-route HTTP server |
| `count` | an HTTP server with SQLite |
| `notes` | a JSON REST API backed by SQLite (the benchmark server) |
| `echo` | a TCP echo server |
| `kv` | a Redis-like key-value server with a line protocol |
| `chat` | a TCP chat room spread over scheduler threads |
| `pipeline` | a worker pool fed by channels |
| `jobs` | a job runner over scheduler threads: shared channels, preemptible and blocking work |
| `jsonq` | a report over a JSON array of orders: decoding, validation, sorting, arenas |
| `life` | Game of Life and a Mandelbrot set: pure computation |
| `shop` | two services calling each other by name, built for containers: deadlines, an admin port with probes and metrics, JSON logs, graceful stops, proven resource limits, a `FROM scratch` image, Compose and Kubernetes files |

Examples with an `expected.out` are also tests: run with no arguments, they drive themselves
(scripted clients, fixed input) and `go test ./internal/...` compares their output.

## Layout

Every example follows the same layout, so they read alike:

- **`main.sq` is the program** you would write. A short header says what it is and how to run
  it, and lists in a few bullets what it shows about squark. Explanations of squark-specific
  choices (where memory lives, why a reference is safe) sit next to the code they explain.
- **`selftest.sq`** (same package) holds what only the test needs: scripted clients, fixed
  input, test-mode switches. `main` calls into it in one place. An example whose normal run is
  its test (`hello`, `jobs`, `life`) has none.
- **`*_test.sq`** holds tests of the program's pieces, run by `squark test examples/<name>`
  (`jsonq` has them).
- **Order:** header comment, `program` with its directives (`budget`, `pool`, `threads`), `use`, constants,
  types and package variables, then the code grouped by topic under `// ---- topic ----`
  markers, with `main` last.
- **Formatting:** as `squark fmt` prints it (tabs, aligned fields, constants and trailing
  comments within a block), one blank line between declarations, lines within 100 columns.
- **Current library:** use the standard library rather than hand-rolled equivalents
  (`bufio.Reader` for line protocols, `http.handle_func` and `w.json()` for HTTP, `!` where an
  error only propagates).
