// Package mcp wires the search_symbol and get_symbol tools to a
// store.Store and exposes them over standard MCP transports via
// github.com/mark3labs/mcp-go. mcp-go handles JSON-RPC framing, the
// legacy HTTP+SSE transport's two-endpoint dance (event stream +
// message POST endpoint, including the initial `event: endpoint`
// handshake), and context-driven cancellation for stdio.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpsrv "github.com/mark3labs/mcp-go/server"

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
			mcplib.WithDescription("Fetch a symbol by id plus its direct callers and callees."),
			mcplib.WithNumber("id",
				mcplib.Required(),
				mcplib.Description("symbol id, as returned by search_symbol"),
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
