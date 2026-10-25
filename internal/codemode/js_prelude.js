// toolyard code mode: the JavaScript prelude (internal/codemode/js.go runs
// it in QuickJS with `qjs --std -e <this file>`).
//
// It speaks to the Go host over the module's stdin and stdout, one JSON
// object per line. The first line in is the boot record: the script and
// the servers it may call. Out go prints, tool calls and the outcome; in
// come the calls' answers, matched by id. A tool call returns a promise at
// once; the calls a script starts before it next awaits go out together,
// so tools.parallel (and any calls left un-awaited) really run in
// parallel, the host answering each as it finishes.
//
// The pump runs as a 0 ms timer, which QuickJS fires only once every
// promise reaction has run: by then the script has started every call it
// can. The pump sends them, then blocks reading one answer (there is
// nothing else to do until one comes), settles its promise and schedules
// itself again while calls are outstanding.
(function () {
  "use strict";
  const std = globalThis.std;
  const os = globalThis.os;
  const out = std.out;
  const input = std.in;
  const evalScript = std.evalScript;
  const setTimer = os.setTimeout;
  // The script has no use for the host modules; nothing in them reaches
  // past the WebAssembly sandbox, which mounts no files, has no network
  // and no environment, but scripts should not lean on them either.
  for (const name of ["std", "os", "bjson", "scriptArgs"]) {
    try {
      delete globalThis[name];
    } catch (e) {
      // not configurable: leave it
    }
  }

  const MAX_ARGS_CHARS = 4 * 1024 * 1024;
  const MAX_PRINT_CHARS = 1024 * 1024;
  const MAX_RESULT_CHARS = 2 * 1024 * 1024;

  function sendLine(line) {
    out.puts(line + "\n");
    out.flush();
  }
  function send(msg) {
    sendLine(JSON.stringify(msg));
  }

  const bootLine = input.getline();
  if (bootLine === null) {
    return; // the host went away before the start
  }
  const boot = JSON.parse(bootLine);
  const maxParallel = boot.maxParallel > 0 ? boot.maxParallel : 8;

  class ToolCallError extends Error {
    constructor(message, status, callId, approvalId) {
      super(message);
      this.name = "ToolCallError";
      this.status = status;
      this.callId = callId;
      if (approvalId) this.approvalId = approvalId;
    }
  }

  // ---- calls ----
  let nextId = 0;
  let outstanding = 0;
  let outbox = [];
  const pending = new Map(); // id -> settle(reply | null)
  let scheduled = false;
  let settled = null; // {ok, value} or {ok: false, error} once the script ends
  let finished = false;

  function schedule() {
    if (!scheduled) {
      scheduled = true;
      setTimer(pump, 0);
    }
  }

  function pump() {
    scheduled = false;
    try {
      if (outbox.length) {
        const batch = outbox;
        outbox = [];
        for (const line of batch) sendLine(line);
        outstanding += batch.length;
      }
      if (outstanding > 0) {
        const line = input.getline();
        if (line === null) {
          // The gateway went away, or the clock ran out.
          const waiting = Array.from(pending.values());
          pending.clear();
          outstanding = 0;
          for (const settle of waiting) settle(null);
        } else {
          const reply = JSON.parse(line);
          const settle = pending.get(reply.id);
          if (settle) {
            pending.delete(reply.id);
            outstanding--;
            settle(reply);
          }
        }
      }
    } catch (e) {
      settled = settled || { ok: false, error: e };
      outstanding = 0;
      outbox = [];
    }
    if (outbox.length || outstanding > 0) schedule();
    else if (settled) finish();
  }

  // request queues one call message and returns [id, promise of reply].
  function request(msg) {
    const id = ++nextId;
    msg.t = "call";
    msg.id = id;
    let line;
    try {
      line = JSON.stringify(msg);
    } catch (e) {
      return [id, Promise.reject(new TypeError("tool arguments cannot be sent as JSON: " + e.message))];
    }
    if (line.length > MAX_ARGS_CHARS) {
      return [id, Promise.reject(new RangeError("tool arguments are about " + line.length +
        " bytes; one call may pass at most 4 MiB. Pass less, or split the work across calls"))];
    }
    const p = new Promise((resolve) => pending.set(id, resolve));
    outbox.push(line);
    schedule();
    return [id, p];
  }

  // decode is a successful answer's value: the host sends JSON answers as
  // values (json) and anything else as text. Text that still parses (a
  // replayed answer) is parsed; only a SyntaxError means "not JSON".
  function decode(reply) {
    if (reply.json !== undefined) return reply.json;
    const text = reply.text === undefined ? "" : reply.text;
    try {
      return JSON.parse(text);
    } catch (e) {
      if (e instanceof SyntaxError) return text;
      throw e;
    }
  }

  function labelOf(target) {
    return target.name || target.server + "." + target.member;
  }

  // invoke makes one call. A plain call resolves to the result and
  // rejects with a ToolCallError; a try call always resolves, to
  // {ok, status, result} or {ok: false, status, error}.
  function invoke(target, args, isTry) {
    if (args === undefined || args === null) args = {};
    if (typeof args !== "object" || Array.isArray(args)) {
      const err = new TypeError("tool arguments must be an object: tools." + labelOf(target) + "({ param: value })");
      return isTry ? Promise.resolve({ ok: false, status: "failed", error: err.message }) : Promise.reject(err);
    }
    const msg = { a: args, try: !!isTry };
    if (target.name) msg.n = target.name;
    else {
      msg.s = target.server;
      msg.m = target.member;
    }
    const [id, sent] = request(msg);
    const p = sent.then((reply) => {
      if (reply === null) {
        const gone = "tool call failed for " + labelOf(target) + ": the gateway went away";
        if (isTry) return { ok: false, status: "failed", error: gone };
        throw new ToolCallError(gone, "failed", id);
      }
      if (isTry) {
        if (reply.ok) {
          try {
            return { ok: true, status: "succeeded", result: decode(reply) };
          } catch (e) {
            return { ok: false, status: "failed", error: String(e && e.message || e) };
          }
        }
        const res = { ok: false, status: reply.status || "failed", error: reply.error };
        if (reply.approvalId) res.approval_id = reply.approvalId;
        return res;
      }
      if (!reply.ok) throw new ToolCallError(reply.error, reply.status || "failed", id, reply.approvalId);
      return decode(reply);
    }, (e) => {
      if (isTry) return { ok: false, status: "failed", error: String(e && e.message || e) };
      throw e;
    });
    // A failed call the script never awaits is not an unhandled rejection;
    // a script that awaits it still gets the error.
    p.catch(() => {});
    return p;
  }

  function targetOf(fn, nameOrTool) {
    if (typeof nameOrTool === "function" && nameOrTool.toolTarget) return nameOrTool.toolTarget;
    if (typeof nameOrTool === "string" && nameOrTool.trim() !== "") return { name: nameOrTool.trim() };
    throw new TypeError(fn + ": name the tool as \"Server.tool\" or pass the function tools.Server.tool");
  }

  function makeTool(server, member) {
    const target = { server: server, member: member };
    const fn = function (args) {
      return invoke(target, args, false);
    };
    Object.defineProperty(fn, "name", { value: member });
    fn.toolTarget = target;
    return fn;
  }

  function refusing(msg) {
    return function () {
      const p = Promise.reject(new Error(msg));
      p.catch(() => {});
      return p;
    };
  }

  function parallel(fns, opts) {
    if (!Array.isArray(fns)) {
      return Promise.reject(new TypeError("tools.parallel takes an array of functions: tools.parallel([() => tools.A.x({}), () => tools.B.y({})], { limit: 4 })"));
    }
    let limit = 4;
    if (opts && opts.limit !== undefined) {
      limit = Math.floor(Number(opts.limit));
      if (!(limit >= 1)) return Promise.reject(new RangeError("tools.parallel: limit must be at least 1"));
    }
    limit = Math.min(limit, maxParallel);
    return new Promise((resolve, reject) => {
      const results = new Array(fns.length);
      let next = 0;
      let running = 0;
      let done = 0;
      let failed = false;
      if (fns.length === 0) {
        resolve(results);
        return;
      }
      const launch = () => {
        while (!failed && running < limit && next < fns.length) {
          const i = next++;
          running++;
          let p;
          try {
            if (typeof fns[i] !== "function") {
              throw new TypeError("tools.parallel: item " + i + " is not a function; wrap each call: () => tools.Server.tool({...})");
            }
            p = Promise.resolve(fns[i]());
          } catch (e) {
            p = Promise.reject(e);
          }
          p.then((v) => {
            results[i] = v;
            running--;
            done++;
            if (done === fns.length) resolve(results);
            else launch();
          }, (e) => {
            if (!failed) {
              failed = true;
              reject(e);
            }
          });
        }
      };
      launch();
    });
  }

  function search(query, opts) {
    const args = Object.assign({}, opts || {}, { query: query === undefined ? "" : String(query) });
    return invoke({ name: "toolyard.tools_search" }, args, false).then((r) =>
      r && Array.isArray(r.results) ? r.results : r);
  }

  const tools = {};
  for (const srv of boot.servers || []) {
    const ns = {};
    for (const m of srv.members || []) ns[m] = makeTool(srv.ident, m);
    for (const ident of Object.keys(srv.refused || {})) {
      if (!(ident in ns)) ns[ident] = refusing(srv.refused[ident]);
    }
    tools[srv.ident] = Object.freeze(ns);
  }
  // The helpers win over a server of the same name, which stays callable
  // by name: tools.call("call.tool", {...}).
  tools.call = (name, args) => {
    try {
      return invoke(targetOf("tools.call", name), args, false);
    } catch (e) {
      return Promise.reject(e);
    }
  };
  tools.tryCall = (name, args) => {
    try {
      return invoke(targetOf("tools.tryCall", name), args, true);
    } catch (e) {
      return Promise.resolve({ ok: false, status: "failed", error: e.message });
    }
  };
  tools.parallel = parallel;
  tools.search = search;
  Object.freeze(tools);

  // ---- console ----
  function fmt(v) {
    if (typeof v === "string") return v;
    if (v instanceof Error) return describe(v);
    if (v === undefined) return "undefined";
    if (typeof v === "function") return "[Function " + (v.name || "anonymous") + "]";
    if (typeof v === "bigint") return v.toString() + "n";
    if (typeof v === "symbol") return v.toString();
    try {
      const s = JSON.stringify(v);
      return s === undefined ? String(v) : s;
    } catch (e) {
      return String(v);
    }
  }
  function log(...args) {
    let text = args.map(fmt).join(" ");
    if (text.length > MAX_PRINT_CHARS) text = text.slice(0, MAX_PRINT_CHARS) + "… [print truncated]";
    send({ t: "print", text: text });
  }
  const console = Object.freeze({ log: log, info: log, warn: log, error: log, debug: log });
  globalThis.console = console;
  globalThis.print = log;
  globalThis.tools = tools;

  // ---- the script ----
  function describe(e) {
    if (e instanceof ToolCallError) return e.message;
    if (e instanceof Error) {
      const head = e.name + ": " + e.message;
      const stack = typeof e.stack === "string" ? e.stack.replace(/\s+$/, "") : "";
      return stack ? head + "\n" + stack : head;
    }
    if (e === null || e === undefined) {
      // What QuickJS throws when it runs out of memory building the error.
      return "Uncaught " + String(e) + " (when the script did not throw it, the JavaScript engine ran out of memory)";
    }
    try {
      return "Uncaught " + JSON.stringify(e);
    } catch (x) {
      return "Uncaught " + String(e);
    }
  }

  function finish() {
    if (finished) return;
    finished = true;
    const s = settled;
    if (!s.ok) {
      const msg = { t: "error", kind: "runtime", message: describe(s.error) };
      if (s.error instanceof ToolCallError) msg.call = s.error.callId;
      send(msg);
      return;
    }
    if (s.value === undefined) {
      send({ t: "done" });
      return;
    }
    let json;
    try {
      json = JSON.stringify(s.value);
    } catch (e) {
      send({ t: "error", kind: "runtime", message: "result cannot be encoded as JSON: " + e.message });
      return;
    }
    if (json === undefined) {
      send({ t: "done" }); // a function or a symbol
      return;
    }
    if (json.length > MAX_RESULT_CHARS) {
      send({ t: "error", kind: "runtime", cause: "limit", message: "result is about " + json.length +
        " bytes; a returned value may be at most 1 MiB (the response is truncated there anyway). Return less, or print a summary and return the key fields" });
      return;
    }
    sendLine('{"t":"done","has":true,"result":' + json + "}");
  }

  function onSettled(s) {
    if (settled) return;
    settled = s;
    if (!outbox.length && outstanding === 0) finish();
  }

  let main;
  try {
    // The script is the body of an async function, on the first line of
    // the source so error line numbers are the script's own.
    main = evalScript("(async function () {" + boot.code + "\n})", { backtrace_barrier: true });
  } catch (e) {
    send({ t: "error", kind: e instanceof SyntaxError ? "syntax" : "runtime", message: describe(e) });
    return;
  }
  let result;
  try {
    result = main();
  } catch (e) {
    result = Promise.reject(e);
  }
  Promise.resolve(result).then((v) => onSettled({ ok: true, value: v }), (e) => onSettled({ ok: false, error: e }));
})();
