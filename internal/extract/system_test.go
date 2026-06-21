//go:build !no_clangd

package extract

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yerden/clang-index-mcp/internal/clangdproc"
	"github.com/yerden/clang-index-mcp/internal/store"
)

// TestSystemFixture runs the full extract pipeline against the C fixture
// at testdata/fixture. Requires clangd on PATH; skipped otherwise.
func TestSystemFixture(t *testing.T) {
	if _, err := exec.LookPath("clangd"); err != nil {
		t.Skip("clangd not installed")
	}
	fixtureDir, err := filepath.Abs("../../testdata/fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixtureDir); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}

	// Write a fresh compile_commands.json with absolute paths so clangd
	// resolves the include directory correctly regardless of cwd.
	includeDir := filepath.Join(fixtureDir, "include")
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
	compdbPath := filepath.Join(t.TempDir(), "compile_commands.json")
	data, _ := json.MarshalIndent(entries, "", "  ")
	if err := os.WriteFile(compdbPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	proc, err := clangdproc.Start(ctx, clangdproc.Options{
		Path:               "clangd",
		CompileCommandsDir: filepath.Dir(compdbPath),
		Stderr:             os.Stderr,
	})
	if err != nil {
		t.Fatalf("start clangd: %v", err)
	}
	defer proc.Stop(context.Background())

	// Give clangd time to index. Most versions emit progress; if not,
	// the per-TU didOpen below still parses successfully.
	runStart := time.Now()
	res, err := Run(ctx, proc.Client(), Options{
		CompDBPath:  compdbPath,
		ProjectRoot: fixtureDir,
		WaitForIndex: func(c context.Context) error {
			waitCtx, waitCancel := context.WithTimeout(c, 30*time.Second)
			defer waitCancel()
			waitStart := time.Now()
			err := proc.WaitIndexed(waitCtx)
			t.Logf("WaitIndexed returned in %s, err=%v", time.Since(waitStart), err)
			return err
		},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	t.Logf("Run returned in %s, symbols=%d edges=%d", time.Since(runStart), len(res.Symbols), len(res.Edges))

	// Expected symbols are at least: shared_hi, hot_callee, factorial,
	// a_calls_b, b_calls_a, dispatch, tu1_caller, tu1_indirect, square,
	// tu2_caller, leaf, mid, chain_root.
	want := []string{
		"shared_hi", "hot_callee", "factorial",
		"a_calls_b", "b_calls_a", "dispatch",
		"tu1_caller", "tu1_indirect", "square",
		"tu2_caller", "leaf", "mid", "chain_root",
	}
	have := map[string]bool{}
	for _, s := range res.Symbols {
		have[s.Name] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing symbol %q in extraction (got %v)", w, names(res.Symbols))
		}
	}

	// Every callable symbol must have a non-empty signature. Regression
	// guard for gap #2: callees discovered via callHierarchy used to be
	// inserted with Signature="", silently breaking FTS5 parameter-type
	// searches; both the documentSymbol path and the callee path now
	// fall back to textDocument/hover.
	for _, s := range res.Symbols {
		if s.Kind == "Function" && s.Signature == "" {
			t.Errorf("function %q has empty signature; gap #2 regression", s.Name)
		}
	}

	// Gap #1: symbols declared in a header should carry decl_file
	// pointing at that header, even when their definition lives in a .c.
	// shared_hi is the canonical case in the fixture: declared in
	// include/shared.h, defined in shared.c.
	for _, s := range res.Symbols {
		if s.Name == "shared_hi" {
			if s.DeclFile != "include/shared.h" {
				t.Errorf("shared_hi.DeclFile = %q, want include/shared.h", s.DeclFile)
			}
			if s.File != "shared.c" {
				t.Errorf("shared_hi.File = %q, want shared.c", s.File)
			}
		}
		// A file-local (static) symbol like `square` should have an empty
		// DeclFile because there is no separate declaration.
		if s.Name == "square" && s.DeclFile != "" {
			t.Errorf("static square should have empty DeclFile, got %q", s.DeclFile)
		}
	}

	// shared_hi should appear once (cross-TU USR dedup, architecture §11).
	hits := 0
	var sharedHiUSR string
	for _, s := range res.Symbols {
		if s.Name == "shared_hi" {
			hits++
			sharedHiUSR = s.USR
		}
	}
	if hits != 1 {
		t.Errorf("shared_hi appeared %d times, want exactly 1 (USR dedup failed)", hits)
	}
	_ = sharedHiUSR

	// Fan-in: both tu1_caller and tu2_caller should resolve a callee
	// edge into hot_callee.
	fanin := 0
	var hotUSR string
	for _, s := range res.Symbols {
		if s.Name == "hot_callee" {
			hotUSR = s.USR
		}
	}
	for _, e := range res.Edges {
		if e.CalleeUSR == hotUSR {
			fanin++
		}
	}
	if fanin < 2 {
		t.Errorf("expected fan-in ≥2 edges into hot_callee, got %d", fanin)
	}

	// Recursion self-edge: factorial → factorial.
	var factUSR string
	for _, s := range res.Symbols {
		if s.Name == "factorial" {
			factUSR = s.USR
		}
	}
	selfFound := false
	for _, e := range res.Edges {
		if e.CallerUSR == factUSR && e.CalleeUSR == factUSR {
			selfFound = true
		}
	}
	if !selfFound {
		t.Errorf("expected factorial→factorial self-edge")
	}

	// 2-cycle: a_calls_b → b_calls_a → a_calls_b.
	var aUSR, bUSR string
	for _, s := range res.Symbols {
		switch s.Name {
		case "a_calls_b":
			aUSR = s.USR
		case "b_calls_a":
			bUSR = s.USR
		}
	}
	ab, ba := false, false
	for _, e := range res.Edges {
		if e.CallerUSR == aUSR && e.CalleeUSR == bUSR {
			ab = true
		}
		if e.CallerUSR == bUSR && e.CalleeUSR == aUSR {
			ba = true
		}
	}
	if !ab || !ba {
		t.Errorf("expected A↔B cycle edges; ab=%v ba=%v", ab, ba)
	}

	// Function-pointer call: dispatch has no idea what `fn` resolves to,
	// so there must NOT be an edge dispatch → square. (clangd's
	// outgoingCalls *does* surface the literal `square` reference at the
	// `dispatch(square, x)` *call site* — that edge ends up on
	// tu1_indirect→square, which is fine; the gap we care about is the
	// one inside dispatch itself.)
	var dispatchUSR, squareUSR string
	for _, s := range res.Symbols {
		switch s.Name {
		case "dispatch":
			dispatchUSR = s.USR
		case "square":
			squareUSR = s.USR
		}
	}
	for _, e := range res.Edges {
		if e.CallerUSR == dispatchUSR && e.CalleeUSR == squareUSR {
			t.Errorf("unexpected dispatch→square edge: callHierarchy claimed to resolve a function-pointer call it cannot statically resolve")
		}
	}

	// Files should be relative to ProjectRoot.
	for _, s := range res.Symbols {
		if strings.HasPrefix(s.File, "/") {
			t.Errorf("symbol %q has absolute file path %q; should be relative to ProjectRoot", s.Name, s.File)
		}
	}
}

func names(ss []store.Symbol) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name
	}
	return out
}
