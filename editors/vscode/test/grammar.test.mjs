// Tokenizes squark source with VS Code's own TextMate engine and checks the scopes of chosen
// tokens, then runs the grammar over every .sq file in the repository looking for tokens that
// should never appear (invalid escapes in real code, keywords scoped as calls).
//
//	npm install && npm test
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createRequire } from "node:module";
import vsctm from "vscode-textmate";
import oniguruma from "vscode-oniguruma";

const here = path.dirname(fileURLToPath(import.meta.url));
const require = createRequire(import.meta.url);
const wasm = fs.readFileSync(require.resolve("vscode-oniguruma/release/onig.wasm"));
await oniguruma.loadWASM(wasm.buffer.slice(wasm.byteOffset, wasm.byteOffset + wasm.byteLength));

const registry = new vsctm.Registry({
  onigLib: Promise.resolve({
    createOnigScanner: (p) => new oniguruma.OnigScanner(p),
    createOnigString: (s) => new oniguruma.OnigString(s),
  }),
  loadGrammar: async () =>
    vsctm.parseRawGrammar(
      fs.readFileSync(path.join(here, "../syntaxes/squark.tmLanguage.json"), "utf8"),
      "squark.tmLanguage.json",
    ),
});
const grammar = await registry.loadGrammar("source.squark");

// tokenize returns [text, innermost scope] for every token of src
function tokenize(src) {
  let state = vsctm.INITIAL;
  const out = [];
  for (const line of src.split("\n")) {
    const r = grammar.tokenizeLine(line, state);
    for (const t of r.tokens) {
      out.push([line.slice(t.startIndex, t.endIndex), t.scopes[t.scopes.length - 1], t.scopes]);
    }
    state = r.ruleStack;
  }
  return out;
}

let failures = 0;
function fail(msg) {
  failures++;
  console.error("FAIL " + msg);
}

// expect checks that the n-th occurrence (default first) of text in src gets a scope starting with
// want ("-" means: no squark-specific scope, i.e. plain source).
function expect(src, text, want, nth = 0) {
  const toks = tokenize(src).filter(([t]) => t.trim() === text);
  if (toks.length <= nth) return fail(`${JSON.stringify(text)} not found as a token in ${JSON.stringify(src)}`);
  const scope = toks[nth][1];
  const ok = want === "-" ? scope === "source.squark" : scope.startsWith(want);
  if (!ok) fail(`${JSON.stringify(src)}: ${JSON.stringify(text)} is ${scope}, want ${want}`);
}

// keywords, including squark's own
expect("arena scratch 64K {", "arena", "storage.type.arena");
expect("arena scratch 64K {", "scratch", "variable.other.arena");
expect("x := new(T) in a", "in", "keyword.operator.in");
expect("x := new(T) in a", "new", "support.function.builtin");
expect("n := try f()", "try", "keyword.control");
expect("task worker(id int) pool 8 {", "task", "keyword.function.task");
expect("task worker(id int) pool 8 {", "worker", "entity.name.function.task");
expect("task worker(id int) pool 8 {", "pool", "keyword.other.clause");
expect("task crunch() pool 4 preemptible {", "preemptible", "keyword.other.clause");
expect('extern "sq_read" func sysRead(fd int, b []byte) int stack 8K', "extern", "storage.modifier.extern");
expect('extern "sq_read" func sysRead(fd int, b []byte) int stack 8K', "sysRead", "entity.name.function");
expect('extern "sq_read" func sysRead(fd int, b []byte) int stack 8K', "stack", "keyword.other.clause");
expect('extern "slow" func q(s string) int stack 128K blocking', "blocking", "keyword.other.clause");
expect("select {", "select", "keyword.control.select");
expect("shared var jobs chan Job cap 64", "shared", "storage.modifier");
expect("thread var hits int", "thread", "storage.modifier");

// contextual words stay identifiers elsewhere
expect("pool := 3", "pool", "-");
expect("stack := s[:0]", "stack", "-");
expect("thread := 1", "thread", "-");
expect("shared := true", "shared", "-");
expect("from := 2", "from", "-");
expect("static := 1", "static", "-");
expect("budget := 5", "budget", "-");
expect("x := select", "select", "-");
expect("n := cap(s)", "cap", "support.function.builtin");

