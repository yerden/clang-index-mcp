package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/yerden/clang-index-mcp/internal/store"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ix.db")
	syms := []store.Symbol{
		{USR: "u1", Name: "alpha", Kind: "Function", File: "a.c", Line: 1, DeclFile: "api.h", DeclLine: 2, Signature: "void alpha()"},
		{USR: "u2", Name: "beta", Kind: "Function", File: "a.c", Line: 5, DeclFile: "api.h", DeclLine: 7, Signature: "void beta()"},
		{USR: "u3", Name: "gamma", Kind: "Function", File: "b.c", Line: 1, Signature: "static void gamma()"},
	}
	edges := []store.Edge{{CallerUSR: "u1", CalleeUSR: "u2"}}
	if err := store.WriteIndex(path, syms, edges); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, "test")
}

func TestInitializeAndListTools(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()
	resp, err := srv.HandleSingleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !jsonContains(resp, "protocolVersion") {
		t.Fatalf("missing protocolVersion: %s", resp)
	}

	resp, err = srv.HandleSingleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !jsonContains(resp, "search_symbol") || !jsonContains(resp, "get_symbol") {
		t.Fatalf("missing tool: %s", resp)
	}
}

func TestSearchSymbolTool(t *testing.T) {
	srv := newTestServer(t)
	req := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_symbol","arguments":{"query":"alpha","limit":10}}}`
	resp, err := srv.HandleSingleMessage(context.Background(), []byte(req))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Result.IsError {
		t.Fatalf("tool reported error: %s", resp)
	}
	if !jsonContains([]byte(parsed.Result.Content[0].Text), "alpha") {
		t.Fatalf("content missing alpha: %s", resp)
	}
}

func TestGetSymbolTool(t *testing.T) {
	srv := newTestServer(t)
	// find id for u1 first via search
	resp, _ := srv.HandleSingleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_symbol","arguments":{"query":"alpha"}}}`))
	var w struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &w); err != nil {
		t.Fatal(err)
	}
	var hits []struct {
		ID int64 `json:"ID"`
	}
	if err := json.Unmarshal([]byte(w.Result.Content[0].Text), &hits); err != nil {
		t.Fatal(err)
	}
	id := hits[0].ID

	req := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_symbol","arguments":{"id":` + intToStr(id) + `}}}`)
	resp, err := srv.HandleSingleMessage(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonContains(resp, `\"callees\"`) || !jsonContains(resp, `\"callers\"`) {
		t.Fatalf("get_symbol missing edges: %s", resp)
	}
}

func TestListSymbolsInFileTool(t *testing.T) {
	srv := newTestServer(t)
	// Query the header — should return alpha and beta but not gamma.
	req := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_symbols_in_file","arguments":{"file":"api.h"}}}`
	resp, err := srv.HandleSingleMessage(context.Background(), []byte(req))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		if !jsonContains(resp, name) {
			t.Errorf("list_symbols_in_file(api.h) missing %q: %s", name, resp)
		}
	}
	if jsonContains(resp, "gamma") {
		t.Errorf("list_symbols_in_file(api.h) shouldn't include gamma (declared elsewhere): %s", resp)
	}

	// Query a .c file — should also match by definition file.
	req = `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"list_symbols_in_file","arguments":{"file":"b.c"}}}`
	resp, _ = srv.HandleSingleMessage(context.Background(), []byte(req))
	if !jsonContains(resp, "gamma") {
		t.Errorf("list_symbols_in_file(b.c) missing gamma: %s", resp)
	}
}

// TestToolDescriptionsCarryAgentGuidance fences the prose contract the
// MCP tool descriptions are supposed to expose to the agent. These
// phrases are load-bearing — if a refactor drops them silently, the
// agent loses the warnings about direct-vs-indirect, function_id
// semantics ambiguity, the LIKE wildcard rules, and the no-prefix-in-
// context_detail trap.
func TestToolDescriptionsCarryAgentGuidance(t *testing.T) {
	srv := newTestServer(t)
	resp, err := srv.HandleSingleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	must := []string{
		// get_symbol must warn the agent that callers/callees are direct only.
		"DIRECT callers and callees",
		// find_address_takes must explain that context_detail has no category prefix.
		"context_detail does NOT include the category prefix",
		// find_address_takes must list the category enum.
		"compared | arg_to | stored_in | array_init | assigned_to | returned_from | other",
		// find_address_takes must define the LIKE wildcards.
		"`%` = any chars",
		// get_address_take_sites must clarify it's the callback, not the dispatcher.
		"WHOSE address is taken (the callback / handler). Not the dispatcher's id.",
		// get_indirect_call_sites must clarify it's the dispatcher.
		"CONTAINING the indirect calls (i.e. the dispatcher)",
		// get_indirect_call_sites must signal the no-arg explosion risk.
		"Omitting function_id returns every indirect call site in the project",
		// Both forward and reverse traversal recipes must be visible.
		"Typical reverse-traversal",
		"Typical forward-traversal",
		// Gap round 2 fix #1: canonical-form warning must mention typedef substitution.
		"typedef-spelled forms",
		// Gap round 2 fix #3: callee_expr_pattern must explain how to use ".field" patterns.
		"callee_expr has the shape",
		// array_init index caveat: must warn the agent that the bracketed index
		// is the InitList child position, not necessarily the actual array slot
		// (multi-dim, designators, mixed positional/designator).
		"Treat i as a hint, not a guaranteed array slot",
		// sql_query: read-only enforcement is at the driver, not in SQL parsing.
		"opened with SQLite ?mode=ro",
		// sql_query: agent must know the row cap is a context-window guard,
		// and that the response shape uses arrays-of-values, not objects.
		"protects YOUR context window, not the DB",
		"arrays-of-values (not objects)",
		// sql_query: WITH RECURSIVE is the supported way to walk transitive graphs.
		"WITH RECURSIVE is allowed",
		// get_symbol must point at sql_query for ad-hoc shapes.
		"For ad-hoc joins or shapes not covered by these tools",
	}
	for _, m := range must {
		if !jsonContains(resp, escapeJSON(m)) {
			t.Errorf("tools/list missing agent-guidance phrase %q", m)
		}
	}
}

