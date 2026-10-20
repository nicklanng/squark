// Runs the extension's JSON-RPC client (rpc.js) against the real squark lsp: a buffer with an
// error gets its diagnostic, hover and definition answer, and formatting returns squark fmt's
// output. It needs the compiler built at ../../bin/squark (go build -o bin/squark ./cmd/squark).
//
//	npm test
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import assert from "node:assert/strict";
import { fileURLToPath, pathToFileURL } from "node:url";
import { createRequire } from "node:module";

const here = path.dirname(fileURLToPath(import.meta.url));
const require = createRequire(import.meta.url);
const { Client } = require("../rpc.js");

const squark = path.resolve(here, "../../../bin/squark");
if (!fs.existsSync(squark)) {
  console.log("skip: no compiler at " + squark);
  process.exit(0);
}
const dir = fs.mkdtempSync(path.join(os.tmpdir(), "squark-lsp-"));
const file = path.join(dir, "main.sq");
fs.writeFileSync(file, "program\n\nfn main() {}\n");
const uri = pathToFileURL(file).toString();

const published = [];
let notice;
const client = new Client(squark, ["lsp"], (method, params) => {
  if (method === "textDocument/publishDiagnostics" && params.uri === uri) {
    published.push(params.diagnostics);
    if (notice) notice();
  }
});
const nextDiagnostics = () => new Promise((resolve, reject) => {
  const n = published.length;
  const timer = setTimeout(() => reject(new Error("no diagnostics")), 20000);
  notice = () => {
    if (published.length > n) {
      clearTimeout(timer);
      resolve(published[published.length - 1]);
    }
  };
});

try {
  const init = await client.request("initialize", { processId: process.pid, rootUri: null, capabilities: {} });
  assert.ok(init.capabilities.hoverProvider);
  client.notify("initialized", {});

  const src = "program\n\n// twice doubles n.\nfn twice(n int) -> int {\n\treturn 2*n\n}\n\nfn main() {\n\tprintln(twice(nope))\n}\n";
  let next = nextDiagnostics();
  client.notify("textDocument/didOpen", { textDocument: { uri, languageId: "squark", version: 1, text: src } });
  let ds = await next;
  assert.equal(ds.length, 1);
  assert.match(ds[0].message, /undefined: nope/);
  assert.deepEqual(ds[0].range.start, { line: 8, character: 15 });

  const hover = await client.request("textDocument/hover", { textDocument: { uri }, position: { line: 8, character: 10 } });
  assert.match(hover.contents.value, /fn twice\(n int\) -> int/);
  assert.match(hover.contents.value, /twice doubles n\./);

  const def = await client.request("textDocument/definition", { textDocument: { uri }, position: { line: 8, character: 10 } });
  assert.equal(def.uri, uri);
  assert.equal(def.range.start.line, 3);

  next = nextDiagnostics();
  const fixed = src.replace("nope", "21");
  client.notify("textDocument/didChange", { textDocument: { uri, version: 2 }, contentChanges: [{ text: fixed.replace("\treturn 2*n", "return 2*n") }] });
  ds = await next;
  assert.equal(ds.length, 0);

  const edits = await client.request("textDocument/formatting", { textDocument: { uri }, options: {} });
  assert.equal(edits.length, 1);
  assert.equal(edits[0].newText, fixed);
  console.log("ok: squark lsp answers the extension's client");
} finally {
  await client.stop();
  fs.rmSync(dir, { recursive: true, force: true });
}
