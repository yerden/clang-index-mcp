// Package mcp wires the search_symbol and get_symbol tools to a
// store.Store and exposes them over standard MCP transports via
// github.com/mark3labs/mcp-go. mcp-go handles JSON-RPC framing, the
// legacy HTTP+SSE transport's two-endpoint dance (event stream +
// message POST endpoint, including the initial `event: endpoint`
// handshake), and context-driven cancellation for stdio.
package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpsrv "github.com/mark3labs/mcp-go/server"

	"github.com/yerden/clang-index-mcp/internal/extract"
	"github.com/yerden/clang-index-mcp/internal/store"
)

// Server is a thin wrapper around mcp-go's MCPServer with our tools
// pre-registered against a store.Store.
type Server struct {
	mcp   *mcpsrv.MCPServer
	store *store.Store
	name  string
}

// New constructs an MCP server with search_symbol and get_symbol
// registered against s.
func New(s *store.Store, name string) *Server {
	srv := &Server{
		mcp:   mcpsrv.NewMCPServer(name, "0.1.0", mcpsrv.WithToolCapabilities(true)),
		store: s,
		name:  name,
	}
	srv.registerTools()
	return srv
}

// MCPServer returns the underlying mcp-go server, for use with any
// transport mcp-go provides (StreamableHTTP, in-process, ...). The
// common ones — stdio and SSE — have convenience wrappers below.
func (s *Server) MCPServer() *mcpsrv.MCPServer { return s.mcp }

