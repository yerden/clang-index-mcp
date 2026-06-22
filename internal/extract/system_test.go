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
		// Static inline functions defined in headers are only surfaced
		// when their header is didOpen'd before symbolInfo runs.
		// Regression guard for that fix.
		"inline_doubled",
		// Helpers added for the address-take precedence test.
		"assert_eq", "tu1_compared",
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

	// Static inline functions: the only direct edge tu1_caller→
	// inline_doubled should exist. inline_doubled itself should record
	// the header (include/shared.h) as its definition file.
	var tu1CallerUSR, inlineDoubledUSR string
	for _, s := range res.Symbols {
		switch s.Name {
		case "tu1_caller":
			tu1CallerUSR = s.USR
		case "inline_doubled":
			inlineDoubledUSR = s.USR
			if s.File != "include/shared.h" {
				t.Errorf("inline_doubled.File = %q, want include/shared.h", s.File)
			}
		}
	}
	hasInlineEdge := false
	for _, e := range res.Edges {
		if e.CallerUSR == tu1CallerUSR && e.CalleeUSR == inlineDoubledUSR {
			hasInlineEdge = true
			break
		}
	}
	if !hasInlineEdge {
		t.Errorf("expected tu1_caller→inline_doubled edge (static inline header callee)")
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

	// Address-take precedence (architecture §6.5):
	//   - square is passed to dispatch(square, x) → arg_to:dispatch#0
	//   - square is passed to assert_eq(square, p) → arg_to:assert_eq#0
	//   - p == square is a comparison → compared
	// The 'compared' case overrides the surrounding `+` arithmetic;
	// the assert_eq arg case overrides any compared classification that
	// might be inferred from the assertion's body.
	wantCategories := map[string]bool{
		"compared":             false,
		"arg_to:dispatch#0":    false,
		"arg_to:assert_eq#0":   false,
	}
	for _, a := range res.AddressTakes {
		if a.FunctionUSR != squareUSR {
			continue
		}
		key := a.Category
		if a.ContextDetail != "" {
			key = a.Category + ":" + a.ContextDetail
		}
		if _, ok := wantCategories[key]; ok {
			wantCategories[key] = true
		}
		if a.TakenAtLine == 0 {
			t.Errorf("address-take site for square has zero line: %+v", a)
		}
		if a.FnPtrType == "" {
			t.Errorf("address-take for square missing fn_ptr_type: %+v", a)
		}
	}
	for k, found := range wantCategories {
		if !found {
			t.Errorf("expected address-take of square with category=%q, not found", k)
		}
	}

	// Indirect call site (architecture §6.5): dispatch's body has an
	// indirect call — `fn(x)`. Assert its presence with the correct
	// callee_type and callee_expr.
	foundDispatchSite := false
	for _, s := range res.IndirectCallSites {
		if s.CallerUSR != dispatchUSR {
			continue
		}
		if s.CalleeType != "int (*)(int)" {
			t.Errorf("dispatch indirect call site has callee_type=%q, want \"int (*)(int)\"", s.CalleeType)
		}
		if s.CalleeExpr != "fn" {
			t.Errorf("dispatch indirect call site has callee_expr=%q, want \"fn\"", s.CalleeExpr)
		}
		foundDispatchSite = true
	}
	if !foundDispatchSite {
		t.Errorf("expected an indirect_call_site row for dispatch, got %+v", res.IndirectCallSites)
	}

	// Gap round 2 — designated-init field name (Gap 2): tu2_callback is
	// registered via `.cb = tu2_callback` in tu2.c and must record
	// `stored_in:struct ops_t.cb`, NOT `<struct>.<init>` and NOT
	// "<init>" alone.
	var tu2CallbackUSR string
	for _, s := range res.Symbols {
		if s.Name == "tu2_callback" {
			tu2CallbackUSR = s.USR
			break
		}
	}
	foundDesignatedField := false
	for _, a := range res.AddressTakes {
		if a.FunctionUSR != tu2CallbackUSR {
			continue
		}
		if a.Category == store.CategoryStoredIn {
			if a.ContextDetail != "struct ops_t.cb" {
				t.Errorf("tu2_callback stored_in detail = %q, want \"struct ops_t.cb\"", a.ContextDetail)
			}
			if a.FnPtrType != "int (*)(int)" {
				t.Errorf("tu2_callback fn_ptr_type = %q, want canonical \"int (*)(int)\"", a.FnPtrType)
			}
			foundDesignatedField = true
		}
	}
	if !foundDesignatedField {
		t.Errorf("expected a stored_in address-take for tu2_callback with field name recovered")
	}

	// Gap round 2 — typedef canonicalization (Gap 1): ops_dispatch's
	// indirect call site reads `o->cb`, whose static type spelling is
	// `cb_t *`. The walker must canonicalize via the shared typedef
	// table so address_takes.fn_ptr_type and
	// indirect_call_sites.callee_type land on the same string —
	// otherwise the documented join-by-type workflow silently breaks.
	var opsDispatchUSR string
	for _, s := range res.Symbols {
		if s.Name == "ops_dispatch" {
			opsDispatchUSR = s.USR
			break
		}
	}
	foundCanonicalOpsICS := false
	for _, s := range res.IndirectCallSites {
		if s.CallerUSR != opsDispatchUSR {
			continue
		}
		if s.CalleeType != "int (*)(int)" {
			t.Errorf("ops_dispatch ICS callee_type = %q, want canonical \"int (*)(int)\" (cb_t typedef must be expanded)", s.CalleeType)
		}
		if !strings.Contains(s.CalleeExpr, ".cb") {
			t.Errorf("ops_dispatch ICS callee_expr = %q, want to contain \".cb\"", s.CalleeExpr)
		}
		foundCanonicalOpsICS = true
	}
	if !foundCanonicalOpsICS {
		t.Errorf("expected an indirect_call_site for ops_dispatch")
	}
}

func names(ss []store.Symbol) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name
	}
	return out
}
