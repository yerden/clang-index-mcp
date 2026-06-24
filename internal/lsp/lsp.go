// Package lsp is a small JSON-RPC 2.0 client for talking to a language
// server over a pair of io.Reader/io.Writer (the LSP base protocol).
//
// It does not know about clangd specifically — clangd-specific message
// shapes (workspace/symbol params, callHierarchy items) live in
// internal/extract; this package only does framing, correlation, and
// outbound notifications.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrClientClosed is returned by Call/Notify when the connection is
// already known to be closed (e.g. the server process has exited).
var ErrClientClosed = errors.New("lsp: client is closed")

// ErrConnectionClosed is returned when the connection closes while a
// request is in flight (the reply channel is drained without a value).
var ErrConnectionClosed = errors.New("lsp: connection closed before reply")

// Client is a goroutine-safe JSON-RPC client over the LSP base protocol.
// Construct with NewClient and start the read loop with Run; one Run per
// Client, on its own goroutine.
type Client struct {
	w io.Writer
	r *bufio.Reader

	wmu sync.Mutex // serializes writes; LSP frames must not interleave

	nextID atomic.Int64

	pendingMu sync.Mutex
	pending   map[int64]chan *response

	// notification handlers, keyed by method name
	handlersMu sync.RWMutex
	handlers   map[string]NotificationHandler

	closed atomic.Bool
}

// NotificationHandler receives a server→client notification.
type NotificationHandler func(params json.RawMessage)

// rpcError mirrors the JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// response is the decoded form of either a result or an error reply.
type response struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

// raw is the union shape we read off the wire.
type raw struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // server uses this for both replies and server→client requests
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// NewClient wires up the Client without starting the read loop.
func NewClient(r io.Reader, w io.Writer) *Client {
	return &Client{
		w:        w,
		r:        bufio.NewReader(r),
		pending:  map[int64]chan *response{},
		handlers: map[string]NotificationHandler{},
	}
}

// OnNotification registers a handler for a server-pushed notification.
// Replacing an existing handler is allowed; the latest one wins.
func (c *Client) OnNotification(method string, h NotificationHandler) {
	c.handlersMu.Lock()
	c.handlers[method] = h
	c.handlersMu.Unlock()
}

// Run reads frames until EOF or ctx done. Returns the underlying read
// error. Should be invoked once on its own goroutine.
func (c *Client) Run(ctx context.Context) error {
	defer c.closeAllPending()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := c.readMessage()
		if err != nil {
			c.closed.Store(true)
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		c.dispatch(msg)
	}
}

func (c *Client) closeAllPending() {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}

func (c *Client) dispatch(m *raw) {
	// Reply: has an id (numeric, since that's what we sent) and either result or error.
	if len(m.ID) > 0 && m.Method == "" {
		var id int64
		if err := json.Unmarshal(m.ID, &id); err != nil {
			return
		}
		c.pendingMu.Lock()
		ch, ok := c.pending[id]
		if ok {
			delete(c.pending, id)
		}
		c.pendingMu.Unlock()
		if !ok {
			return
		}
		ch <- &response{ID: id, Result: m.Result, Error: m.Error}
		close(ch)
		return
	}
	// Server→client request (id + method): we don't implement any
	// reverse-direction RPC, but several LSP servers — clangd included —
	// gate features like $/progress on a successful reply to setup
	// requests such as window/workDoneProgress/create. Auto-reply OK so
	// those features turn on. Ignore errors; nothing we can do about them.
	if m.Method != "" && len(m.ID) > 0 {
		_ = c.writeFrame(map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(m.ID),
			"result":  nil,
		})
		return
	}
	// Server→client notification.
	if m.Method != "" && len(m.ID) == 0 {
		c.handlersMu.RLock()
		h := c.handlers[m.Method]
		c.handlersMu.RUnlock()
		if h != nil {
			h(m.Params)
		}
	}
}

// readMessage reads one framed JSON-RPC message.
func (c *Client) readMessage() (*raw, error) {
	contentLen := -1
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length:") {
			v := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length %q: %w", v, err)
			}
			contentLen = n
		}
		// Ignore other headers (Content-Type).
	}
	if contentLen < 0 {
		return nil, errors.New("missing Content-Length header")
	}
	body := make([]byte, contentLen)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return nil, err
	}
	var m raw
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	return &m, nil
}

// Call sends a request and waits for the reply. The result is the raw
// JSON of the `result` field; decode it at the call site.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.closed.Load() {
		return nil, ErrClientClosed
	}
	id := c.nextID.Add(1)
	ch := make(chan *response, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	if err := c.writeFrame(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return nil, ErrConnectionClosed
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// Notify sends a notification (no reply expected).
func (c *Client) Notify(method string, params any) error {
	return c.writeFrame(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (c *Client) writeFrame(payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	if _, err := c.w.Write(body); err != nil {
		return err
	}
	return nil
}

// Initialize is a thin convenience around the initialize/initialized
// handshake, since every clangd session needs it before any other call.
func (c *Client) Initialize(ctx context.Context, rootURI string, capabilities any) (json.RawMessage, error) {
	res, err := c.Call(ctx, "initialize", map[string]any{
		"processId":    nil,
		"rootUri":      rootURI,
		"capabilities": capabilities,
	})
	if err != nil {
		return nil, err
	}
	if err := c.Notify("initialized", map[string]any{}); err != nil {
		return nil, err
	}
	return res, nil
}

// Shutdown performs the LSP shutdown/exit handshake.
func (c *Client) Shutdown(ctx context.Context) error {
	if _, err := c.Call(ctx, "shutdown", nil); err != nil {
		return err
	}
	return c.Notify("exit", nil)
}
