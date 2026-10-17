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

// file header and directives
expect("program", "program", "keyword.other.package");
expect("module http", "module", "keyword.other.package");
expect("module http", "http", "entity.name.namespace");
expect("program {\n\tbudget 64M\n\tthreads auto\n}", "budget", "keyword.other.directive");
expect("program {\n\tbudget 64M\n\tthreads auto\n}", "M", "keyword.other.unit.size");
expect("program {\n\tbudget 64M\n\tthreads auto\n}", "threads", "keyword.other.directive");
expect("program {\n\tshutdown 20s\n}", "shutdown", "keyword.other.directive");
expect("program {\n\tconnections 2048\n\tsockbuf 32K\n}", "connections", "keyword.other.directive");
expect("program {\n\tconnections 2048\n\tsockbuf 32K\n}", "sockbuf", "keyword.other.directive");
expect("module sqlite {\n\theap sqlite3 16M\n}", "heap", "keyword.other.directive");
expect("task serve(c net.Conn) pool 512 isolate {", "isolate", "keyword.other.clause");
expect("isolate := true", "isolate", "-");
expect("program {\n\tshutdown 20s\n}", "s", "keyword.other.unit.duration");
expect("program {\n\tshutdown 500ms\n}", "500", "constant.numeric.integer.decimal");
expect("program {\n\tpool http.serve_conn 128 // max\n}", "pool", "keyword.other.directive");
expect("program {\n\tpool http.serve_conn 128 // max\n}", "max", "comment.line");
expect("program {\n\tworkers 4\n}", "workers", "keyword.other.directive");
expect('module sqlite {\n\tlink "sqlite3"\n\tcflags "-I/x"\n\tldflags "-L/x"\n}', "link", "keyword.other.directive");
expect('module sqlite {\n\tlink "sqlite3"\n\tcflags "-I/x"\n\tldflags "-L/x"\n}', "cflags", "keyword.other.directive");
expect('module sqlite {\n\tlink "sqlite3"\n\tcflags "-I/x"\n\tldflags "-L/x"\n}', "sqlite", "entity.name.namespace");
expect("program {\n\tthreads 4\n}\n\nfn main() {\n\tthreads := 4\n}", "threads", "-", 1);
expect("budget 64M", "budget", "-");
expect("\tthreads := 4", "threads", "-");
expect("\tprogram := 1", "program", "-");

// imports
expect("use http, sqlite, strconv", "use", "keyword.other.import");
expect("use http, sqlite, strconv", "sqlite", "entity.name.namespace");
expect("use math/rand as r", "math/rand", "entity.name.namespace");
expect("use math/rand as r", "as", "keyword.other.as");
expect("use math/rand as r", "r", "entity.name.namespace.alias");
expect("use ./mylib", "./mylib", "entity.name.namespace");
expect('use "strings"', "strings", "string.quoted.double");
expect("use (\n\tbytes\n\tmath/rand as r // rng\n)", "bytes", "entity.name.namespace");
expect("use (\n\tbytes\n\tmath/rand as r // rng\n)", "as", "keyword.other.as");
expect("use (\n\tbytes\n\tmath/rand as r // rng\n)", "rng", "comment.line");
expect("\tuse := 1", "use", "-");
expect("x := as", "as", "-");

