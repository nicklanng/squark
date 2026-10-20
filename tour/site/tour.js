// A Tour of squark: the page. Lessons come from lessons.json; with squark tour behind the page,
// code is checked as you type (/api/check) and built and run on demand (/api/run). An exported
// copy has each lesson's results baked in instead.
"use strict";

const $ = (id) => document.getElementById(id);

const store = {
  get(k) { try { return localStorage.getItem("squark-tour:" + k); } catch { return null; } },
  set(k, v) { try { localStorage.setItem("squark-tour:" + k, v); } catch {} },
  del(k) { try { localStorage.removeItem("squark-tour:" + k); } catch {} },
};

// ---------- syntax highlighting ----------

const KEYWORDS = new Set(("and arena break case chan const continue default defer else enum extern fault fn for if in " +
  "not or pub return struct switch task var while impl program module use as thread shared start loop with select " +
  "deadline self").split(" "));
const DIRECTIVES = new Set("budget pool workers threads shutdown connections sockbuf heap link cflags ldflags stack blocking preemptible isolate static".split(" "));
const TYPES = new Set("bool str int i8 i16 i32 i64 uint u8 u16 u32 u64 uintptr f32 f64 rawptr error anytype rune".split(" "));
const CONSTS = new Set(["true", "false", "nil"]);
const TOKEN = /(\/\/[^\n]*)|(c?"(?:[^"\\\n]|\\.)*"?|`[^`]*`?)|('(?:[^'\\\n]|\\.)*'?)|(\b0x[0-9a-fA-F_]+\b|\b\d[\d_]*(?:\.\d+)?(?:[eE][+-]?\d+)?[KMG]?\b)|([A-Za-z_]\w*)|(->|\.\.|!|<-)/g;