func (s *Server) registerTools() {
	s.mcp.AddTool(
		mcplib.NewTool("search_symbol",
			mcplib.WithDescription("Full-text search over symbol name+signature in the index."),
			mcplib.WithString("query",
				mcplib.Required(),
				mcplib.Description("FTS5 MATCH expression."),
			),
			mcplib.WithNumber("limit",
				mcplib.Description("Maximum results to return (default 50, max 500)."),
			),
		),
		s.handleSearchSymbol,
	)
	s.mcp.AddTool(
		mcplib.NewTool("get_symbol",
			mcplib.WithDescription("Fetch a symbol by id plus its DIRECT callers and callees. "+
				"\"Direct\" means clangd resolved the call to a concrete function statically — `foo(x)` where `foo` is a known function. "+
				"Indirect calls (`fn(x)` where `fn` is a function-pointer parameter, variable, struct field, or array slot) are NOT in callers/callees here. "+
				"If the symbol is a dispatcher (look for indirect calls in its body) and you need to know who it might invoke, call get_indirect_call_sites with this symbol's id; "+
				"if you want to know who registers THIS symbol as a callback (the reverse), call get_address_take_sites with this symbol's id. "+
				"For ad-hoc joins or shapes not covered by these tools (transitive callers, name-pattern aggregation), use sql_query — call describe_schema first."),
			mcplib.WithNumber("id",
				mcplib.Required(),
				mcplib.Description("Symbol id, as returned by search_symbol."),
			),
		),
		s.handleGetSymbol,
	)
	s.mcp.AddTool(
		mcplib.NewTool("list_symbols_in_file",
			mcplib.WithDescription("List all symbols declared or defined in a given file (typically a header). Use to answer 'what's the public API of this header' / 'what's declared in xxx.h'."),
			mcplib.WithString("file",
				mcplib.Required(),
				mcplib.Description("Repo-relative path; matched exactly against either decl_file or file (definition)."),
			),
			mcplib.WithNumber("limit",
				mcplib.Description("Maximum results (default 200)."),
			),
		),
		s.handleListSymbolsInFile,
	)

	// Function-pointer dispatch tools (architecture §6.5). These
	// surface raw facts (address-take sites + indirect call sites) and
	// leave the synthesis of "which dispatcher might call this
	// callback" to the agent — the agent has contextual knowledge
	// (naming conventions, header membership) that lets it filter
	// far more precisely than a sound-but-noisy static synthesis can.

	addressTakeDesc := "Find function-pointer address-take sites in the index.\n\n" +
		"Typical reverse-traversal usage (\"who might dispatch to X?\"):\n" +
		"  1. get_address_take_sites(function_id=X.id)\n" +
		"  2. For each `arg_to:F#i` row, F is a function that received X's address — likely a dispatcher.\n" +
		"  3. get_indirect_call_sites(function_id=F.id) to verify F has an indirect call of matching type.\n\n" +
		"Typical forward-traversal usage (\"what might dispatcher D invoke?\"):\n" +
		"  1. get_indirect_call_sites(function_id=D.id) → read off callee_type T and callee_expr.\n" +
		"  2. find_address_takes(type=T, category=\"arg_to\", context_detail_pattern=\"D_name#%\")\n" +
		"     to enumerate the candidates registered with D.\n\n" +
		extract.AddressTakeCategoryVocabulary

	s.mcp.AddTool(
		mcplib.NewTool("find_address_takes",
			mcplib.WithDescription(addressTakeDesc),
			mcplib.WithString("type",
				mcplib.Description("Optional. Exact match on the canonical function-pointer type, e.g. \"int (*)(int)\". Use the canonical (post-typedef-decay) form — \"op_t\" will NOT match \"int (*)(int)\"."),
			),
			mcplib.WithString("category",
				mcplib.Description("Optional. Exact match. One of: compared | arg_to | stored_in | array_init | assigned_to | returned_from | other. See category vocabulary in this tool's description."),
			),
			mcplib.WithString("context_detail_pattern",
				mcplib.Description("Optional. Case-sensitive SQL LIKE pattern matched against context_detail. Wildcards: `%` = any chars (including empty), `_` = exactly one char. "+
					"IMPORTANT: context_detail does NOT include the category prefix; pattern matches only the suffix part. "+
					"Examples for category='arg_to': \"register_handler#%\" (any arg slot), \"register_handler#0\" (first arg only). "+
					"Examples for category='stored_in': \"struct_ops.%\" (any field of struct_ops). "+
					"Examples for category='array_init': \"ops%\" (any array starting with 'ops')."),
			),
			mcplib.WithNumber("limit",
				mcplib.Description("Maximum results (default 200)."),
			),
		),
		s.handleFindAddressTakes,
	)

	s.mcp.AddTool(
		mcplib.NewTool("get_address_take_sites",
			mcplib.WithDescription("List every site where the given function's address is taken (registered, stored, compared, returned, ...). "+
				"Useful after get_symbol when you have a function and want to know how it's wired into dispatchers. "+
				"Each row carries category + context_detail per site; aggregate across rows for the full picture.\n\n"+
				extract.AddressTakeCategoryVocabulary),
			mcplib.WithNumber("function_id",
				mcplib.Required(),
				mcplib.Description("Symbol id of the function WHOSE address is taken (the callback / handler). Not the dispatcher's id."),
			),
			mcplib.WithNumber("limit",
				mcplib.Description("Maximum results (default 200)."),
			),
		),
		s.handleGetAddressTakeSites,
	)

	s.mcp.AddTool(
		mcplib.NewTool("get_indirect_call_sites",
			mcplib.WithDescription("List indirect-call sites — CallExprs whose callee isn't a directly-named function. "+
				"Each row carries callee_type (e.g. \"int (*)(int)\") and callee_expr (e.g. \"fn\", \"ops[i]\", \"<base>.cb\"). "+
				"When you hit a dispatcher dead-end via get_symbol, call this with the dispatcher's id; "+
				"inspect callee_expr and callee_type, then use find_address_takes(type=callee_type, category=\"arg_to\", context_detail_pattern=\"dispatcher_name#%\") to enumerate candidates.\n\n"+
				"Use callee_expr_pattern to narrow by which field/expression is being called through — e.g. \"%.lcore_func\" matches every dispatch site that reads `.lcore_func` on any base, which is how you distinguish multiple same-typed dispatchers in a registry-heavy codebase.\n\n"+
				"Omitting function_id returns every indirect call site in the project — useful for exploration but potentially many rows; combine with `type`, `callee_expr_pattern`, or `limit`."),
			mcplib.WithNumber("function_id",
				mcplib.Description("Optional. Symbol id of the function CONTAINING the indirect calls (i.e. the dispatcher). Different semantics from get_address_take_sites's function_id."),
			),
			mcplib.WithString("type",
				mcplib.Description("Optional. Exact match on callee_type (canonical form, e.g. \"int (*)(int)\"). Types are canonicalized at extract time — typedef-spelled forms (e.g. \"lcore_function_t *\") are substituted to the canonical pointer form, so always match against the canonical."),
			),
			mcplib.WithString("callee_expr_pattern",
				mcplib.Description("Optional. Case-sensitive SQL LIKE pattern over callee_expr. Wildcards: `%` = any chars, `_` = single char. "+
					"For member-access dispatch sites callee_expr has the shape \"<base>.<field>\" — use \"%.<field>\" to match any base with a specific field, or \"<expr>\" for the opaque catch-all expressions. "+
					"Examples: \"fn\" (param-named callbacks), \"%.lcore_func\" (any \".lcore_func\" dispatch), \"ops[%]\" (array-of-ops dispatch)."),
			),
			mcplib.WithNumber("limit",
				mcplib.Description("Maximum results (default 200)."),
			),
		),
		s.handleGetIndirectCallSites,
	)

	s.mcp.AddTool(
		mcplib.NewTool("describe_address_take_categories",
			mcplib.WithDescription("Returns the vocabulary used in the `category` field of find_address_takes / get_address_take_sites. Structured for programmatic use: a precedence_order list and a per-category descriptor with name, rank, description, example, and agent_guidance. Call once at session start if you want the contract; the same prose is also embedded in those tools' descriptions."),
		),
		s.handleDescribeCategories,
	)

	s.mcp.AddTool(
		mcplib.NewTool("describe_schema",
			mcplib.WithDescription(
				"Returns the SQLite schema of the index DB and a semantic guide for writing sql_query against it. Response shape:\n"+
					"  { \"schema_sql\":  string,   // raw CREATE TABLE/INDEX (authoritative for column names/types)\n"+
					"    \"guide\":       string,   // semantic layer: sentinel meanings, enum values, join recipes\n"+
					"    \"categories\":  object }  // same structure as describe_address_take_categories\n"+
					"Call this once before sql_query. The address_takes.category vocabulary is the same one returned by describe_address_take_categories — included here so a single call covers everything you need to write SQL against the index.\n\n"+
					"Schema and guide are versioned together: a column rename or semantic shift updates both in one commit. If your SQL stops returning expected shapes after a daemon restart, re-call this — the underlying DB may have been rebuilt with a newer extraction."),
		),
		s.handleDescribeSchema,
	)

	s.mcp.AddTool(
		mcplib.NewTool("sql_query",
			mcplib.WithDescription(
				"Execute a read-only SQL query against the index DB. The DB handle is opened with SQLite ?mode=ro, so writes (INSERT/UPDATE/DELETE/ALTER/ATTACH/PRAGMA writable_schema=ON) fail at the engine with 'attempt to write a readonly database' — no SQL parsing is done here, the guarantee is at the driver level.\n\n"+
					"Call describe_schema first for table/column semantics, sentinel meanings (decl_file='' means same as file), enum values, and canonical join recipes.\n\n"+
					"Response shape:\n"+
					"  { \"columns\":    [string],   // column names in result order\n"+
					"    \"rows\":       [[any]],   // row values in `columns` order; NULL → null; BLOB → base64 string\n"+
					"    \"truncated\":  bool,      // true if max_rows was hit — rewrite (COUNT/GROUP BY/narrower WHERE) rather than page\n"+
					"    \"elapsed_ms\": number }\n"+
					"Rows are arrays-of-values (not objects) to preserve column order and to keep duplicate column names (e.g. `SELECT a.id, b.id`) from clobbering each other.\n\n"+
					"Use ? placeholders bound via `params` (positional, in order) rather than interpolating into `sql` — avoids escaping issues and lets the SQLite query planner cache the plan.\n\n"+
					"Limits — the row cap protects YOUR context window, not the DB:\n"+
					"  - max_rows:   default 500, max 5000. If truncated=true, prefer rewriting the query (COUNT(*), GROUP BY, narrower WHERE) over paging.\n"+
					"  - timeout_ms: default 5000, max 30000. Enforced via SQLite interrupt — kills runaway WITH RECURSIVE before it exhausts memory.\n\n"+
					"WITH RECURSIVE is allowed and is how you walk the call graph transitively. The timeout is the bound — there is no separate recursion-depth cap. Temp tables / ATTACH / CTE-INSERT are blocked by ?mode=ro.\n\n"+
					"Error messages from SQLite are surfaced verbatim (syntax errors, no-such-column, readonly violations) — read and self-correct."),
			mcplib.WithString("sql", mcplib.Required(),
				mcplib.Description("SQL SELECT, WITH ... SELECT, or read-only PRAGMA (e.g. 'PRAGMA table_info(symbols)'). Writes fail at the engine.")),
			mcplib.WithArray("params",
				mcplib.Description("Optional positional params for ? placeholders. Bind types: JSON number → INTEGER/REAL, string → TEXT, null → NULL, bool → INTEGER 0/1.")),
			mcplib.WithNumber("max_rows",
				mcplib.Description("Row cap (default 500, max 5000). On hit, truncated=true.")),
			mcplib.WithNumber("timeout_ms",
				mcplib.Description("Per-query timeout in milliseconds (default 5000, max 30000).")),
		),
		s.handleSQLQuery,
	)
}

