// A JSON-RPC client for a language server over a child process's stdin and stdout (the Language
// Server Protocol's framing: a Content-Length header, a blank line, the JSON). It has no
// dependencies, so it is tested with plain node (test/lsp.test.mjs).
"use strict";

const { spawn } = require("node:child_process");

class Client {
  // command and args start the server; onNotify(method, params) receives its notifications.
  constructor(command, args, onNotify, options = {}) {
    this.nextId = 1;
    this.waiting = new Map();
    this.onNotify = onNotify;
    this.buf = Buffer.alloc(0);
    this.proc = spawn(command, args, { stdio: ["pipe", "pipe", "pipe"], ...options });
    this.proc.stdout.on("data", (chunk) => this.receive(chunk));
    this.proc.stderr.on("data", () => {});
    this.exited = new Promise((resolve) => this.proc.on("exit", (code) => {
      for (const { reject } of this.waiting.values()) reject(new Error("squark lsp exited"));
      this.waiting.clear();
      resolve(code);
    }));
    this.proc.on("error", (err) => {
      for (const { reject } of this.waiting.values()) reject(err);
      this.waiting.clear();
    });
  }

  send(msg) {
    const body = Buffer.from(JSON.stringify({ jsonrpc: "2.0", ...msg }), "utf8");
    this.proc.stdin.write(`Content-Length: ${body.length}\r\n\r\n`);
    this.proc.stdin.write(body);
  }

  request(method, params) {
    const id = this.nextId++;
    return new Promise((resolve, reject) => {
      this.waiting.set(id, { resolve, reject });
      this.send({ id, method, params });
    });
  }

  notify(method, params) {
    this.send({ method, params });
  }

  receive(chunk) {
    this.buf = Buffer.concat([this.buf, chunk]);
    for (;;) {
      const end = this.buf.indexOf("\r\n\r\n");
      if (end < 0) return;
      const header = this.buf.subarray(0, end).toString("ascii");
      const m = /Content-Length:\s*(\d+)/i.exec(header);
      const length = m ? Number(m[1]) : 0;
      if (this.buf.length < end + 4 + length) return;
      const body = this.buf.subarray(end + 4, end + 4 + length).toString("utf8");
      this.buf = this.buf.subarray(end + 4 + length);
      let msg;
      try {
        msg = JSON.parse(body);
      } catch {
        continue;
      }
      if (msg.id !== undefined && msg.method === undefined) {
        const w = this.waiting.get(msg.id);
        if (!w) continue;
        this.waiting.delete(msg.id);
        if (msg.error) w.reject(new Error(msg.error.message));
        else w.resolve(msg.result);
      } else if (msg.method !== undefined && msg.id === undefined) {
        this.onNotify(msg.method, msg.params);
      } else if (msg.method !== undefined) {
        // a request from the server: none is needed, so answer that it isn't supported
        this.send({ id: msg.id, error: { code: -32601, message: "not supported" } });
      }
    }
  }

  // stop shuts the server down politely, then makes sure it is gone.
  async stop() {
    try {
      await Promise.race([this.request("shutdown", null), new Promise((r) => setTimeout(r, 2000))]);
      this.notify("exit", null);
    } catch {}
    const timer = setTimeout(() => this.proc.kill(), 2000);
    await this.exited;
    clearTimeout(timer);
  }
}

module.exports = { Client };