function esc(s) {
  return s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

function highlight(src) {
  let out = "", last = 0;
  TOKEN.lastIndex = 0;
  for (let m; (m = TOKEN.exec(src));) {
    out += esc(src.slice(last, m.index));
    last = TOKEN.lastIndex;
    const t = m[0];
    let cls = null;
    if (m[1]) cls = "t-com";
    else if (m[2] || m[3]) cls = "t-str";
    else if (m[4]) cls = "t-num";
    else if (m[6]) cls = "t-op";
    else if (m[5]) {
      const next = src.slice(last).match(/^\s*(.)/);
      const nc = next ? next[1] : "";
      if (KEYWORDS.has(t)) cls = "t-kw";
      else if (CONSTS.has(t)) cls = "t-num";
      else if (TYPES.has(t) || /^[iuf]\d+x\d+$/.test(t) || /^[A-Z]/.test(t)) cls = "t-type";
      else if (DIRECTIVES.has(t) && !"(.:=,".includes(nc)) cls = "t-dir";
      else if (src[last] === "(") cls = "t-fn";
    }
    out += cls ? `<span class="${cls}">${esc(t)}</span>` : esc(t);
  }
  return out + esc(src.slice(last));
}

// ---------- sizes ----------

function human(n) {
  if (n < 1024) return n + " B";
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let v = n / 1024, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(1) + " " + units[i];
}

// ---------- state ----------

const state = {
  live: false,
  chapters: [],
  lessons: [], // flat, each with .chapter and .index
  cur: null,
  lastMem: new Map(), // lesson id -> the memory of its previous build, for deltas
  checkCtl: null,
  runCtl: null,
  checkTimer: 0,
};

const lessonId = (l) => l.chapter.slug + "/" + l.slug;

function progress() {
  try { return JSON.parse(store.get("progress") || "{}"); } catch { return {}; }
}
function mark(l, what) {
  const p = progress();
  const id = lessonId(l);
  if (p[id] === "done") return;
  if (what === "seen" && p[id]) return;
  p[id] = what;
  store.set("progress", JSON.stringify(p));
}

// ---------- editor ----------

const ta = $("code"), hl = $("hl"), gutter = $("gutter"), marks = $("marks");
let errLines = new Map(); // line -> message

function renderEditor() {
  const src = ta.value;
  hl.innerHTML = highlight(src) + "\n";
  const n = src.split("\n").length;
  let g = "";
  for (let i = 1; i <= n; i++) {
    const e = errLines.get(i);
    g += e ? `<div class="err" title="${esc(e)}">${i}</div>` : `<div>${i}</div>`;
  }
  gutter.innerHTML = g;
  let mk = "";
  for (const [line] of errLines) {
    if (line <= n) mk += `<div class="err" style="top:calc(12px + ${line - 1} * var(--lh))"></div>`;
  }
  marks.innerHTML = mk;
  syncScroll();
}

function syncScroll() {
  hl.scrollTop = ta.scrollTop;
  hl.scrollLeft = ta.scrollLeft;
  gutter.scrollTop = ta.scrollTop;
  marks.style.transform = `translate(0, ${-ta.scrollTop}px)`;
}

function setErrors(diags) {
  errLines = new Map();
  for (const d of diags || []) if (!errLines.has(d.line)) errLines.set(d.line, d.msg);
  renderEditor();
}

function insert(text) {
  ta.focus();
  // execCommand keeps the browser's undo history; setRangeText is the fallback
  if (!document.execCommand || !document.execCommand("insertText", false, text)) {
    ta.setRangeText(text, ta.selectionStart, ta.selectionEnd, "end");
    ta.dispatchEvent(new Event("input"));
  }
}

function lineStart(pos) { return ta.value.lastIndexOf("\n", pos - 1) + 1; }

// revealLine scrolls the editor so line is in view, without moving the caret.
function revealLine(line) {
  const lh = parseFloat(getComputedStyle(ta).lineHeight) || 21;
  const top = (line - 1) * lh;
  if (top < ta.scrollTop || top > ta.scrollTop + ta.clientHeight - 2 * lh) {
    ta.scrollTop = Math.max(0, top - ta.clientHeight / 3);
    syncScroll();
  }
}

function goToLine(line, col) {
  const lines = ta.value.split("\n");
  let pos = 0;
  for (let i = 0; i < line - 1 && i < lines.length; i++) pos += lines[i].length + 1;
  pos += Math.max(0, (col || 1) - 1);
  ta.focus();
  ta.setSelectionRange(pos, pos);
  revealLine(line);
  const f = document.createElement("div");
  f.className = "flash";
  f.style.top = `calc(12px + ${line - 1} * var(--lh))`;
  marks.appendChild(f);
  requestAnimationFrame(() => requestAnimationFrame(() => { f.style.background = "transparent"; }));
  setTimeout(() => f.remove(), 1200);
}

ta.addEventListener("scroll", syncScroll);
ta.addEventListener("input", () => {
  renderEditor();
  if (state.cur) store.set("code:" + lessonId(state.cur), ta.value);
  scheduleCheck();
});
ta.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && (e.shiftKey || e.metaKey || e.ctrlKey)) {
    e.preventDefault();
    run();
    return;
  }
  if (e.key === "Escape") { ta.blur(); return; }
  if (e.key === "Tab") {
    e.preventDefault();
    const { selectionStart: s, selectionEnd: end, value: v } = ta;
    if (s === end && !e.shiftKey) { insert("\t"); return; }
    // indent or outdent every selected line
    const from = lineStart(s);
    const block = v.slice(from, end);
    const changed = e.shiftKey ? block.replace(/^\t/gm, "") : block.replace(/^/gm, "\t");
    ta.setSelectionRange(from, end);
    insert(changed);
    ta.setSelectionRange(from, from + changed.length);
    return;
  }
  if (e.key === "Enter") {
    e.preventDefault();
    const s = ta.selectionStart;
    const line = ta.value.slice(lineStart(s), s);
    let indent = line.match(/^\s*/)[0];
    if (/[{(]\s*$/.test(line)) indent += "\t";
    insert("\n" + indent);
    return;
  }
  if (e.key === "}") {
    const s = ta.selectionStart;
    const before = ta.value.slice(lineStart(s), s);
    if (/^\t+$/.test(before) && s === ta.selectionEnd) {
      e.preventDefault();
      ta.setSelectionRange(s - 1, s);
      insert("}");
    }
  }
});

// ---------- live checking ----------

function scheduleCheck() {
  clearTimeout(state.checkTimer);
  if (!state.live) return;
  setStatus("", "");
  state.checkTimer = setTimeout(check, 350);
}