// functions, methods, results
expect("fn add(a int, b int) -> int {", "fn", "keyword.function");
expect("fn add(a int, b int) -> int {", "add", "entity.name.function");
expect("fn add(a int, b int) -> int {", "->", "keyword.operator.arrow");
expect("fn add(a int, b int) -> int {", "int", "storage.type.builtin");
expect("impl File {", "File", "entity.name.type");
expect("impl File {", "impl", "keyword.other.impl");
expect("\tfn read(&self, p u8[]) -> int ! {", "self", "variable.language.self");
expect("\t\treturn self.n", "self", "variable.language.self");
expect("\tfn read(&self, p u8[]) -> int ! {", "read", "entity.name.function");
expect("\tfn read(&self, p u8[]) -> int ! {", "->", "keyword.operator.arrow");
expect("\tfn read(&self, p u8[]) -> int ! {", "!", "keyword.operator.error");
expect("\tfn read(&self, p u8[]) -> int ! {", "u8", "storage.type.builtin");
expect("\tfn read(&self, p u8[]) -> int ! {", "[", "punctuation.bracket");
expect("pub fn errno(n int) -> ! {", "pub", "storage.modifier.pub");
expect("pub fn errno(n int) -> ! {", "!", "keyword.operator.error");
expect("fn pair() -> (int, str) ! {", "str", "storage.type.builtin");
expect("fn pair() -> (int, str) ! {", "!", "keyword.operator.error");
expect("fn h() -> (n int, err error) {", "error", "storage.type.builtin");
expect("var f fn(int) -> bool", "fn", "keyword.function");
expect("var f fn(int) -> bool", "bool", "storage.type.builtin");
expect("f := os.open(path)!", "!", "keyword.operator.error");
expect("f := os.open(path)!", "open", "entity.name.function.call");
expect("if x != 0 {", "!=", "keyword.operator.comparison");

// tasks and externs
expect("task worker(id int) pool 4 preemptible {", "task", "keyword.function.task");
expect("task worker(id int) pool 4 preemptible {", "worker", "entity.name.function.task");
expect("task worker(id int) pool 4 preemptible {", "pool", "keyword.other.clause");
expect("task worker(id int) pool 4 preemptible {", "preemptible", "keyword.other.clause");
expect("task work(i int) pool len(peers) {", "pool", "keyword.other.clause");
expect("task worker() pool num_workers {", "pool", "keyword.other.clause");
expect('extern fn sys_read(fd int, b u8[]) -> int stack 8K c"sq_read"', "extern", "storage.modifier.extern");
expect('extern fn sys_read(fd int, b u8[]) -> int stack 8K c"sq_read"', "sys_read", "entity.name.function");
expect('extern fn sys_read(fd int, b u8[]) -> int stack 8K c"sq_read"', "stack", "keyword.other.clause");
expect('extern fn sys_read(fd int, b u8[]) -> int stack 8K c"sq_read"', "c", "storage.type.string.c-symbol");
expect('extern fn sys_read(fd int, b u8[]) -> int stack 8K c"sq_read"', "sq_read", "string.quoted.double.c-symbol");
expect('extern fn fetch(id int) -> int stack 8K blocking c"jobs_fetch"', "blocking", "keyword.other.clause");
expect('pub extern fn exit(code int) stack 8K c"sq_exit"', "exit", "entity.name.function");
expect('pub extern fn exit(code int) stack 8K c"sq_exit"', "stack", "keyword.other.clause");
expect("start worker(i)", "start", "keyword.control.start");
expect("start worker(i)", "worker", "entity.name.function.call");
expect("\tstart http.serve(l)", "start", "keyword.control.start");

// contextual words stay identifiers elsewhere
expect("pool := 3", "pool", "-");
expect("\tworkers = 2", "workers", "-");
expect("stack.push(root)", "stack", "-");
expect("var stack *Node[..64]", "stack", "-");
expect("stack := s[:0]", "stack", "-");
expect("start := 0", "start", "-");
expect("start = i + 1", "start", "-");
expect("start, end := 0, 1", "start", "-");
expect("put(s[start:i])", "start", "-");
expect('port := start("127.0.0.1:0")', "start", "entity.name.function.call");
expect("r.start = r.end", "start", "-");
expect("thread := 1", "thread", "-");
expect("shared := true", "shared", "-");
expect("static := 1", "static", "-");
expect("x := select", "select", "-");
expect("m.select(a, b)", "select", "entity.name.function.call");
expect("loop := 1", "loop", "-");
expect("with := 1", "with", "-");
expect("n := cap(s)", "cap", "support.function.builtin");
expect("x.close()", "close", "entity.name.function.call");
expect("req.arena = scratch", "arena", "-");

