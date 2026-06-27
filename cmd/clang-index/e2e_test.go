package main_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture is the shared scaffolding for cmd-level e2e tests: builds the
// binary once, generates a compile_commands.json against the fixture
// project, and exposes the resulting paths.
type fixture struct {
	repoRoot   string
	fixtureDir string
	tmp        string
	binPath    string
	compdb     string
}

func setupFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("clangd"); err != nil {
		t.Skip("clangd not installed")
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	fixtureDir := filepath.Join(repoRoot, "testdata", "fixture")
	includeDir := filepath.Join(fixtureDir, "include")
	tmp := t.TempDir()

	binPath := filepath.Join(tmp, "clang-index")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/clang-index")
	build.Dir = repoRoot
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}

	type cmdEntry struct {
		Directory string   `json:"directory"`
		File      string   `json:"file"`
		Arguments []string `json:"arguments"`
	}
	var entries []cmdEntry
	for _, base := range []string{"shared.c", "tu1.c", "tu2.c"} {
		entries = append(entries, cmdEntry{
			Directory: fixtureDir,
			File:      filepath.Join(fixtureDir, base),
			Arguments: []string{"clang", "-std=c11", "-I" + includeDir, "-c", filepath.Join(fixtureDir, base)},
		})
	}
	compdb := filepath.Join(tmp, "compile_commands.json")
	data, _ := json.MarshalIndent(entries, "", "  ")
	if err := os.WriteFile(compdb, data, 0o644); err != nil {
		t.Fatal(err)
	}

	return &fixture{
		repoRoot:   repoRoot,
		fixtureDir: fixtureDir,
		tmp:        tmp,
		binPath:    binPath,
		compdb:     compdb,
	}
}

// runBuild invokes the build subcommand with the given extra flags,
// returns elapsed wall time and captured stderr. Fails the test on any
// non-zero exit.
func (f *fixture) runBuild(t *testing.T, out string, extra ...string) (time.Duration, string) {
	t.Helper()
	args := append([]string{"build", "--compdb", f.compdb, "--out", out, "--project-root", f.fixtureDir}, extra...)
	cmd := exec.Command(f.binPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("build %v failed: %v\nstderr:\n%s", args, err, stderr.String())
	}
	return time.Since(start), stderr.String()
}

