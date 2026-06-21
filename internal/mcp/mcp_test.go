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
	edges := []store.Edge{
		{CallerUSR: "u1", CalleeUSR: "u2", Kind: store.EdgeDirect},
		{CallerUSR: "u3", CalleeUSR: "u1", Kind: store.EdgeIndirect},
	}
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
	// Each caller/callee row carries an EdgeKind tag so AI consumers can
	// distinguish a clangd-confirmed direct call from a Tier 2
	// synthesized indirect-call candidate (architecture §6.5).
	if !jsonContains(resp, `\"EdgeKind\":\"direct\"`) {
		t.Fatalf("get_symbol response should mark direct edges: %s", resp)
	}
	if !jsonContains(resp, `\"EdgeKind\":\"indirect\"`) {
		t.Fatalf("get_symbol response should mark synthesized indirect edges: %s", resp)
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