async function check() {
  if (state.checkCtl) state.checkCtl.abort();
  const ctl = (state.checkCtl = new AbortController());
  const lesson = state.cur;
  setStatus("checking…", "");
  try {
    const r = await api("/api/check", { code: ta.value }, ctl.signal);
    if (ctl.signal.aborted || lesson !== state.cur) return;
    setErrors(r.errors);
    if (r.stage === "compile") {
      const n = r.errors ? r.errors.length : 0;
      const first = r.errors && r.errors[0];
      setStatus(first ? `✗ ${n > 1 ? n + " errors · " : ""}line ${first.line}: ${first.msg}` : "✗ " + r.output.split("\n")[0], "bad");
    } else {
      setStatus("✓ types and lifetimes check", "ok");
    }
    // the compiler saying no is a run, as far as a "rejects" goal is concerned
    if (lesson.goal && lesson.goal.kind === "rejects") judge(lesson, r);
  } catch (err) {
    if (err.name !== "AbortError") setStatus("can't reach squark tour", "bad");
  }
}

function setStatus(text, cls) {
  const s = $("check-status");
  s.textContent = text;
  s.title = text;
  s.className = "check-status " + cls;
}

// squark tour opens the page with ?k=KEY; the server runs code only for requests that carry it
const tourKey = new URLSearchParams(location.search).get("k") || "";

async function api(path, body, signal) {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Squark-Tour": tourKey },
    body: JSON.stringify(body),
    signal,
  });
  if (!res.ok) throw new Error((await res.text()) || res.statusText);
  return res.json();
}

// ---------- running ----------

const norm = (s) => s.replace(/\s+$/gm, "").trim();

async function run() {
  const lesson = state.cur;
  if (!lesson) return;
  const btn = $("run-btn");
  if (!state.live) {
    let r = null;
    if (norm(ta.value) === norm(lesson.code)) r = lesson.baked;
    else if (lesson.solution && norm(ta.value) === norm(lesson.solution)) r = lesson.bakedSolution;
    if (r) { showResult(lesson, r); return; }
    showNotice("This copy of the tour shows the results of the lesson's own code (and its solution). " +
      "To run code you've changed, install squark and run <code>squark tour</code>: the same lessons, with the compiler behind them.");
    return;
  }
  if (state.runCtl) state.runCtl.abort();
  const ctl = (state.runCtl = new AbortController());
  btn.classList.add("busy");
  $("run-label").textContent = lesson.mode === "build" ? "Building" : "Running";
  $("console-meta").textContent = "compiling with clang…";
  try {
    const r = await api("/api/run", { code: ta.value, mode: lesson.mode }, ctl.signal);
    if (lesson === state.cur) showResult(lesson, r);
  } catch (err) {
    if (err.name !== "AbortError") showNotice("Couldn't run it: " + esc(err.message) + ". Is <code>squark tour</code> still running?");
  } finally {
    if (state.runCtl === ctl) {
      btn.classList.remove("busy");
      $("run-label").textContent = lesson.mode === "build" ? "Build" : "Run";
    }
  }
}

function showNotice(html) {
  $("console-title").textContent = "Output";
  $("console-meta").textContent = "";
  $("console-body").innerHTML = `<p class="notice">${html}</p>`;
}