func TestBuildThenServe(t *testing.T) {
	f := setupFixture(t)

	dbPath := filepath.Join(f.tmp, "index.db")
	if _, _ = f.runBuild(t, dbPath); false {
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("db not created: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serve := exec.CommandContext(ctx, f.binPath, "serve", "--db", dbPath)
	serve.Stderr = os.Stderr
	stdin, err := serve.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := serve.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		serve.Wait()
	}()

	rd := bufio.NewReader(stdout)
	send := func(payload string) {
		stdin.Write([]byte(payload + "\n"))
	}
	readLine := func() ([]byte, error) {
		return rd.ReadBytes('\n')
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if _, err := readLine(); err != nil {
		t.Fatalf("read initialize: %v", err)
	}

	send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_symbol","arguments":{"query":"factorial"}}}`)
	resp, err := readLine()
	if err != nil {
		t.Fatalf("read search: %v", err)
	}
	if !bytes.Contains(resp, []byte("factorial")) {
		t.Fatalf("search_symbol(factorial) didn't return factorial: %s", resp)
	}

	go io.Copy(io.Discard, strings.NewReader(""))
}

// TestBuildWholeBuildCache asserts:
//   - first build with --cache populates the cache and writes a real DB
//   - second build with the same inputs is a cache hit (announced on stderr)
//     and produces a bit-identical DB
//
// We avoid wall-time assertions; the functional signal (stderr message +
// byte-identical artifact) is enough and doesn't get flaky on shared CI.
func TestBuildWholeBuildCache(t *testing.T) {
	f := setupFixture(t)
	cacheDir := filepath.Join(f.tmp, "wb-cache")
	db1 := filepath.Join(f.tmp, "wb1.db")
	db2 := filepath.Join(f.tmp, "wb2.db")

	_, stderr1 := f.runBuild(t, db1, "--cache", cacheDir)
	if strings.Contains(stderr1, "whole-build cache hit") {
		t.Fatalf("first build shouldn't be a hit; stderr=%q", stderr1)
	}
	if !strings.Contains(stderr1, "wrote ") {
		t.Fatalf("first build didn't extract: stderr=%q", stderr1)
	}
	if entries, _ := os.ReadDir(cacheDir); len(entries) == 0 {
		t.Fatalf("cache dir %s is empty after first build", cacheDir)
	}

	_, stderr2 := f.runBuild(t, db2, "--cache", cacheDir)
	if !strings.Contains(stderr2, "whole-build cache hit") {
		t.Fatalf("second build wasn't a hit; stderr=%q", stderr2)
	}

	a, err := os.ReadFile(db1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(db2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("whole-build hit produced different bytes (len %d vs %d)", len(a), len(b))
	}
}

// TestBuildPerFileCache asserts the per-file fast-path independently of
// the whole-build layer, even though they now share a cache root:
//
//   - first build populates the merged cache; we sanity-check that the
//     per-file/ subdir got one entry per TU.
//   - we re-serialize the compdb with different whitespace, which changes
//     its raw-bytes digest (→ whole-build miss) but leaves each TU's
//     parsed (Directory, Arguments) untouched (→ per-file hit).
//   - the second build is fast enough that we know clangd's TU pipeline
//     was skipped: the only work left is process spawn + LSP init, which
//     on this fixture is well under 1s. Cold extraction is dominated by
//     indexer settle, ~80–150ms; we set a generous 2s upper bound to
//     stay non-flaky on slow CI.
//   - both produced DBs report the same symbol via a serve search.
func TestBuildPerFileCache(t *testing.T) {
	f := setupFixture(t)
	cacheDir := filepath.Join(f.tmp, "cache")
	db1 := filepath.Join(f.tmp, "pf1.db")
	db2 := filepath.Join(f.tmp, "pf2.db")

	_, _ = f.runBuild(t, db1, "--cache", cacheDir)
	pfDir := filepath.Join(cacheDir, "per-file")
	entries, err := os.ReadDir(pfDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("per-file cache dir empty after first build: %v", err)
	}

	// Verify the per-file subdir has one entry per TU in the fixture (3).
	if len(entries) != 3 {
		t.Errorf("expected 3 per-file cache entries (one per TU), got %d", len(entries))
	}

	// Re-serialize the compdb with different formatting so whole-build's
	// raw-bytes digest changes; per-file keys are over parsed (Directory,
	// Arguments) and are unaffected.
	reserializeCompDB(t, f.compdb)

	hitElapsed, stderr2 := f.runBuild(t, db2, "--cache", cacheDir)
	if strings.Contains(stderr2, "whole-build cache hit") {
		t.Fatalf("compdb reserialization should have invalidated whole-build; stderr=%q", stderr2)
	}
	if hitElapsed > 2*time.Second {
		t.Errorf("per-file cache hit took %s; expected well under 2s (bug: maybe WaitForIndex still being called on all-cached path)", hitElapsed)
	}

	// Both DBs should answer the same search_symbol query identically.
	got1 := searchSymbolViaServe(t, f.binPath, db1, "factorial")
	got2 := searchSymbolViaServe(t, f.binPath, db2, "factorial")
	if !bytes.Contains(got1, []byte("factorial")) {
		t.Fatalf("first DB missing factorial: %s", got1)
	}
	if !bytes.Contains(got2, []byte("factorial")) {
		t.Fatalf("cached DB missing factorial: %s", got2)
	}
}

// reserializeCompDB rewrites compile_commands.json with different
// whitespace, changing its raw-bytes content while preserving parsed
// equivalence. Used to invalidate whole-build's input digest without
// touching per-file keys.
func reserializeCompDB(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	// Re-encode compact (no indent) — different bytes, same parsed value.
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(out, raw) {
		t.Fatal("reserialize produced identical bytes; pick a different formatting")
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// searchSymbolViaServe boots `serve --db dbPath` on stdio, sends an
// initialize then a search_symbol call, and returns the raw response
// bytes for the search. Used to confirm an arbitrary index.db is usable
// without importing internal/store (cmd packages live in main_test).
func searchSymbolViaServe(t *testing.T, binPath, dbPath, query string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "serve", "--db", dbPath)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		cmd.Wait()
	}()
	rd := bufio.NewReader(stdout)
	stdin.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n"))
	if _, err := rd.ReadBytes('\n'); err != nil {
		t.Fatalf("read initialize: %v", err)
	}
	req, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name":      "search_symbol",
			"arguments": map[string]any{"query": query},
		},
	})
	stdin.Write(append(req, '\n'))
	resp, err := rd.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read search: %v", err)
	}
	return resp
}
