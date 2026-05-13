package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/lake"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
)

// lakeUpstream is the synthetic upstream we tag lake.* tools with so the
// catalog can group them and the dashboard can list them under one header.
const lakeUpstream = "lake"

// allowAction is the package-level *policy.Action handed to forcedAction on
// tools that should bypass the policy heuristic and auto-allow. We point at
// a single shared instance so every tool registration shares it (no extra
// allocations, easier to assert against in tests).
var (
	actionAllow = policy.ActionAllow
)

// lakeTools returns the ten lake.* MCP tools backed by g.lake. Approval
// matrix:
//
//	free (forcedAction=Allow): query, list_tables, describe_table,
//	                           insert, create_table, ingest
//	approval (default policy): update, delete, alter, drop
//
// The four approval-gated tools deliberately have no forcedAction — they
// fall through to policy.Eval, where the name heuristic already classifies
// them as writes and the engine returns ActionApprove.
func (g *Gateway) lakeTools() []toolEntry {
	if g.lake == nil {
		return nil
	}
	lk := g.lake

	out := []toolEntry{}
	out = append(out, lakeEntry(
		"lake.query",
		"Run a read-only SQL query against the personal data lake (DuckDB). Use for analytics: aggregations, time-series, joins. Provide standard SQL. Wrap exploratory `SELECT *` with LIMIT — results are capped at 1000 rows by default and truncated past 2 MiB.",
		mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "sql"},
			Properties: addMetaProps(map[string]any{
				"sql":      map[string]any{"type": "string", "description": "Read-only SQL (SELECT, WITH, SHOW, PRAGMA)."},
				"params":   map[string]any{"type": "array", "items": map[string]any{}, "description": "Positional bind parameters (?)"},
				"max_rows": map[string]any{"type": "integer", "minimum": 1, "maximum": 10000, "description": "Hard cap. Default 1000, max 10000."},
			}),
		},
		&actionAllow,
		handleLakeQuery(lk),
	))
	out = append(out, lakeEntry(
		"lake.list_tables",
		"List every table and view in the lake. Optional schema filter ('raw', 'mart', 'app').",
		mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField},
			Properties: addMetaProps(map[string]any{
				"schema": map[string]any{"type": "string"},
			}),
		},
		&actionAllow,
		handleLakeListTables(lk),
	))
	out = append(out, lakeEntry(
		"lake.describe_table",
		"Return column metadata + row count for a single table. Pass `schema.name` (e.g. `mart.bank_accounts`).",
		mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "table"},
			Properties: addMetaProps(map[string]any{
				"table": map[string]any{"type": "string"},
			}),
		},
		&actionAllow,
		handleLakeDescribeTable(lk),
	))
	out = append(out, lakeEntry(
		"lake.insert",
		"Append rows to a lake table. NO approval — additive operations on the lake do not need human review. Provide either `rows` (array of objects, server builds the INSERT) or `sql` (raw INSERT statement, parser-validated). Use this when accumulating a new observation: a holding snapshot, a workout entry, a transaction.",
		mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "table"},
			Properties: addMetaProps(map[string]any{
				"table":  map[string]any{"type": "string", "description": "schema.table"},
				"rows":   map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "Structured rows. All rows must share the same column set."},
				"sql":    map[string]any{"type": "string", "description": "Raw INSERT (server validates statement type)."},
				"params": map[string]any{"type": "array", "items": map[string]any{}},
			}),
		},
		&actionAllow,
		handleLakeInsert(lk),
	))
	out = append(out, lakeEntry(
		"lake.create_table",
		"Create a new table, view, or index. NO approval — schema growth is additive. Pass raw DDL via `sql`. Use CREATE OR REPLACE freely; DROP/ALTER are NOT permitted here (they have their own approval-gated tools).",
		mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "sql"},
			Properties: addMetaProps(map[string]any{
				"sql": map[string]any{"type": "string", "description": "CREATE TABLE / CREATE VIEW / CREATE INDEX / CREATE SCHEMA."},
			}),
		},
		&actionAllow,
		handleLakeCreateTable(lk),
	))
	out = append(out, lakeEntry(
		"lake.ingest",
		"Bulk-load data from a CSV / Parquet / JSON / JSONL / SQLite source into a target table. NO approval — additive. For SQLite, pass `source_uri` (path to the .db file) and `sqlite_table`.",
		mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "source_type", "source_uri", "target_table"},
			Properties: addMetaProps(map[string]any{
				"source_type":  map[string]any{"type": "string", "enum": []string{"csv", "json", "jsonl", "parquet", "sqlite", "http"}},
				"source_uri":   map[string]any{"type": "string"},
				"target_table": map[string]any{"type": "string", "description": "schema.table"},
				"mode":         map[string]any{"type": "string", "enum": []string{"append", "replace"}, "description": "Default append."},
				"options":      map[string]any{"type": "object"},
				"sqlite_table": map[string]any{"type": "string", "description": "Required when source_type=sqlite."},
			}),
		},
		&actionAllow,
		handleLakeIngest(lk),
	))

	// Approval-gated tools below: forcedAction is nil so policy.Eval applies.
	// The name heuristic ("update"/"delete"/"alter"/"drop") classifies each
	// as a write, so the engine returns ActionApprove and the gateway holds
	// the call until the human decides on their phone.
	out = append(out, lakeEntry(
		"lake.update",
		"Modify existing rows. APPROVAL REQUIRED. WHERE is mandatory; an unbounded UPDATE is rejected.",
		mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "table", "set", "where"},
			Properties: addMetaProps(map[string]any{
				"table":  map[string]any{"type": "string"},
				"set":    map[string]any{"type": "object"},
				"where":  map[string]any{"type": "string"},
				"params": map[string]any{"type": "array", "items": map[string]any{}},
			}),
		},
		nil,
		handleLakeUpdate(lk),
	))
	out = append(out, lakeEntry(
		"lake.delete",
		"Delete rows. APPROVAL REQUIRED. WHERE is mandatory.",
		mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "table", "where"},
			Properties: addMetaProps(map[string]any{
				"table":  map[string]any{"type": "string"},
				"where":  map[string]any{"type": "string"},
				"params": map[string]any{"type": "array", "items": map[string]any{}},
			}),
		},
		nil,
		handleLakeDelete(lk),
	))
	out = append(out, lakeEntry(
		"lake.alter",
		"ALTER TABLE / VIEW. APPROVAL REQUIRED. Pass full ALTER SQL via `sql`.",
		mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "sql"},
			Properties: addMetaProps(map[string]any{
				"sql": map[string]any{"type": "string"},
			}),
		},
		nil,
		handleLakeAlter(lk),
	))
	out = append(out, lakeEntry(
		"lake.drop",
		"DROP a table/view/index/schema. APPROVAL REQUIRED. Server builds the DROP — no raw SQL.",
		mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "object", "kind"},
			Properties: addMetaProps(map[string]any{
				"object":  map[string]any{"type": "string"},
				"kind":    map[string]any{"type": "string", "enum": []string{"table", "view", "index", "schema"}},
				"cascade": map[string]any{"type": "boolean"},
			}),
		},
		nil,
		handleLakeDrop(lk),
	))
	return out
}