// package variables, types, faults, constants
expect("thread hits int", "thread", "storage.modifier");
expect("shared jobs Job chan[16]", "shared", "storage.modifier");
expect("shared jobs Job chan[16]", "chan", "keyword.type");
expect("pub thread stdout = File{sysfd: 1}", "pub", "storage.modifier.pub");
expect("pub thread stdout = File{sysfd: 1}", "thread", "storage.modifier");
expect("struct Node {", "Node", "entity.name.type");
expect("struct Node {", "struct", "keyword.type");
expect("Celsius: f64", "Celsius", "entity.name.type");
expect("pub Duration: int", "Duration", "entity.name.type");
expect("pub struct File {", "File", "entity.name.type");
expect("\tpub body u8[]", "pub", "storage.modifier.pub");
expect('fault NotFound 404 "not found"', "fault", "keyword.other.fault");
expect('fault NotFound 404 "not found"', "NotFound", "variable.other.constant.fault");
expect('pub fault Closed "closed"', "Closed", "variable.other.constant.fault");
expect('fault (\n\tNotArray 100 "not an array"\n\tpub Short -3 "short"\n)', "NotArray", "variable.other.constant.fault");
expect('fault (\n\tNotArray 100 "not an array"\n\tpub Short -3 "short"\n)', "Short", "variable.other.constant.fault");
expect('fault (\n\tNotArray 100 "not an array"\n\tpub Short -3 "short"\n)', "100", "constant.numeric.integer.decimal");
expect("const (\n\tpub pi = 3.14\n)", "pub", "storage.modifier.pub");
expect("const (\n\tpub pi = 3.14\n)", "const", "keyword.var");
expect("enum Dir { north, east }", "enum", "keyword.type.enum");
expect("say(.alice, \"hi\", .bob)", "alice", "variable.other.enummember");
expect("case .ok, .created:", "created", "variable.other.enummember");
expect("enum Dir { north, east }", "Dir", "entity.name.type.enum");
expect("pub enum Status u16 {", "u16", "storage.type.builtin");

// statements
expect("for x in xs {", "for", "keyword.control");
expect("for x in xs {", "in", "keyword.operator.in");
expect("for i, x in xs {", "in", "keyword.operator.in");
expect("for i in 0..n {", "0", "constant.numeric.integer.decimal");
expect("for i in 0..n {", "..", "keyword.operator.range");
expect("for i in 0..256 {", "256", "constant.numeric.integer.decimal");
expect("for i in 0..len(xs) {", "len", "support.function.builtin");
expect("for i in 1..n-1 {", "1", "constant.numeric.integer.decimal");
expect("for i := 0; i < n; i++ {", "++", "keyword.operator.increment");
expect("while cond {", "while", "keyword.control");
expect("loop {", "loop", "keyword.control.loop");
expect("select {", "select", "keyword.control.select");
expect("select {\ncase v := <-c:", "<-", "keyword.operator.channel");
expect("with arena scratch 64K {", "with", "keyword.control.with");
expect("with arena scratch 64K {", "arena", "storage.type.arena");
expect("with arena scratch 64K {", "scratch", "variable.other.arena");
expect("with arena store 2 * max_input {", "store", "variable.other.arena");
expect("with deadline 2 * time.second {", "with", "keyword.control.with");
expect("with deadline 2 * time.second {", "deadline", "keyword.control.deadline");
expect("deadline := 1", "deadline", "-");
expect("arena keep 256", "keep", "variable.other.arena");
expect("fn build(a arena, depth int) -> *Node {", "arena", "storage.type.arena");
expect("defer f.close()", "defer", "keyword.control");
expect("outer:\n\tfor i := 0; i < n; i++ {", "outer", "entity.name.label");
expect("loop:\n\tfor {", "loop", "entity.name.label");
expect("\t\tbreak outer", "outer", "entity.name.label");
expect("\t\tbreak loop", "loop", "entity.name.label");
expect("if (x) {", "if", "keyword.control");