function showResult(lesson, r) {
  setErrors(r.errors);
  const body = $("console-body");
  const meta = [];
  if (r.buildMs) meta.push(r.buildMs < 1000 ? `built in ${r.buildMs} ms` : `built in ${(r.buildMs / 1000).toFixed(1)} s`);
  if (r.stage === "run") meta.push(`ran in ${r.runMs} ms`);
  $("console-meta").textContent = meta.join(" · ");
  let html = "";
  const met = judge(lesson, r);
  if (lesson.goal) {
    html += met
      ? `<p class="verdict ok">✓ Goal met</p>`
      : `<p class="verdict bad">Not yet: ${goalHint(lesson.goal, r)}</p>`;
  }
  if (r.stage === "compile") {
    $("console-title").textContent = r.memory ? "Over budget" : "Compiler";
    if (r.errors && r.errors.length) {
      html += `<ul class="diag-list">` + r.errors.map((d, i) =>
        `<li><button class="diag" data-i="${i}"><span class="pos">${d.line}:${d.col}</span>${esc(d.msg)}</button></li>`).join("") + `</ul>`;
      const rest = r.output.split("\n").filter((l) => !/^main\.sq:\d+:\d+: /.test(l)).join("\n").trim();
      if (rest) html += `<pre>${esc(rest)}</pre>`;
    } else if (r.memory) {
      // the breakdown is in the memory map above: keep the verdict and the advice
      const lines = r.output.split("\n");
      html += `<pre>${esc([lines[0], ...lines.filter((l) => l.startsWith("to fit"))].join("\n\n"))}</pre>`;
    } else {
      html += `<pre>${esc(r.output)}</pre>`;
    }
    if (r.errors && r.errors.length) revealLine(r.errors[0].line);
    setStatus(r.memory ? "✗ over the memory budget" : `✗ the compiler said no`, "bad");
  } else {
    $("console-title").textContent = r.stage === "built" ? "Build" : "Output";
    if (r.stage === "built") {
      html += `<p class="notice">Built, not run: this program would keep running (a server waits for connections). The memory map shows what it can ever use.</p>`;
      if (r.output) html += `<pre>${esc(r.output)}</pre>`;
    } else {
      html += r.output ? `<pre>${esc(r.output)}</pre>` : `<p class="console-hint">(no output)</p>`;
      const expected = r.exit === (lesson.exit || 0);
      let exit = r.timedOut ? "stopped after 10 s" : `exit ${r.exit}`;
      if (r.exit === 2 && !r.timedOut) exit += " (panic)";
      html += `<div class="exit${expected && !r.timedOut ? "" : " bad"}">${exit}</div>`;
    }
    if (state.live) setStatus("✓ types and lifetimes check", "ok");
  }
  body.innerHTML = html;
  body.querySelectorAll(".diag").forEach((b) => b.addEventListener("click", () => {
    const d = r.errors[+b.dataset.i];
    goToLine(d.line, d.col);
  }));
  showMemory(lesson, r.memory);
  if (!lesson.goal && (r.stage === "run" || r.stage === "built") && r.exit === (lesson.exit || 0)) {
    mark(lesson, "done");
  }
}

function goalMet(g, r) {
  switch (g.kind) {
    case "rejects":
      return r.stage === "compile" && ((r.errors || []).some((d) => d.msg.includes(g.text)) || r.output.includes(g.text));
    case "prints":
      return r.stage === "run" && r.exit === 0 && !r.timedOut && r.output.includes(g.text);
    case "builds":
      return r.stage === "run" || r.stage === "built";
  }
  return false;
}

function goalHint(g, r) {
  switch (g.kind) {
    case "rejects":
      return r.stage === "compile" ? `the compiler refused, but not with “${esc(g.text)}”.` : "the compiler accepted it.";
    case "prints":
      if (r.stage === "compile") return "it doesn't compile.";
      if (r.timedOut) return "it was still running after 10 seconds.";
      if (r.exit !== 0) return `it ended with exit status ${r.exit}.`;
      return `the output doesn't contain “${esc(g.text)}”.`;
    case "builds":
      return r.memory ? "the worst case is still over the budget." : "it doesn't build yet.";
  }
  return "";
}

function judge(lesson, r) {
  if (!lesson.goal) return false;
  const met = goalMet(lesson.goal, r);
  if (met) {
    mark(lesson, "done");
    $("goal").classList.add("met");
    $("goal-state").textContent = "done ✓";
  }
  return met;
}

// ---------- memory map ----------

const KIND_COLOR = { code: "--m-code", static: "--m-static", stack: "--m-stack", helpers: "--m-helpers", sockets: "--m-sockets", heap: "--m-heap" };

function rowDetail(row, mem) {
  switch (row.kind) {
    case "code": return "machine code and read-only data: constants, faults, string literals";
    case "static": return "the runtime's tables (about 96 KiB: output buffer, poller, timers, signal stack), then package variables, package arenas, channel buffers";
    case "stack": return `${row.count.toLocaleString()} × ${human(row.each)} stack (proven ${row.proven.toLocaleString()} B)`;
    case "helpers": return `${row.count} × ${human(row.each)}: threads that run blocking C calls`;
    case "sockets": return "kernel socket buffers";
    case "heap": return "the C library's heap limit";
  }
  return "";
}