func lakeEntry(name, desc string, schema mcp.ToolInputSchema, forced *policy.Action, h directHandler) toolEntry {
	tool := mcp.Tool{
		Name:        name,
		Description: descriptionBanner + desc,
		InputSchema: schema,
	}
	short := strings.TrimPrefix(name, "lake.")
	return toolEntry{
		tool:         tool,
		upstream:     lakeUpstream,
		originalName: short,
		reasonField:  ReasonField,
		handle:       h,
		forcedAction: forced,
	}
}

// ---- handlers ----------------------------------------------------------------

func handleLakeQuery(lk *lake.Service) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		sqlStr, _ := args["sql"].(string)
		if strings.TrimSpace(sqlStr) == "" {
			return mcp.NewToolResultError("sql is required"), nil
		}
		opts := lake.QueryOpts{}
		if maxRows, ok := asInt(args["max_rows"]); ok {
			opts.MaxRows = maxRows
		}
		if p, ok := args["params"].([]any); ok {
			opts.Params = p
		}
		res, err := lk.Query(ctx, sqlStr, opts)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("lake.query", err), nil
		}
		return jsonResult(res)
	}
}

func handleLakeListTables(lk *lake.Service) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		schema, _ := args["schema"].(string)
		tabs, err := lk.ListTables(ctx, schema)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("lake.list_tables", err), nil
		}
		return jsonResult(tabs)
	}
}

func handleLakeDescribeTable(lk *lake.Service) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		tbl, _ := args["table"].(string)
		if strings.TrimSpace(tbl) == "" {
			return mcp.NewToolResultError("table is required"), nil
		}
		desc, err := lk.DescribeTable(ctx, tbl)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("lake.describe_table", err), nil
		}
		return jsonResult(desc)
	}
}