// directives (column 0)
expect("budget 64M", "budget", "keyword.other.directive");
expect("pool http.serveConn 256", "pool", "keyword.other.directive");
expect("threads auto", "threads", "keyword.other.directive");
expect("workers 4", "workers", "keyword.other.directive");
expect('link "sqlite3"', "link", "keyword.other.directive");
expect("\tthreads := 4", "threads", "-");

// sizes, numbers, vectors, storage
expect("budget 128M", "128", "constant.numeric.integer.decimal");
expect("budget 128M", "M", "keyword.other.unit.size");
expect("x := 0x1p4", "0x1p4", "constant.numeric.float.hexadecimal");
expect("x := 1_000", "1_000", "constant.numeric.integer.decimal");
expect("x := 1e-9", "1e-9", "constant.numeric.float.decimal");
expect("x := 0b1010", "0b1010", "constant.numeric.integer.binary");
expect("var line [..256]byte", "..", "keyword.operator.vector");
expect("var line [..256]byte", "byte", "storage.type.builtin");
expect("a := float32x4(xs[i:])", "float32x4", "storage.type.builtin");
expect("var m uint8x16", "uint8x16", "storage.type.builtin");
expect("float32x45", "float32x45", "-");
expect("var seen map[string]int cap 1024", "cap", "keyword.other.capacity");
expect("var jobs chan Job cap 16", "cap", "keyword.other.capacity");

// provenance
expect("func Split(s string into out, sep string, out []string) []string from out {", "into", "storage.modifier.provenance");
expect("func Split(s string into out, sep string, out []string) []string from out {", "from", "storage.modifier.provenance");
expect("func New(msg static string) error {", "static", "storage.modifier.provenance");
expect("func Name() string static {", "static", "storage.modifier.provenance");

// declarations and calls
expect("func (w *ResponseWriter) WriteHeader(code int) {", "WriteHeader", "entity.name.function");
expect("func (w *ResponseWriter) WriteHeader(code int) {", "ResponseWriter", "-");
expect("type Node struct {", "Node", "entity.name.type");
expect("x := strconv.Atoi(s)", "Atoi", "entity.name.function.call");
expect("if (x) {", "if", "keyword.control");

// literals, comments, pragmas
expect('s := "a\\tb"', "\\t", "constant.character.escape");
expect('s := "a\\qb"', "\\q", "invalid.illegal");
expect("r := '\\n'", "\\n", "constant.character.escape");
expect("r := 'x'", "'x'", "constant.character.rune");
expect("//squark:noinline", "squark:", "keyword.control.directive.pragma");
expect("x := 1 // in a comment: arena try", "in a comment: arena try", "comment.line");
expect("x := `raw \\n`", "raw \\n", "string.quoted.raw");
expect("const ErrX = error(404, \"not found\")", "error", "storage.type.builtin");
expect("v := <-c", "<-", "keyword.operator.channel");

// every .sq file in the repository: no invalid escapes, and nothing a keyword misread as a call
const root = path.resolve(here, "../../..");
const files = [];
(function walk(dir) {
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    if (e.name === "node_modules" || e.name.startsWith(".")) continue;
    const p = path.join(dir, e.name);
    if (e.isDirectory()) walk(p);
    else if (e.name.endsWith(".sq")) files.push(p);
  }
})(root);
const keywords = new Set(["if", "for", "switch", "return", "func", "go", "defer", "range", "select", "try", "in", "arena", "task"]);
for (const f of files) {
  const src = fs.readFileSync(f, "utf8");
  const rel = path.relative(root, f);
  for (const [text, scope, scopes] of tokenize(src)) {
    const inComment = scopes.some((s) => s.startsWith("comment"));
    if (scope.startsWith("invalid") && !rel.startsWith("testdata/errors/")) fail(`${rel}: invalid token ${JSON.stringify(text)}`);
    if (!inComment && scope.startsWith("entity.name.function.call") && keywords.has(text)) {
      fail(`${rel}: keyword ${text} scoped as a call`);
    }
  }
}

if (failures) {
  console.error(`${failures} failure(s)`);
  process.exit(1);
}
console.log(`ok: grammar checks pass; ${files.length} .sq files tokenized`);