// expressions
expect("if a and not b or c {", "and", "keyword.operator.logical");
expect("if a and not b or c {", "not", "keyword.operator.logical");
expect("if a and not b or c {", "or", "keyword.operator.logical");
expect("x := new(T) in a", "in", "keyword.operator.in");
expect("x := new(T) in a", "new", "support.function.builtin");
expect("v := <-c", "<-", "keyword.operator.channel");
expect("dst = append(dst, s...)", "...", "keyword.operator.variadic");

// provenance
expect("pub fn split(s str in out, sep str, out str[]) -> str[] in out {", "in", "keyword.operator.in");
expect("pub fn split(s str in out, sep str, out str[]) -> str[] in out {", "in", "keyword.operator.in", 1);
expect("pub fn new(msg str in static) -> ! {", "static", "storage.modifier.provenance");
expect("pub fn arg(i int) -> str in static {", "static", "storage.modifier.provenance");
expect("fn insert(b *Node in a, a arena) {", "Node", "-");

// types, sizes, numbers
expect("budget 128M", "128", "constant.numeric.integer.decimal");
expect("x := 0x1p4", "0x1p4", "constant.numeric.float.hexadecimal");
expect("x := 1_000", "1_000", "constant.numeric.integer.decimal");
expect("x := 1e-9", "1e-9", "constant.numeric.float.decimal");
expect("x := 1.5", "1.5", "constant.numeric.float.decimal");
expect("x := .5", ".5", "constant.numeric.float.decimal");
expect("x := 0b1010", "0b1010", "constant.numeric.integer.binary");
expect("x := 0o644", "0o644", "constant.numeric.integer.octal");
expect("var line u8[..256]", "..", "keyword.operator.vector");
expect("var line u8[..256]", "u8", "storage.type.builtin");
expect("var t u32[256]", "u32", "storage.type.builtin");
expect("xs := int[...]{1, 2, 3}", "...", "keyword.operator.variadic");
expect("var seen int[str; 1024]", "str", "storage.type.builtin");
expect("var p *(u8[64])", "*", "keyword.operator.arithmetic");
expect("a := f32x4(xs[i:])", "f32x4", "storage.type.builtin");
expect("var m u8x16", "u8x16", "storage.type.builtin");
expect("var m i32x8", "i32x8", "storage.type.builtin");
expect("var m f64x4", "f64x4", "storage.type.builtin");
expect("f32x45", "f32x45", "-");
expect("var p rawptr", "rawptr", "storage.type.builtin");
expect("x := float32(1)", "float32", "entity.name.function.call");
expect("x := string", "string", "-");

// removed Go words are plain identifiers now
expect("func := 1", "func", "-");
expect("range := 1", "range", "-");
expect("go := 1", "go", "-");
expect("try := 1", "try", "-");
expect("package := 1", "package", "-");

// literals, comments, pragmas
expect('s := "a\\tb"', "\\t", "constant.character.escape");
expect('s := "a\\qb"', "\\q", "invalid.illegal");
expect("r := '\\n'", "\\n", "constant.character.escape");
expect("r := 'x'", "'x'", "constant.character.rune");
expect("//squark:noinline", "squark:", "keyword.control.directive.pragma");
expect("x := 1 // in a comment: arena fn", "in a comment: arena fn", "comment.line");
expect("x := `raw \\n`", "raw \\n", "string.quoted.raw");
expect("return error(404, \"not found\")", "error", "storage.type.builtin");

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
const keywords = new Set(["if", "for", "while", "switch", "return", "fn", "defer", "in", "arena", "task", "and", "or", "not"]);
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