func (s *Server) handleSearchSymbol(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	args := req.GetArguments()
	query, ok := args["query"].(string)
	if !ok || query == "" {
		return mcplib.NewToolResultError("query is required and must be a string"), nil
	}
	limit := 50
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}
	hits, err := s.store.SearchSymbol(query, limit)
	if err != nil {
		return mcplib.NewToolResultError("search_symbol: " + err.Error()), nil
	}
	return jsonResult(hits)
}

func (s *Server) handleListSymbolsInFile(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	args := req.GetArguments()
	file, ok := args["file"].(string)
	if !ok || file == "" {
		return mcplib.NewToolResultError("file is required and must be a string"), nil
	}
	limit := 200
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}
	hits, err := s.store.ListSymbolsInFile(file, limit)
	if err != nil {
		return mcplib.NewToolResultError("list_symbols_in_file: " + err.Error()), nil
	}
	return jsonResult(hits)
}

func (s *Server) handleFindAddressTakes(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	args := req.GetArguments()
	typeFilter, _ := args["type"].(string)
	category, _ := args["category"].(string)
	pattern, _ := args["context_detail_pattern"].(string)
	limit := 200
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}
	hits, err := s.store.FindAddressTakes(typeFilter, category, pattern, limit)
	if err != nil {
		return mcplib.NewToolResultError("find_address_takes: " + err.Error()), nil
	}
	return jsonResult(hits)
}