function showMemory(lesson, mem) {
  const id = lessonId(lesson);
  const prev = state.lastMem.get(id);
  const has = !!mem;
  $("mem-empty").hidden = has;
  $("mem-bar-wrap").hidden = !has;
  $("mem-rows").hidden = !has;
  $("report-btn").hidden = !has;
  if (!has) {
    $("mem-total").textContent = "";
    $("mem-report").hidden = true;
    return;
  }
  state.lastMem.set(id, mem);
  const threads = mem.threads || 1, workers = mem.workers || 1;
  const times = (row) => row.kind === "code" ? 1 : (row.kind === "sockets" || row.kind === "heap") ? workers : threads * workers;
  const sum = mem.rows.reduce((a, r) => a + r.bytes * times(r), 0);
  const total = Math.max(sum, mem.worst, mem.budget || 0);
  const over = mem.budget > 0 && mem.worst > mem.budget;

  $("mem-total").innerHTML = mem.budget
    ? `worst case <b class="${over ? "over" : ""}">${human(mem.worst)}</b> ${over ? "over" : "of"} the ${human(mem.budget)} budget`
    : `worst case <b>${human(mem.worst)}</b>${mem.threads === 0 ? " per core" : ""}`;

  const bar = $("mem-bar");
  bar.innerHTML = "";
  let stackN = 0;
  mem.rows.forEach((row, i) => {
    const seg = document.createElement("div");
    seg.className = "mem-seg";
    const color = row.kind === "stack" ? (stackN++ % 2 ? "--m-stack-2" : "--m-stack") : KIND_COLOR[row.kind];
    row._color = color;
    seg.style.background = `var(${color})`;
    seg.style.flex = `${(row.bytes * times(row)) / total} 0 0`;
    if (row.kind === "stack" && row.count > 1 && row.count <= 64) {
      seg.classList.add("slots");
      seg.style.setProperty("--slot", `calc(100% / ${row.count})`);
    }
    seg.title = `${row.name}: ${human(row.bytes)}`;
    seg.dataset.i = i;
    bar.appendChild(seg);
  });
  if (total > sum) {
    const free = document.createElement("div");
    free.className = "mem-seg";
    free.style.flex = `${(total - sum) / total} 0 0`;
    free.style.background = "transparent";
    free.title = "budget left";
    bar.appendChild(free);
  }
  const b = $("mem-budget");
  $("mem-bar-wrap").classList.toggle("has-budget", mem.budget > 0);
  if (mem.budget > 0) {
    b.hidden = false;
    const at = (mem.budget / total) * 100;
    b.style.left = `${at}%`;
    b.classList.toggle("edge", at > 85); // keep the label inside the bar's width
    b.classList.toggle("over", over);
    b.firstElementChild.textContent = "budget " + human(mem.budget);
  } else {
    b.hidden = true;
  }

  const prevRows = new Map((prev ? prev.rows : []).map((r) => [r.name, r.bytes]));
  let rows = "";
  mem.rows.forEach((row, i) => {
    let delta = "";
    if (prev && prevRows.has(row.name) && prevRows.get(row.name) !== row.bytes) {
      const d = row.bytes - prevRows.get(row.name);
      delta = `<span class="delta ${d > 0 ? "up" : "down"}">${d > 0 ? "+" : "−"}${human(Math.abs(d))}</span>`;
    } else if (prev && !prevRows.has(row.name)) {
      delta = `<span class="delta up">new</span>`;
    }
    const mult = times(row) > 1 ? ` <span class="console-meta">× ${times(row)}</span>` : "";
    rows += `<tr data-i="${i}"><td class="sw"><i style="background:var(${row._color})"></i></td>` +
      `<td class="name">${esc(row.name)}</td><td class="size">${human(row.bytes)}${mult}${delta}</td>` +
      `<td class="detail">${esc(rowDetail(row, mem))}</td></tr>`;
  });
  for (const lib of mem.uncounted || []) {
    rows += `<tr class="uncounted"><td class="sw"></td><td class="name">C library ${esc(lib)}</td><td class="size">not counted</td><td class="detail">it manages its own memory (heap ${esc(lib)} N would count it)</td></tr>`;
  }
  if (threads > 1 || workers > 1) {
    rows += `<tr class="uncounted"><td class="sw"></td><td class="name">${threads > 1 ? threads + " threads" : ""}${workers > 1 ? " " + workers + " workers" : ""}</td><td class="size"></td><td class="detail">static data and stacks are per scheduler thread, so they count more than once</td></tr>`;
  }
  const table = $("mem-rows");
  table.innerHTML = rows;
  const hot = (i, on) => {
    bar.querySelector(`[data-i="${i}"]`)?.classList.toggle("hot", on);
    table.querySelector(`tr[data-i="${i}"]`)?.classList.toggle("hot", on);
  };
  for (const el of [...bar.children, ...table.querySelectorAll("tr[data-i]")]) {
    if (el.dataset.i === undefined) continue;
    el.addEventListener("mouseenter", () => hot(el.dataset.i, true));
    el.addEventListener("mouseleave", () => hot(el.dataset.i, false));
  }
  $("mem-report").textContent = mem.report;
}

