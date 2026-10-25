// tools.bk_core.get_all_clients: List every client.
// Usage (executeToolCode, language "javascript"):
//   const r = await tools.bk_core.get_all_clients({});
// A call resolves to the tool's structuredContent when it sends one, else to its
// text parsed as JSON, else to the text. A failed or held call rejects with a
// ToolCallError; tools.tryCall("bk_core.get_all_clients", args) resolves to
// { ok, status, result } or { ok: false, status, error } instead.
// Server "bk-core" is called bk_core in code.

/** List every client. */
export interface Input {}

// The tool declares no output schema.
export type Output = unknown;

export type Call = (args?: Input) => Promise<Output>;