func (s *Server) handleGetAddressTakeSites(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	args := req.GetArguments()
	idF, ok := args["function_id"].(float64)
	if !ok {
		return mcplib.NewToolResultError("function_id is required and must be a number"), nil
	}
	limit := 200
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}
	hits, err := s.store.GetAddressTakeSites(int64(idF), limit)
	if err != nil {
		return mcplib.NewToolResultError("get_address_take_sites: " + err.Error()), nil
	}
	return jsonResult(hits)
}

func (s *Server) handleGetIndirectCallSites(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	args := req.GetArguments()
	limit := 200
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}
	typeFilter, _ := args["type"].(string)
	exprPattern, _ := args["callee_expr_pattern"].(string)
	if idF, ok := args["function_id"].(float64); ok {
		hits, err := s.store.GetIndirectCallSitesByCaller(int64(idF), exprPattern, limit)
		if err != nil {
			return mcplib.NewToolResultError("get_indirect_call_sites: " + err.Error()), nil
		}
		return jsonResult(hits)
	}
	hits, err := s.store.ListIndirectCallSites(typeFilter, exprPattern, limit)
	if err != nil {
		return mcplib.NewToolResultError("get_indirect_call_sites: " + err.Error()), nil
	}
	return jsonResult(hits)
}

func (s *Server) handleDescribeCategories(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	return jsonResult(categoriesPayload())
}

func categoriesPayload() map[string]any {
	return map[string]any{
		"precedence_order": extract.AddressTakeCategoryPrecedence(),
		"categories":       extract.AddressTakeCategoryDescriptors(),
		"prose":            extract.AddressTakeCategoryVocabulary,
	}
}

func (s *Server) handleDescribeSchema(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	return jsonResult(map[string]any{
		"schema_sql": store.SchemaSQL(),
		"guide":      store.SchemaGuide,
		"categories": categoriesPayload(),
	})
}

const (
	sqlDefaultMaxRows = 500
	sqlMaxMaxRows     = 5000
	sqlDefaultTimeout = 5 * time.Second
	sqlMaxTimeout     = 30 * time.Second
)