$("report-btn").addEventListener("click", () => {
  const r = $("mem-report");
  r.hidden = !r.hidden;
  $("report-btn").setAttribute("aria-expanded", String(!r.hidden));
});

// ---------- lessons ----------

function goalCopy(g) {
  switch (g.kind) {
    case "rejects": return ["Make it fail", `Change the program so the compiler rejects it with <code>${esc(g.text)}</code>.`];
    case "prints": return ["Make it work", `Make the program compile and print <code>${esc(g.text)}</code>.`];
    case "builds": return ["Make it fit", "Make the program build: its worst case has to fit within its budget."];
  }
  return ["Goal", ""];
}

function show(lesson) {
  if (state.cur && state.cur !== lesson) {
    if (state.runCtl) state.runCtl.abort();
    if (state.checkCtl) state.checkCtl.abort();
  }
  state.cur = lesson;
  mark(lesson, "seen");
  const ch = lesson.chapter;
  const chNum = state.chapters.indexOf(ch) + 1;
  const lNum = ch.lessons.indexOf(lesson) + 1;
  document.title = `${lesson.title} · A Tour of squark`;
  $("crumb-chapter").textContent = ch.title;
  $("crumb-lesson").textContent = lesson.title;
  $("crumb-count").textContent = `${lesson.index + 1}/${state.lessons.length}`;
  $("lesson-num").textContent = `${String(chNum).padStart(2, "0")}.${lNum} · ${ch.title}`;
  $("lesson-title").textContent = lesson.title;
  const body = $("lesson-body");
  body.innerHTML = (lNum === 1 && ch.intro ? `<div class="ch-intro">${ch.intro}</div>` : "") + lesson.body;
  body.querySelectorAll('pre.snippet[data-lang="squark"] code').forEach((c) => { c.innerHTML = highlight(c.textContent); });

  const goal = $("goal");
  if (lesson.goal) {
    const [kind, text] = goalCopy(lesson.goal);
    goal.hidden = false;
    $("goal-kind").textContent = kind;
    $("goal-text").innerHTML = text;
    const done = progress()[lessonId(lesson)] === "done";
    goal.classList.toggle("met", done);
    $("goal-state").textContent = done ? "done ✓" : "not yet";
  } else {
    goal.hidden = true;
  }

  const prev = state.lessons[lesson.index - 1], next = state.lessons[lesson.index + 1];
  $("prev-btn").disabled = !prev;
  $("prev-label").textContent = prev ? prev.title : "Back";
  $("next-btn").disabled = !next;
  $("next-label").textContent = next ? next.title : "The end";

  $("solution-btn").hidden = !lesson.solution;
  $("run-label").textContent = lesson.mode === "build" ? "Build" : "Run";
  $("run-btn").classList.remove("busy");
  ta.value = store.get("code:" + lessonId(lesson)) ?? lesson.code;
  ta.scrollTop = 0;
  ta.scrollLeft = 0;
  errLines = new Map();
  renderEditor();
  $("console-title").textContent = "Output";
  $("console-meta").textContent = "";
  $("console-body").innerHTML = `<p class="console-hint">${lesson.mode === "build"
    ? "Press Build: this program is built, not run, and its memory map shown."
    : "Press Run (or Shift+Enter in the editor)."}</p>`;
  $("mem-report").hidden = true;
  $("report-btn").setAttribute("aria-expanded", "false");
  showMemory(lesson, null);
  $("lesson").querySelector(".lesson-scroll").scrollTop = 0;
  if (state.live) { setStatus("", ""); check(); }
  else setStatus("read-only copy: results are baked in", "");
  renderToc();
}

