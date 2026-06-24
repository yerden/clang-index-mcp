// Command clang-index produces (`build`) or serves (`serve`) a frozen
// SQLite index of a C/C++ project. See clang-index-architecture.md §3.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	mcpsrv "github.com/mark3labs/mcp-go/server"

	"github.com/yerden/clang-index-mcp/internal/cache"
	"github.com/yerden/clang-index-mcp/internal/clangdproc"
	"github.com/yerden/clang-index-mcp/internal/extract"
	"github.com/yerden/clang-index-mcp/internal/mcp"
	"github.com/yerden/clang-index-mcp/internal/store"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: clang-index <build|serve> [flags]")
	fmt.Fprintln(os.Stderr, "  build  produce an index.db from a compile_commands.json snapshot")
	fmt.Fprintln(os.Stderr, "  serve  serve a frozen index.db over MCP (stdio + optional SSE)")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]
	switch sub {
	case "build":
		os.Exit(runBuild(args))
	case "serve":
		os.Exit(runServe(args))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", sub)
		usage()
		os.Exit(2)
	}
}

func runBuild(args []string) int {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	compdb := fs.String("compdb", "", "path to compile_commands.json (required)")
	out := fs.String("out", "index.db", "output index.db path")
	projectRoot := fs.String("project-root", "", "project root (file paths are stored relative to this); default: compdb's directory")
	cacheRoot := fs.String("cache", "", "whole-build cache root (empty = disabled)")
	perFileRoot := fs.String("per-file-cache", "", "per-file cache root (empty = disabled)")
	clangdPath := fs.String("clangd", "clangd", "clangd binary to spawn")
	indexTimeout := fs.Duration("index-timeout", 5*time.Minute, "max time to wait for background indexing to settle")
	clangdJobs := fs.Int("clangd-jobs", 0, "clangd -j=N worker count (0 = clangd's default, ≈ half the logical cores)")
	clangdBoost := fs.Bool("clangd-boost", false, "run clangd's background indexer at normal OS priority instead of the default nice-19 \"background\" — recommended on a dedicated build host")
	_ = fs.Parse(args)

	if *compdb == "" {
		fmt.Fprintln(os.Stderr, "build: --compdb is required")
		return 2
	}
	abs, err := filepath.Abs(*compdb)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		return 1
	}

	ctx, cancel := signalCtx()
	defer cancel()

	// Whole-build cache lookup (architecture §7.1).
	wb, err := cache.NewWholeBuild(*cacheRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build: cache init:", err)
		return 1
	}
	entries, raw, err := extract.LoadCompDB(abs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build: load compdb:", err)
		return 1
	}
	compdbDigest := cache.Sum(raw)
	fileDigests := make(map[string]cache.Digest, len(entries))
	for _, e := range entries {
		fd, err := cache.SumFile(e.AbsFile())
		if err == nil {
			fileDigests[e.AbsFile()] = fd
		}
	}
	inputDigest := cache.InputDigest(compdbDigest, fileDigests)
	if hit, err := wb.Lookup(inputDigest); err == nil {
		if err := copyFile(hit, *out); err != nil {
			fmt.Fprintln(os.Stderr, "build: cache copy:", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "build: whole-build cache hit for %s -> %s\n", inputDigest, *out)
		return 0
	}

	// Per-file cache (architecture §7.2).
	pf, err := cache.NewPerFile(*perFileRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build: per-file cache init:", err)
		return 1
	}

	// `clang-index build` runs disposable extraction; no persistent
	// background-index path is configured here per architecture §6.2.
	clangdOpts := clangdproc.Options{
		Path:               *clangdPath,
		CompileCommandsDir: filepath.Dir(abs),
		Jobs:               *clangdJobs,
	}
	if *clangdBoost {
		clangdOpts.BackgroundIndexPriority = "normal"
	}
	proc, err := clangdproc.Start(ctx, clangdOpts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build: spawn clangd:", err)
		return 1
	}
	defer proc.Stop(context.Background())

	bar := newProgressBar()
	defer bar.Finish()
	proc.OnIndexProgress(bar.reportIndex)

	res, err := extract.Run(ctx, proc.Client(), extract.Options{
		CompDBPath:  abs,
		ProjectRoot: *projectRoot,
		PerFile:     pf,
		WaitForIndex: func(c context.Context) error {
			waitCtx, cancel := context.WithTimeout(c, *indexTimeout)
			defer cancel()
			if err := proc.WaitIndexed(waitCtx); err != nil {
				fmt.Fprintln(os.Stderr, "build: warning: index-settle wait:", err)
			}
			return nil
		},
		OnTUProgress: bar.reportExtract,
	})
	bar.Finish()
	if err != nil {
		fmt.Fprintln(os.Stderr, "build: extract:", err)
		return 1
	}

	tmpOut := *out + ".tmp"
	if err := store.WriteIndexWithFacts(tmpOut, res.Symbols, res.Edges, res.AddressTakes, res.IndirectCallSites); err != nil {
		fmt.Fprintln(os.Stderr, "build: write:", err)
		return 1
	}
	if err := os.Rename(tmpOut, *out); err != nil {
		fmt.Fprintln(os.Stderr, "build: rename:", err)
		return 1
	}

	if err := wb.Put(inputDigest, *out); err != nil {
		fmt.Fprintln(os.Stderr, "build: warning: cache put:", err)
	}

	fmt.Fprintf(os.Stderr, "build: wrote %d symbols, %d edges -> %s\n", len(res.Symbols), len(res.Edges), *out)
	return 0
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dbPath := fs.String("db", "index.db", "path to the frozen index.db")
	httpAddr := fs.String("http", "", "if non-empty, also serve MCP over Streamable HTTP on this address (e.g. :8080)")
	httpPath := fs.String("http-path", "/mcp", "endpoint path for the Streamable HTTP transport")
	_ = fs.Parse(args)

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve: open:", err)
		return 1
	}
	defer st.Close()

	srv := mcp.New(st, "clang-index")

	ctx, cancel := signalCtx()
	defer cancel()

	// Either Streamable HTTP or stdio — never both. Running both at
	// once over the same store has no useful semantics (the two
	// clients would share state without any access-control story) and
	// silently picking the wrong endpoint is exactly the foot-gun
	// we'd rather force the operator to be explicit about.
	if *httpAddr != "" {
		httpSrv := srv.StreamableHTTPServer(mcpsrv.WithEndpointPath(*httpPath))
		go func() {
			defer cancel() // listener exit => unblock main and shut down
			fmt.Fprintf(os.Stderr, "serve: Streamable HTTP listening on %s%s\n", *httpAddr, *httpPath)
			if err := httpSrv.Start(*httpAddr); err != nil && err != http.ErrServerClosed {
				fmt.Fprintln(os.Stderr, "serve: http:", err)
			}
		}()
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = httpSrv.Shutdown(shutCtx)
		return 0
	}

	if err := srv.ServeStdio(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "serve: stdio:", err)
		return 1
	}
	return 0
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

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
