// Command clangd-mcp-daemon runs a long-lived clangd against a project,
// continuously rebuilds a SQLite index as the tree changes, and serves
// the result over MCP (stdio + optional SSE). See architecture §3, §6.
//
// This daemon runs *natively* on the developer host for toolchain/header
// parity with the project's real build (architecture §5.3). Do not put
// it in a container with a different system header layout.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	mcpsrv "github.com/mark3labs/mcp-go/server"

	"github.com/yerden/clang-index-mcp/internal/cache"
	"github.com/yerden/clang-index-mcp/internal/clangdproc"
	"github.com/yerden/clang-index-mcp/internal/extract"
	"github.com/yerden/clang-index-mcp/internal/mcp"
	"github.com/yerden/clang-index-mcp/internal/store"
)

func main() {
	compdb := flag.String("compdb", "", "path to compile_commands.json (required)")
	projectRoot := flag.String("project-root", "", "project root (file paths stored relative to this); default: compdb's directory")
	bgIndexPath := flag.String("background-index-path", "", "persistent clangd background-index dir (architecture §6.2)")
	perFileRoot := flag.String("per-file-cache", "", "per-file extraction cache dir (empty = disabled)")
	clangdPath := flag.String("clangd", "clangd", "clangd binary")
	indexTimeout := flag.Duration("index-timeout", 5*time.Minute, "max time to wait for clangd's background-index settle on each restart")
	dbDir := flag.String("db-dir", ".", "where to write rebuilt index.db files")
	debounce := flag.Duration("debounce", 5*time.Second, "compdb-change debounce window (architecture §6.1)")
	httpAddr := flag.String("http", "", "if non-empty, also serve MCP over Streamable HTTP on this address (e.g. :8080)")
	httpPath := flag.String("http-path", "/mcp", "endpoint path for the Streamable HTTP transport")
	clangdJobs := flag.Int("clangd-jobs", 0, "clangd -j=N worker count (0 = clangd's default, ≈ half the logical cores)")
	clangdBoost := flag.Bool("clangd-boost", false, "run clangd's background indexer at normal OS priority instead of the default nice-19 \"background\" — recommended on a dedicated build host")
	flag.Parse()

	if *compdb == "" {
		fmt.Fprintln(os.Stderr, "clangd-mcp-daemon: --compdb is required")
		os.Exit(2)
	}
	absCompDB, err := filepath.Abs(*compdb)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if err := os.MkdirAll(*dbDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "db-dir:", err)
		os.Exit(1)
	}

	pf, err := cache.NewPerFile(*perFileRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "per-file cache:", err)
		os.Exit(1)
	}

	// Build a first DB synchronously so the MCP server has something to
	// serve from the moment it accepts connections.
	ctx, cancel := signalCtx()
	defer cancel()

	firstDB := filepath.Join(*dbDir, "index.db")
	st, err := openOrSeed(firstDB)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed db:", err)
		os.Exit(1)
	}
	defer st.Close()

	srv := mcp.New(st, "clangd-mcp-daemon")

	// The daemon serves exactly one MCP transport: Streamable HTTP if
	// --http is set, stdio otherwise. Running both at once would let a
	// stdio client and an HTTP client share a single in-memory DB
	// without any access-control story, and clients picking up the
	// wrong endpoint silently is exactly the kind of foot-gun we'd
	// rather force the operator to be explicit about.
	var wg sync.WaitGroup
	var httpSrv *mcpsrv.StreamableHTTPServer
	if *httpAddr != "" {
		httpSrv = srv.StreamableHTTPServer(mcpsrv.WithEndpointPath(*httpPath))
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel() // listener exit => shut the daemon down
			fmt.Fprintf(os.Stderr, "daemon: Streamable HTTP listening on %s%s\n", *httpAddr, *httpPath)
			if err := httpSrv.Start(*httpAddr); err != nil && err != http.ErrServerClosed {
				fmt.Fprintln(os.Stderr, "http:", err)
			}
		}()
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel() // stdin EOF / client hangup => shut the daemon down
			if err := srv.ServeStdio(ctx, os.Stdin, os.Stdout); err != nil {
				fmt.Fprintln(os.Stderr, "stdio:", err)
			}
		}()
	}

	// Build cycle: every time clangd is (re)ready, run a fresh
	// extraction, write a new DB, then atomically swap it into the
	// live Store (architecture §8.3).
	var rebuildSeq uint64
	rebuildMu := &sync.Mutex{}
	onReady := func(p *clangdproc.Process) error {
		rebuildMu.Lock()
		rebuildSeq++
		seq := rebuildSeq
		rebuildMu.Unlock()

		newPath := filepath.Join(*dbDir, fmt.Sprintf("index-%d.db", seq))
		res, err := extract.Run(ctx, p.Client(), extract.Options{
			CompDBPath:  absCompDB,
			ProjectRoot: *projectRoot,
			PerFile:     pf,
			WaitForIndex: func(c context.Context) error {
				waitCtx, cancel := context.WithTimeout(c, *indexTimeout)
				defer cancel()
				if err := p.WaitIndexed(waitCtx); err != nil {
					fmt.Fprintln(os.Stderr, "daemon: warning: index-settle wait:", err)
				}
				return nil
			},
		})
		if err != nil {
			return fmt.Errorf("extract: %w", err)
		}
		if err := store.WriteIndexWithFacts(newPath, res.Symbols, res.Edges, res.AddressTakes, res.IndirectCallSites); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		if err := st.Swap(newPath); err != nil {
			return fmt.Errorf("swap: %w", err)
		}
		// Replace the canonical firstDB with the latest, so a daemon
		// restart loads a recent index without re-extracting.
		_ = os.Remove(firstDB)
		_ = os.Rename(newPath, firstDB)
		_ = st.Swap(firstDB)
		fmt.Fprintf(os.Stderr, "daemon: rebuilt index: %d symbols, %d edges\n", len(res.Symbols), len(res.Edges))
		return nil
	}

	clangdOpts := clangdproc.Options{
		Path:                *clangdPath,
		CompileCommandsDir:  filepath.Dir(absCompDB),
		BackgroundIndexPath: *bgIndexPath,
		Jobs:                *clangdJobs,
	}
	if *clangdBoost {
		clangdOpts.BackgroundIndexPriority = "normal"
	}
	d := clangdproc.NewDaemon(clangdOpts, *debounce)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := d.Run(ctx, onReady); err != nil && err != context.Canceled {
			fmt.Fprintln(os.Stderr, "daemon loop:", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := watchCompDB(ctx, absCompDB, d); err != nil && err != context.Canceled {
			fmt.Fprintln(os.Stderr, "compdb watch:", err)
		}
	}()

	<-ctx.Done()
	d.Close()
	if httpSrv != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = httpSrv.Shutdown(shutCtx)
		cancel()
	}
	wg.Wait()
}

// openOrSeed returns a Store backed by path. If path doesn't exist yet,
// we seed an empty index so the MCP server has a valid handle.
func openOrSeed(path string) (*store.Store, error) {
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		if err := store.WriteIndex(path, nil, nil); err != nil {
			return nil, fmt.Errorf("seed: %w", err)
		}
	}
	return store.Open(path)
}

// watchCompDB pulses d.Restart() whenever compile_commands.json changes.
// fsnotify can't watch a file that's atomically replaced (rename), so we
// watch its parent directory and filter by basename.
func watchCompDB(ctx context.Context, path string, d *clangdproc.Daemon) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	if err := w.Add(dir); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-w.Events:
			if filepath.Base(ev.Name) != base {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0 {
				d.Restart()
			}
		case err := <-w.Errors:
			fmt.Fprintln(os.Stderr, "compdb watch error:", err)
		}
	}
}

func signalCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel()
	}()
	return ctx, cancel
}
