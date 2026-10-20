// The squark extension: highlighting comes from the grammar; this starts `squark lsp` for
// the compiler's errors as you type, hover, go to definition and formatting (squark fmt).
"use strict";

const vscode = require("vscode");
const fs = require("node:fs");
const path = require("node:path");
const { Client } = require("./rpc.js");

let client;
let diagnostics;

// squarkCommand is the compiler to run: the squark.path setting, else bin/squark in a workspace
// folder (a checkout of squark), else squark on the PATH.
function squarkCommand() {
  const configured = vscode.workspace.getConfiguration("squark").get("path");
  if (configured) return configured;
  for (const folder of vscode.workspace.workspaceFolders || []) {
    const bin = path.join(folder.uri.fsPath, "bin", "squark");
    if (fs.existsSync(bin)) return bin;
  }
  return "squark";
}

const toRange = (r) => new vscode.Range(r.start.line, r.start.character, r.end.line, r.end.character);
const docParams = (doc) => ({ textDocument: { uri: doc.uri.toString() } });
const posParams = (doc, pos) => ({ ...docParams(doc), position: { line: pos.line, character: pos.character } });

function activate(context) {
  diagnostics = vscode.languages.createDiagnosticCollection("squark");
  context.subscriptions.push(diagnostics);
  const command = squarkCommand();
  client = new Client(command, ["lsp"], (method, params) => {
    if (method !== "textDocument/publishDiagnostics") return;
    diagnostics.set(vscode.Uri.parse(params.uri), params.diagnostics.map((d) => {
      const diag = new vscode.Diagnostic(toRange(d.range), d.message, vscode.DiagnosticSeverity.Error);
      diag.source = "squark";
      return diag;
    }));
  });
  client.proc.on("error", (err) => {
    vscode.window.showWarningMessage(`squark: can't start "${command} lsp" (${err.message}); set squark.path to the compiler`);
  });
  const ready = client.request("initialize", {
    processId: process.pid,
    rootUri: vscode.workspace.workspaceFolders?.[0]?.uri.toString() ?? null,
    capabilities: {},
  }).then(() => client.notify("initialized", {}));

  const isSquark = (doc) => doc.languageId === "squark" && doc.uri.scheme === "file";
  const open = (doc) => {
    if (!isSquark(doc)) return;
    ready.then(() => client.notify("textDocument/didOpen", {
      textDocument: { uri: doc.uri.toString(), languageId: "squark", version: doc.version, text: doc.getText() },
    }));
  };
  vscode.workspace.textDocuments.forEach(open);
  context.subscriptions.push(
    vscode.workspace.onDidOpenTextDocument(open),
    vscode.workspace.onDidChangeTextDocument((e) => {
      if (!isSquark(e.document)) return;
      ready.then(() => client.notify("textDocument/didChange", {
        textDocument: { uri: e.document.uri.toString(), version: e.document.version },
        contentChanges: [{ text: e.document.getText() }],
      }));
    }),
    vscode.workspace.onDidCloseTextDocument((doc) => {
      if (!isSquark(doc)) return;
      ready.then(() => client.notify("textDocument/didClose", docParams(doc)));
    }),
    vscode.languages.registerHoverProvider("squark", {
      async provideHover(doc, pos) {
        await ready;
        const h = await client.request("textDocument/hover", posParams(doc, pos));
        return h ? new vscode.Hover(new vscode.MarkdownString(h.contents.value)) : null;
      },
    }),
    vscode.languages.registerDefinitionProvider("squark", {
      async provideDefinition(doc, pos) {
        await ready;
        const l = await client.request("textDocument/definition", posParams(doc, pos));
        return l ? new vscode.Location(vscode.Uri.parse(l.uri), toRange(l.range)) : null;
      },
    }),
    vscode.languages.registerDocumentFormattingEditProvider("squark", {
      async provideDocumentFormattingEdits(doc) {
        await ready;
        const edits = await client.request("textDocument/formatting", { ...docParams(doc), options: {} });
        return (edits || []).map((e) => vscode.TextEdit.replace(toRange(e.range), e.newText));
      },
    }),
  );
}

function deactivate() {
  return client ? client.stop() : undefined;
}

module.exports = { activate, deactivate };
