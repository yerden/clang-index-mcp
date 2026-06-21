// Package clangdproc spawns and supervises a clangd process and exposes
// it to higher layers as an LSP client. It owns:
//
//   - process lifecycle (Start, Stop, Wait)
//   - clangd-flavor extras: --background-index-path persistence,
//     waiting for the "indexed" progress signal before letting callers
//     begin extraction
//   - a thin Daemon wrapper that adds debounced restart-on-compdb-change
//
// The "wait for index settle" path subscribes to clangd's
// $/progress notifications and resolves when the
// "backgroundIndexProgress" progress token closes with `end`. Some
// clangd versions emit a different token name; we accept any progress
// token whose title or kind indicates background indexing.
package clangdproc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yerden/clang-index-mcp/internal/lsp"
)

// Options configures a clangd process. Path defaults to "clangd" on
// $PATH if empty.
type Options struct {
	// Path to the clangd binary; "" means "look up clangd on PATH".
	Path string

	// CompileCommandsDir is the directory holding compile_commands.json.
	// Passed as --compile-commands-dir.
	CompileCommandsDir string

	// BackgroundIndexPath, if non-empty, is forwarded as
	// --background-index-path so shards persist across restarts
	// (architecture §6.2). For `clang-index build` leave this empty —
	// disposable extraction (architecture §6.2 closing paragraph).
	BackgroundIndexPath string

	// Stderr receives clangd's stderr stream. nil → os.Stderr.
	Stderr io.Writer

	// ExtraArgs lets tests inject -log=verbose or similar without
	// growing this struct for every clangd flag.
	ExtraArgs []string
}

// Process is a running clangd with its LSP client wired up.
type Process struct {
	cmd       *exec.Cmd
	cli       *lsp.Client
	runErrCh  chan error
	rootURI   string
	indexDone atomic.Bool

	// indexedCh is closed exactly once when background indexing settles,
	// or when we decide to give up waiting.
	indexedOnce sync.Once
	indexedCh   chan struct{}
}

// Client returns the underlying LSP client for issuing requests.
func (p *Process) Client() *lsp.Client { return p.cli }

// RootURI returns the file:// URI that was passed to initialize.
func (p *Process) RootURI() string { return p.rootURI }

// Start spawns clangd, performs the initialize handshake, and registers
// progress handlers. The caller must call Stop when done.
func Start(ctx context.Context, opts Options) (*Process, error) {
	bin := opts.Path
	if bin == "" {
		bin = "clangd"
	}
	args := []string{
		"--log=error",
		"--pch-storage=memory",
	}
	if opts.CompileCommandsDir != "" {
		args = append(args, "--compile-commands-dir="+opts.CompileCommandsDir)
	}
	if opts.BackgroundIndexPath != "" {
		// Ensure the directory exists so clangd doesn't silently skip persistence.
		if err := os.MkdirAll(opts.BackgroundIndexPath, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir background-index-path: %w", err)
		}
		args = append(args, "--background-index-path="+opts.BackgroundIndexPath)
	}
	args = append(args, opts.ExtraArgs...)

	cmd := exec.CommandContext(ctx, bin, args...)
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", bin, err)
	}

	cli := lsp.NewClient(stdout, stdin)
	p := &Process{
		cmd:       cmd,
		cli:       cli,
		runErrCh:  make(chan error, 1),
		indexedCh: make(chan struct{}),
	}

	// Track background-index progress. clangd emits work-done progress
	// with token "backgroundIndexProgress"; .kind = begin/report/end.
	cli.OnNotification("$/progress", func(raw json.RawMessage) {
		var note struct {
			Token any `json:"token"`
			Value struct {
				Kind  string `json:"kind"`
				Title string `json:"title"`
			} `json:"value"`
		}
		if err := json.Unmarshal(raw, &note); err != nil {
			return
		}
		tok := fmt.Sprintf("%v", note.Token)
		if note.Value.Kind == "end" &&
			(strings.Contains(strings.ToLower(tok), "background") ||
				strings.Contains(strings.ToLower(note.Value.Title), "indexing")) {
			p.markIndexed()
		}
	})
	// clangd also expects window/workDoneProgress/create — we just ack it.
	// It's a server→client request, our minimal client doesn't reply, that's fine.

	go func() { p.runErrCh <- cli.Run(ctx) }()

	rootURI := dirToURI(opts.CompileCommandsDir)
	p.rootURI = rootURI

	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := cli.Initialize(initCtx, rootURI, map[string]any{
		"window": map[string]any{
			"workDoneProgress": true,
		},
		"textDocument": map[string]any{
			// Advertise hierarchical DocumentSymbol so clangd returns
			// DocumentSymbol[] with a precise `selectionRange` pointing
			// at the symbol's identifier — without this it falls back
			// to legacy SymbolInformation[] whose range covers the
			// whole declaration body, and symbolInfo at body-start
			// returns nothing.
			"documentSymbol": map[string]any{
				"hierarchicalDocumentSymbolSupport": true,
			},
			"callHierarchy": map[string]any{"dynamicRegistration": false},
		},
	}); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("initialize: %w", err)
	}

	return p, nil
}

func (p *Process) markIndexed() {
	p.indexedOnce.Do(func() {
		p.indexDone.Store(true)
		close(p.indexedCh)
	})
}

// WaitIndexed blocks until clangd reports background indexing complete,
// or ctx fires. If clangd is too old or too quiet to emit progress, this
// will time out — callers should pass a deadline.
func (p *Process) WaitIndexed(ctx context.Context) error {
	select {
	case <-p.indexedCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ForceIndexed is an escape hatch for callers that have an out-of-band
// signal that indexing is done (e.g. a test fixture that knows it has
// 3 TUs and waits for them by other means).
func (p *Process) ForceIndexed() { p.markIndexed() }

// Stop performs shutdown/exit and waits for the process to exit. Best
// effort — if shutdown hangs, kills the process.
func (p *Process) Stop(ctx context.Context) error {
	shutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = p.cli.Shutdown(shutCtx) // best effort

	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
		return errors.New("clangdproc: forced kill after shutdown timeout")
	case err := <-done:
		return err
	}
}

// dirToURI converts an absolute directory path to a file:// URI.
func dirToURI(dir string) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	u := url.URL{Scheme: "file", Path: abs}
	return u.String()
}