// escapeJSON returns the JSON-string-escaped form of s, since the
// tools/list response embeds descriptions inside JSON string literals
// and we want a byte-level match without dragging in a full decoder.
func escapeJSON(s string) string {
	b, _ := json.Marshal(s)
	// strip the surrounding quotes
	return string(b[1 : len(b)-1])
}

// TestDescribeSchemaCarriesGuidance fences the semantic-guide contract.
// The guide is the agent's only documentation of sentinel meanings and
// canonical joins; if a refactor drops a phrase, agents start emitting
// wrong SQL (e.g. assuming decl_file is NULL when same as file).
func TestDescribeSchemaCarriesGuidance(t *testing.T) {
	srv := newTestServer(t)
	resp, err := srv.HandleSingleMessage(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"describe_schema","arguments":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	must := []string{
		"RELATIVE to ProjectRoot",
		"EMPTY STRING",
		"MATCH, not LIKE",
		"compared | arg_to | stored_in | array_init | assigned_to | returned_from | other",
		"Does NOT carry the category prefix",
		"WITH RECURSIVE",
		// schema_sql echoes the embed verbatim
		"CREATE TABLE symbols",
		"CREATE TABLE call_edges",
		"CREATE TABLE address_takes",
		"CREATE TABLE indirect_call_sites",
	}
	for _, m := range must {
		if !jsonContains(resp, escapeJSON(m)) {
			t.Errorf("describe_schema missing phrase %q", m)
		}
	}
}

// TestSQLQueryReadOnly proves read works and writes are rejected by the
// SQLite engine (no SQL parsing in our code).
func TestSQLQueryReadOnly(t *testing.T) {
	srv := newTestServer(t)

	sel := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sql_query","arguments":{"sql":"SELECT COUNT(*) AS n FROM symbols"}}}`)
	resp, err := srv.HandleSingleMessage(context.Background(), sel)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonContains(resp, `\"columns\":[\"n\"]`) {
		t.Errorf("expected columns=[n] in: %s", resp)
	}
	if !jsonContains(resp, `\"rows\":[[3]]`) {
		t.Errorf("expected rows=[[3]] (three fixture symbols) in: %s", resp)
	}

	upd := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sql_query","arguments":{"sql":"UPDATE symbols SET name='x' WHERE id=1"}}}`)
	resp, err = srv.HandleSingleMessage(context.Background(), upd)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonContains(resp, "readonly") {
		t.Errorf("expected readonly error: %s", resp)
	}
}

// TestSQLQueryParamsAndTruncation covers param binding and the row cap.
func TestSQLQueryParamsAndTruncation(t *testing.T) {
	srv := newTestServer(t)

	// Param binding: filter by exact name.
	q := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sql_query","arguments":{"sql":"SELECT name FROM symbols WHERE name = ?","params":["alpha"]}}}`)
	resp, err := srv.HandleSingleMessage(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonContains(resp, `\"rows\":[[\"alpha\"]]`) {
		t.Errorf("expected rows=[[\"alpha\"]]: %s", resp)
	}

	// Truncation: fixture has 3 symbols; cap at 2 should truncate.
	q = []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sql_query","arguments":{"sql":"SELECT id FROM symbols ORDER BY id","max_rows":2}}}`)
	resp, err = srv.HandleSingleMessage(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonContains(resp, `\"truncated\":true`) {
		t.Errorf("expected truncated=true: %s", resp)
	}
	// No truncation when cap > rows.
	q = []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sql_query","arguments":{"sql":"SELECT id FROM symbols ORDER BY id","max_rows":100}}}`)
	resp, err = srv.HandleSingleMessage(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonContains(resp, `\"truncated\":false`) {
		t.Errorf("expected truncated=false: %s", resp)
	}
}

func TestUnknownTool(t *testing.T) {
	srv := newTestServer(t)
	resp, _ := srv.HandleSingleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}`))
	if !jsonContains(resp, "not found") {
		t.Fatalf("expected not-found error: %s", resp)
	}
}

func intToStr(i int64) string {
	return jsonEncode(i)
}

func jsonEncode(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonContains(haystack []byte, needle string) bool {
	return bytesContains(haystack, []byte(needle))
}

func bytesContains(b, sub []byte) bool {
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == string(sub) {
			return true
		}
	}
	return false
}