func (s *Server) handleSQLQuery(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	args := req.GetArguments()
	sqlText, ok := args["sql"].(string)
	if !ok || sqlText == "" {
		return mcplib.NewToolResultError("sql is required and must be a string"), nil
	}
	maxRows := sqlDefaultMaxRows
	if v, ok := args["max_rows"].(float64); ok && v > 0 {
		maxRows = int(v)
		if maxRows > sqlMaxMaxRows {
			maxRows = sqlMaxMaxRows
		}
	}
	timeout := sqlDefaultTimeout
	if v, ok := args["timeout_ms"].(float64); ok && v > 0 {
		timeout = time.Duration(v) * time.Millisecond
		if timeout > sqlMaxTimeout {
			timeout = sqlMaxTimeout
		}
	}
	var params []any
	if raw, ok := args["params"].([]any); ok {
		params = raw
	}

	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	cols, rows, truncated, err := s.store.QueryReadOnly(qctx, sqlText, params, maxRows)
	elapsed := time.Since(start)
	if err != nil {
		// Surface the SQLite message verbatim — readonly violations, syntax
		// errors, no-such-column all carry useful detail the agent can act on.
		return mcplib.NewToolResultError("sql_query: " + err.Error()), nil
	}
	for i := range rows {
		for j, v := range rows[i] {
			if b, ok := v.([]byte); ok {
				rows[i][j] = base64.StdEncoding.EncodeToString(b)
			}
		}
	}
	return jsonResult(map[string]any{
		"columns":    cols,
		"rows":       rows,
		"truncated":  truncated,
		"elapsed_ms": elapsed.Milliseconds(),
	})
}

func (s *Server) handleGetSymbol(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	args := req.GetArguments()
	idF, ok := args["id"].(float64)
	if !ok {
		return mcplib.NewToolResultError("id is required and must be a number"), nil
	}
	id := int64(idF)
	sym, err := s.store.GetSymbol(id)
	if err != nil {
		return mcplib.NewToolResultError("get_symbol: " + err.Error()), nil
	}
	callers, err := s.store.Callers(id)
	if err != nil {
		return mcplib.NewToolResultError("callers: " + err.Error()), nil
	}
	callees, err := s.store.Callees(id)
	if err != nil {
		return mcplib.NewToolResultError("callees: " + err.Error()), nil
	}
	return jsonResult(map[string]any{
		"symbol":  sym,
		"callers": callers,
		"callees": callees,
	})
}

// jsonResult serializes v as a single text/json content part so the
// assistant can parse structured data on the other side.
func jsonResult(v any) (*mcplib.CallToolResult, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return mcplib.NewToolResultError("encode: " + err.Error()), nil
	}
	return mcplib.NewToolResultText(string(body)), nil
}

// ServeStdio runs the MCP stdio transport. mcp-go's readNextLine runs
// the read in a goroutine and selects on ctx.Done(), so cancelling ctx
// unblocks the read immediately — no need to also close stdin. A
// context-canceled return is treated as a clean shutdown so SIGINT
// doesn't log as an error.
func (s *Server) ServeStdio(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	stdio := mcpsrv.NewStdioServer(s.mcp)
	err := stdio.Listen(ctx, stdin, stdout)
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// StreamableHTTPServer returns an *mcpsrv.StreamableHTTPServer bound to
// this MCPServer. This is the modern (2025-03 spec) single-endpoint
// MCP transport: POST for client→server JSON-RPC, GET to open an
// optional SSE stream for server→client notifications, DELETE to close
// the session. Pass StreamableHTTPOption values (e.g.
// mcpsrv.WithEndpointPath, WithStateLess) to customize.
func (s *Server) StreamableHTTPServer(opts ...mcpsrv.StreamableHTTPOption) *mcpsrv.StreamableHTTPServer {
	return mcpsrv.NewStreamableHTTPServer(s.mcp, opts...)
}

// HandleSingleMessage drives one JSON-RPC payload through the server
// synchronously, for unit tests.
func (s *Server) HandleSingleMessage(ctx context.Context, payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("empty payload")
	}
	resp := s.mcp.HandleMessage(ctx, payload)
	if resp == nil {
		return nil, nil
	}
	return json.Marshal(resp)
}