function route() {
  const m = location.hash.match(/^#\/([^/]+)\/([^/]+)/);
  let l = m && state.lessons.find((x) => x.chapter.slug === m[1] && x.slug === m[2]);
  if (!l) l = state.lessons[0];
  show(l);
}

const go = (l) => { if (l) location.hash = `#/${l.chapter.slug}/${l.slug}`; };

$("prev-btn").addEventListener("click", () => go(state.lessons[state.cur.index - 1]));
$("next-btn").addEventListener("click", () => go(state.lessons[state.cur.index + 1]));
$("run-btn").addEventListener("click", run);
$("reset-btn").addEventListener("click", () => {
  if (!state.cur) return;
  store.del("code:" + lessonId(state.cur));
  ta.value = state.cur.code;
  errLines = new Map();
  renderEditor();
  scheduleCheck();
});
$("solution-btn").addEventListener("click", () => {
  if (!state.cur || !state.cur.solution) return;
  ta.value = state.cur.solution;
  store.set("code:" + lessonId(state.cur), ta.value);
  errLines = new Map();
  renderEditor();
  scheduleCheck();
});
window.addEventListener("hashchange", route);
document.addEventListener("keydown", (e) => {
  if (e.target === ta || e.altKey || e.metaKey || e.ctrlKey || !$("contents").hidden) {
    if (e.key === "Escape" && !$("contents").hidden) closeContents();
    return;
  }
  if (e.key === "ArrowRight") go(state.lessons[state.cur.index + 1]);
  if (e.key === "ArrowLeft") go(state.lessons[state.cur.index - 1]);
});

// ---------- contents ----------

function renderToc() {
  const p = progress();
  let done = 0;
  $("toc").innerHTML = state.chapters.map((ch, ci) => `<li><div class="ch-title"><span>${String(ci + 1).padStart(2, "0")}</span>${esc(ch.title)}</div><ol>` +
    ch.lessons.map((l) => {
      const st = p[lessonId(l)] || "";
      if (st === "done") done++;
      const tag = l.goal ? `<span class="tag">${l.goal.kind === "rejects" ? "make it fail" : l.goal.kind === "builds" ? "make it fit" : "make it work"}</span>` : "";
      return `<li class="${st}"><a href="#/${ch.slug}/${l.slug}"${l === state.cur ? ' aria-current="page"' : ""}><span class="dot"></span>${esc(l.title)}${tag}</a></li>`;
    }).join("") + `</ol></li>`).join("");
  $("contents-progress").textContent = `${done} of ${state.lessons.length} done`;
}

function openContents() {
  renderToc();
  $("contents").hidden = false;
  $("contents-backdrop").hidden = false;
  $("contents").querySelector('[aria-current="page"]')?.scrollIntoView({ block: "center" });
  $("contents-close").focus();
}
function closeContents() {
  $("contents").hidden = true;
  $("contents-backdrop").hidden = true;
  $("contents-btn").focus();
}
$("contents-btn").addEventListener("click", openContents);
$("contents-close").addEventListener("click", closeContents);
$("contents-backdrop").addEventListener("click", closeContents);
$("toc").addEventListener("click", (e) => { if (e.target.closest("a")) closeContents(); });

// ---------- theme ----------

function applyTheme(t) {
  if (t) document.documentElement.dataset.theme = t;
  else delete document.documentElement.dataset.theme;
}
applyTheme(store.get("theme"));
$("theme-btn").addEventListener("click", () => {
  const dark = document.documentElement.dataset.theme
    ? document.documentElement.dataset.theme === "dark"
    : matchMedia("(prefers-color-scheme: dark)").matches;
  const t = dark ? "light" : "dark";
  store.set("theme", t);
  applyTheme(t);
});

// ---------- start ----------

(async function start() {
  try {
    const res = await fetch("lessons.json");
    const data = await res.json();
    state.live = data.live;
    state.chapters = data.chapters;
    for (const ch of data.chapters) for (const l of ch.lessons) {
      l.chapter = ch;
      l.index = state.lessons.length;
      state.lessons.push(l);
    }
    const pill = $("mode-pill");
    pill.hidden = false;
    pill.textContent = state.live ? "compiler: local" : "read-only copy";
    pill.title = state.live ? "squark tour is compiling and running your code on this machine" : "results were baked in when this copy was made; run squark tour to edit and run code";
    route();
  } catch (err) {
    $("lesson-title").textContent = "The tour couldn't load its lessons";
    $("lesson-body").innerHTML = `<p>${esc(String(err))}</p>`;
  }
})();
