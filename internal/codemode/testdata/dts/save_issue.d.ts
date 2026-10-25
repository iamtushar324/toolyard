// tools.Linear.save_issue: Create or update an issue.
// Usage (executeToolCode, language "javascript"):
//   const r = await tools.Linear.save_issue({ state: "todo" });
// A call resolves to the tool's structuredContent when it sends one, else to its
// text parsed as JSON, else to the text. A failed or held call rejects with a
// ToolCallError; tools.tryCall("Linear.save_issue", args) resolves to
// { ok, status, result } or { ok: false, status, error } instead.

/**
 * Create or update an issue.
 *
 * Pass id to update.
 */
export interface Input {
  state: "todo" | "in progress" | "done";
  /** Issue title */
  title: string;
  /** Who owns it */
  assignee?: {
    email: string;
    name?: string | null;
  };
  id?: string;
  labels?: string[];
  links?: {
    url: string;
    title?: string;
  }[];
  meta?: Record<string, number>;
  /**
   * 0 is none
   * @default 0
   */
  priority?: 0 | 1 | 2;
  "x-trace"?: boolean;
}

/** The saved issue. */
export interface Output {
  id: string;
  status: "created" | "updated";
  url?: string;
}

export type Call = (args: Input) => Promise<Output>;