func handleLakeInsert(lk *lake.Service) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		tbl, _ := args["table"].(string)
		if strings.TrimSpace(tbl) == "" {
			return mcp.NewToolResultError("table is required"), nil
		}
		// Two modes: structured rows OR raw INSERT SQL.
		if rows, ok := args["rows"].([]any); ok && len(rows) > 0 {
			rowMaps := make([]map[string]any, 0, len(rows))
			for i, r := range rows {
				m, ok := r.(map[string]any)
				if !ok {
					return mcp.NewToolResultErrorf("row %d is not an object", i), nil
				}
				rowMaps = append(rowMaps, m)
			}
			res, err := lk.InsertRows(ctx, tbl, rowMaps)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("lake.insert rows", err), nil
			}
			return jsonResult(res)
		}
		if sqlStr, ok := args["sql"].(string); ok && strings.TrimSpace(sqlStr) != "" {
			if err := lake.ValidateInsert(sqlStr); err != nil {
				return mcp.NewToolResultErrorFromErr("lake.insert sql", err), nil
			}
			var params []any
			if p, ok := args["params"].([]any); ok {
				params = p
			}
			res, err := lk.Exec(ctx, sqlStr, params...)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("lake.insert exec", err), nil
			}
			return jsonResult(res)
		}
		return mcp.NewToolResultError("provide either `rows` or `sql`"), nil
	}
}

func handleLakeCreateTable(lk *lake.Service) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		sqlStr, _ := args["sql"].(string)
		if err := lake.ValidateCreate(sqlStr); err != nil {
			return mcp.NewToolResultErrorFromErr("lake.create_table", err), nil
		}
		res, err := lk.Exec(ctx, sqlStr)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("lake.create_table exec", err), nil
		}
		return jsonResult(res)
	}
}

func handleLakeIngest(lk *lake.Service) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		req := lake.IngestRequest{
			Type:        lake.IngestSource(stringArg(args, "source_type")),
			URI:         stringArg(args, "source_uri"),
			TargetTable: stringArg(args, "target_table"),
			Mode:        lake.IngestMode(stringArg(args, "mode")),
			SQLiteTable: stringArg(args, "sqlite_table"),
		}
		if opts, ok := args["options"].(map[string]any); ok {
			req.Options = opts
		}
		if err := validateIngestPath(req.URI); err != nil {
			return mcp.NewToolResultErrorFromErr("lake.ingest sandbox", err), nil
		}
		report, err := lk.Ingest(ctx, req)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("lake.ingest", err), nil
		}
		return jsonResult(report)
	}
}

func handleLakeUpdate(lk *lake.Service) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		tbl := stringArg(args, "table")
		setMap, _ := args["set"].(map[string]any)
		where := stringArg(args, "where")
		var params []any
		if p, ok := args["params"].([]any); ok {
			params = p
		}
		res, err := lk.UpdateRows(ctx, tbl, setMap, where, params)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("lake.update", err), nil
		}
		return jsonResult(res)
	}
}

func handleLakeDelete(lk *lake.Service) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		tbl := stringArg(args, "table")
		where := stringArg(args, "where")
		var params []any
		if p, ok := args["params"].([]any); ok {
			params = p
		}
		res, err := lk.DeleteRows(ctx, tbl, where, params)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("lake.delete", err), nil
		}
		return jsonResult(res)
	}
}

func handleLakeAlter(lk *lake.Service) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		sqlStr := stringArg(args, "sql")
		if err := lake.ValidateAlter(sqlStr); err != nil {
			return mcp.NewToolResultErrorFromErr("lake.alter", err), nil
		}
		res, err := lk.Exec(ctx, sqlStr)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("lake.alter exec", err), nil
		}
		return jsonResult(res)
	}
}

func handleLakeDrop(lk *lake.Service) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		obj := stringArg(args, "object")
		kind := stringArg(args, "kind")
		cascade := false
		if c, ok := args["cascade"].(bool); ok {
			cascade = c
		}
		res, err := lk.DropObject(ctx, obj, kind, cascade)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("lake.drop", err), nil
		}
		return jsonResult(res)
	}
}

// ---- helpers ----------------------------------------------------------------

func jsonResult(v any) (*mcp.CallToolResult, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("lake: marshal", err), nil
	}
	return mcp.NewToolResultText(string(body)), nil
}

func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func asInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int32:
		return int(x), true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}

// validateIngestPath enforces a sandbox: lake.ingest only accepts paths that
// fall under one of the configured allowlist roots, or http(s) URLs (which
// have no filesystem access). Defaults are kept conservative — the user's
// nova workspace and the toolyard inbox.
func validateIngestPath(uri string) error {
	if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") {
		return nil
	}
	if uri == "" {
		return fmt.Errorf("source_uri is required")
	}
	abs, err := absUserPath(uri)
	if err != nil {
		return err
	}
	for _, root := range ingestAllowedRoots() {
		if strings.HasPrefix(abs, root) {
			return nil
		}
	}
	return fmt.Errorf("path %q is outside the ingest allowlist (~/.toolyard/inbox)", abs)
}
