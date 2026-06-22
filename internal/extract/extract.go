// Package extract walks a compile_commands.json, drives clangd over LSP
// for each TU, and produces a flat list of symbols + call edges suitable
// for store.WriteIndex.
//
// It owns no clangd lifecycle: the caller hands in an already-initialized
// lsp.Client (typically wrapped by clangdproc.Process). This is what lets
// `clang-index build` and the daemon share the same code path.
//
// Per-TU caching: if a *cache.PerFile is provided, the extractor keys on
// (TU content digest, compile command digest) and skips clangd entirely
// on hits. The DB write itself is always a full rebuild from cached +
// fresh results combined — see architecture §7.2.
package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/yerden/clang-index-mcp/internal/cache"
	"github.com/yerden/clang-index-mcp/internal/lsp"
	"github.com/yerden/clang-index-mcp/internal/store"
)

// Options controls a single extraction run.
type Options struct {
	// CompDBPath is the path to compile_commands.json.
	CompDBPath string

	// ProjectRoot is what file paths get made relative to before being
	// stored in `symbols.file` (architecture §5.2). If empty, defaults to
	// the compdb's directory.
	ProjectRoot string

	// PerFile, if non-nil, memoizes per-TU extraction results.
	PerFile *cache.PerFile

	// WaitForIndex, if non-nil, is called after every TU has been
	// didOpen-ed but before we start asking for symbols / call hierarchy.
	// This is where the caller waits for clangd's background-index to
	// settle; that's needed for cross-TU edges to resolve. clangd's
	// background indexer doesn't start until at least one file is open,
	// so the wait must happen *after* the didOpens, not before.
	WaitForIndex func(context.Context) error
}

// Result is the in-memory bundle ready for store.WriteIndexWithFacts.
type Result struct {
	Symbols           []store.Symbol
	Edges             []store.Edge
	AddressTakes      []store.AddressTake
	IndirectCallSites []store.IndirectCallSite
}

// tuPayload is the JSON shape we round-trip through PerFile cache.
// Address-take and indirect-call-site facts are per-TU, so they cache
// the same way as symbols and edges (architecture §6.5).
type tuPayload struct {
	Symbols           []store.Symbol         `json:"symbols"`
	Edges             []store.Edge           `json:"edges"`
	AddressTakes      []addressTakeFact      `json:"address_takes,omitempty"`
	IndirectCallSites []indirectCallSiteFact `json:"indirect_call_sites,omitempty"`
}

// Run walks the compdb, queries clangd for each TU, and returns the
// merged Result. Symbols are deduped by USR (last writer wins for fields
// other than USR/Name/Kind, which are stable). Edges are deduped on the
// (caller, callee) USR pair.
func Run(ctx context.Context, cli *lsp.Client, opts Options) (*Result, error) {
	entries, _, err := LoadCompDB(opts.CompDBPath)
	if err != nil {
		return nil, fmt.Errorf("load compdb: %w", err)
	}

	projectRoot := opts.ProjectRoot
	if projectRoot == "" {
		projectRoot = filepath.Dir(opts.CompDBPath)
	}
	projectRoot, _ = filepath.Abs(projectRoot)

	// First pass: didOpen everything that's NOT already in the per-file
	// cache. This is what triggers clangd's background index — without an
	// open file it sits idle and never emits $/progress, so any
	// WaitForIndex would hang. We didClose them all at the end of Run.
	type tuPlan struct {
		entry  CompDBEntry
		key    cache.PerFileKey
		cached *tuPayload
	}
	plans := make([]tuPlan, 0, len(entries))
	openedURIs := make([]string, 0, len(entries))
	for _, entry := range entries {
		absFile := entry.AbsFile()
		fileDigest, err := cache.SumFile(absFile)
		if err != nil {
			continue
		}
		key := cache.PerFileKey{FileDigest: fileDigest, CommandDigest: commandDigest(entry)}
		plan := tuPlan{entry: entry, key: key}
		if opts.PerFile != nil {
			if hit, err := opts.PerFile.Lookup(key); err == nil {
				var p tuPayload
				if err := json.Unmarshal(hit.Payload, &p); err == nil {
					plan.cached = &p
					plans = append(plans, plan)
					continue
				}
			}
		}
		uri, err := openTU(cli, absFile)
		if err != nil {
			return nil, fmt.Errorf("didOpen %s: %w", absFile, err)
		}
		openedURIs = append(openedURIs, uri)
		plans = append(plans, plan)
	}
	openedSet := make(map[string]bool, len(openedURIs))
	for _, u := range openedURIs {
		openedSet[u] = true
	}
	// ensureOpened lazily opens a file we haven't seen yet. This is the
	// fix for static inline (and any other header-defined) callees:
	// clangd's textDocument/symbolInfo only resolves USRs for files
	// that have been didOpen'd, so a callee whose definition lives in
	// an unopened header silently disappears from the index. By opening
	// it on demand the first time we hit such a callee, every later TU
	// also benefits from the open (the set is shared across TUs).
	ensureOpened := func(targetURI string) {
		if openedSet[targetURI] {
			return
		}
		absPath := uriToPath(targetURI)
		if absPath == "" {
			return
		}
		if _, err := openTU(cli, absPath); err != nil {
			return
		}
		openedSet[targetURI] = true
		openedURIs = append(openedURIs, targetURI)
	}
	defer func() {
		for _, uri := range openedURIs {
			_ = cli.Notify("textDocument/didClose", map[string]any{
				"textDocument": map[string]any{"uri": uri},
			})
		}
	}()

	// Only wait for clangd's background index if we actually opened at
	// least one TU. With a full per-file cache hit there's nothing to
	// open, so clangd's indexer never starts and WaitForIndex would just
	// block until its deadline. Cached TUs already carry their edges, so
	// no fresh index lookup is needed either.
	if opts.WaitForIndex != nil && len(openedURIs) > 0 {
		if err := opts.WaitForIndex(ctx); err != nil {
			// non-fatal: degrade to whatever clangd has so far
		}
	}

	symbolsByUSR := map[string]store.Symbol{}
	edgeSet := map[string]struct{}{}
	var edges []store.Edge
	var addressTakes []store.AddressTake
	var indirectCallSites []store.IndirectCallSite

	addPayload := func(p *tuPayload) {
		for _, s := range p.Symbols {
			if s.USR == "" {
				continue
			}
			// Always relativize: cache entries may have been written by a
			// previous run with the same ProjectRoot; even so, defensively
			// normalize once more here.
			s.File = relative(projectRoot, s.File)
			symbolsByUSR[s.USR] = s
		}
		for _, e := range p.Edges {
			if e.CallerUSR == "" || e.CalleeUSR == "" {
				continue
			}
			k := e.CallerUSR + "\x00" + e.CalleeUSR
			if _, dup := edgeSet[k]; dup {
				continue
			}
			edgeSet[k] = struct{}{}
			edges = append(edges, e)
		}
		for _, a := range p.AddressTakes {
			if a.FunctionUSR == "" {
				continue
			}
			addressTakes = append(addressTakes, store.AddressTake{
				FunctionUSR:   a.FunctionUSR,
				TakenAtFile:   relative(projectRoot, a.TakenAtFile),
				TakenAtLine:   a.TakenAtLine,
				FnPtrType:     a.FnPtrType,
				Category:      a.Category,
				ContextDetail: a.ContextDetail,
			})
		}
		for _, s := range p.IndirectCallSites {
			if s.CallerUSR == "" {
				continue
			}
			indirectCallSites = append(indirectCallSites, store.IndirectCallSite{
				CallerUSR:  s.CallerUSR,
				File:       relative(projectRoot, s.File),
				Line:       s.Line,
				CalleeType: s.CalleeType,
				CalleeExpr: s.CalleeExpr,
			})
		}
	}

	for _, plan := range plans {
		if plan.cached != nil {
			addPayload(plan.cached)
			continue
		}
		payload, err := extractTU(ctx, cli, plan.entry.AbsFile(), projectRoot, ensureOpened)
		if err != nil {
			return nil, fmt.Errorf("extract %s: %w", plan.entry.AbsFile(), err)
		}
		if opts.PerFile != nil {
			b, _ := json.Marshal(payload)
			_ = opts.PerFile.Put(plan.key, &cache.PerFileEntry{Payload: b})
		}
		addPayload(payload)
	}

	out := &Result{
		Edges:             edges,
		AddressTakes:      addressTakes,
		IndirectCallSites: indirectCallSites,
	}
	for _, s := range symbolsByUSR {
		out.Symbols = append(out.Symbols, s)
	}
	return out, nil
}

func commandDigest(e CompDBEntry) cache.Digest {
	parts := make([]string, 0, len(e.Arguments)+2)
	parts = append(parts, e.Directory)
	if len(e.Arguments) > 0 {
		parts = append(parts, e.Arguments...)
	} else {
		parts = append(parts, e.Command)
	}
	return cache.SumStrings(parts...)
}

func relative(root, p string) string {
	if !filepath.IsAbs(p) {
		return filepath.ToSlash(p)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

// openTU sends a didOpen for absFile (read from disk) and returns the
// URI used so the caller can later didClose it.
func openTU(cli *lsp.Client, absFile string) (string, error) {
	src, err := os.ReadFile(absFile)
	if err != nil {
		return "", err
	}
	uri := pathToURI(absFile)
	if err := cli.Notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": detectLanguage(absFile),
			"version":    1,
			"text":       string(src),
		},
	}); err != nil {
		return "", err
	}
	return uri, nil
}

// extractTU drives clangd for a single translation unit. The file must
// already be open (see openTU); Run handles open/close lifecycle.
// ensureOpened is a callback into Run that lazily opens header files
// when a callee's USR resolution fails — clangd's symbolInfo only
// answers for didOpen'd files, so static inline functions and any
// other header-defined callees need their header opened first.
func extractTU(ctx context.Context, cli *lsp.Client, absFile, projectRoot string, ensureOpened func(uri string)) (*tuPayload, error) {
	uri := pathToURI(absFile)

	rawSyms, err := cli.Call(ctx, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": uri},
	})
	if err != nil {
		return nil, fmt.Errorf("documentSymbol: %w", err)
	}

	docSyms, err := decodeDocumentSymbols(rawSyms, uri)
	if err != nil {
		return nil, err
	}

	// Build the (name → identifier-position) map used by the AST
	// walker. AST FunctionDecl nodes carry only the whole-declaration
	// Range; symbolInfo needs the precise identifier position.
	nameToNamePos := make(map[string]Position, len(docSyms))
	for _, ds := range docSyms {
		if isCallable(ds.Kind) {
			if _, exists := nameToNamePos[ds.Name]; !exists {
				nameToNamePos[ds.Name] = ds.SelectionRange.Start
			}
		}
	}

	// Tier 2-style fact extraction (architecture §6.5). Best-effort:
	// returns empty sets on older clangd builds without
	// textDocument/ast support, and the rest of extraction proceeds.
	var addressTakes []addressTakeFact
	var indirectCallSites []indirectCallSiteFact
	if root, _ := fetchAST(ctx, cli, uri); root != nil {
		w := newWalker(ctx, cli, uri)
		w.nameToNamePos = nameToNamePos
		w.walk(*root)
		addressTakes = w.addressTakes
		indirectCallSites = w.indirectCallSites
	}

	out := &tuPayload{}
	seen := map[string]bool{}

	for _, ds := range docSyms {
		ds := ds // capture
		usr, err := symbolUSR(ctx, cli, uri, ds.SelectionRange.Start)
		if err != nil || usr == "" {
			continue
		}
		if !seen[usr] {
			seen[usr] = true
			sig := ds.Detail
			if !looksLikeSignature(sig, ds.Name) {
				if hsig := hoverSignature(ctx, cli, uri, ds.SelectionRange.Start); hsig != "" {
					sig = hsig
				}
			}
			declFile, declLine := declarationLocation(ctx, cli, uri, ds.SelectionRange.Start, absFile, ds.SelectionRange.Start.Line+1, projectRoot)
			out.Symbols = append(out.Symbols, store.Symbol{
				USR:       usr,
				Name:      ds.Name,
				Kind:      kindName(ds.Kind),
				File:      relative(projectRoot, absFile),
				Line:      ds.SelectionRange.Start.Line + 1,
				DeclFile:  declFile,
				DeclLine:  declLine,
				Signature: sig,
			})
		}

		// Outgoing calls — only meaningful for function-like symbols.
		if !isCallable(ds.Kind) {
			continue
		}
		callees, err := outgoingCalls(ctx, cli, uri, ds.SelectionRange.Start)
		if err != nil {
			// callHierarchy may not be supported for this point; tolerate.
			continue
		}
		for _, callee := range callees {
			calleeUSR, err := symbolUSR(ctx, cli, callee.URI, callee.Pos)
			if err != nil || calleeUSR == "" {
				if ensureOpened != nil {
					ensureOpened(callee.URI)
					calleeUSR, err = symbolUSR(ctx, cli, callee.URI, callee.Pos)
				}
				if err != nil || calleeUSR == "" {
					continue
				}
			}
			if !seen[calleeUSR] {
				seen[calleeUSR] = true
				calleeAbs := uriToPath(callee.URI)
				declFile, declLine := declarationLocation(ctx, cli, callee.URI, callee.Pos, calleeAbs, callee.Pos.Line+1, projectRoot)
				out.Symbols = append(out.Symbols, store.Symbol{
					USR:       calleeUSR,
					Name:      callee.Name,
					Kind:      callee.Kind,
					File:      relative(projectRoot, calleeAbs),
					Line:      callee.Pos.Line + 1,
					DeclFile:  declFile,
					DeclLine:  declLine,
					Signature: hoverSignature(ctx, cli, callee.URI, callee.Pos),
				})
			}
			out.Edges = append(out.Edges, store.Edge{CallerUSR: usr, CalleeUSR: calleeUSR})
		}
	}

	out.AddressTakes = addressTakes
	out.IndirectCallSites = indirectCallSites
	return out, nil
}

// flatDocumentSymbol is the normalized shape we work with — either the
// hierarchical DocumentSymbol form (LSP 3.16+) or the legacy
// SymbolInformation form, flattened.
type flatDocumentSymbol struct {
	Name           string
	Detail         string
	Kind           int
	SelectionRange Range
	Range          Range
}

// Position is the LSP position (0-indexed line/character).
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is the LSP range.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// hierSymbol mirrors the LSP DocumentSymbol shape (3.16+).
type hierSymbol struct {
	Name           string       `json:"name"`
	Detail         string       `json:"detail"`
	Kind           int          `json:"kind"`
	Range          Range        `json:"range"`
	SelectionRange Range        `json:"selectionRange"`
	Children       []hierSymbol `json:"children"`
}

// legacySymbol mirrors the older SymbolInformation shape.
type legacySymbol struct {
	Name     string `json:"name"`
	Kind     int    `json:"kind"`
	Location struct {
		URI   string `json:"uri"`
		Range Range  `json:"range"`
	} `json:"location"`
}

func decodeDocumentSymbols(raw json.RawMessage, uri string) ([]flatDocumentSymbol, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	// Probe the first element: hierarchical entries have `selectionRange`,
	// legacy entries have `location`.
	var probe []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("decode documentSymbol: %w", err)
	}
	if len(probe) == 0 {
		return nil, nil
	}
	if _, hier := probe[0]["selectionRange"]; hier {
		var top []hierSymbol
		if err := json.Unmarshal(raw, &top); err != nil {
			return nil, fmt.Errorf("decode hierarchical documentSymbol: %w", err)
		}
		var out []flatDocumentSymbol
		var walk func(items []hierSymbol)
		walk = func(items []hierSymbol) {
			for _, it := range items {
				out = append(out, flatDocumentSymbol{
					Name: it.Name, Detail: it.Detail, Kind: it.Kind,
					Range: it.Range, SelectionRange: it.SelectionRange,
				})
				walk(it.Children)
			}
		}
		walk(top)
		return out, nil
	}
	var legacy []legacySymbol
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return nil, fmt.Errorf("decode legacy documentSymbol: %w", err)
	}
	out := make([]flatDocumentSymbol, 0, len(legacy))
	for _, l := range legacy {
		if l.Location.URI != uri {
			continue
		}
		out = append(out, flatDocumentSymbol{
			Name: l.Name, Kind: l.Kind,
			Range: l.Location.Range, SelectionRange: l.Location.Range,
		})
	}
	return out, nil
}

// looksLikeSignature accepts clangd's documentSymbol detail field if
// it's substantive enough to keep — defined here as "non-empty and
// containing the symbol's name." Bare parameter-list shapes like
// "int (FILE *, char *, size_t)" pass; anonymous or stripped details
// don't, so we drop down to hover.
func looksLikeSignature(detail, name string) bool {
	if detail == "" {
		return false
	}
	// detail forms like "int (int)" don't contain the name; accept them
	// anyway since they're informative for parameter-type queries.
	return true
}

// hoverSignature returns the canonical declaration line for the symbol
// at uri+pos, parsed from clangd's textDocument/hover. Returns "" on
// any failure — callers fall back to whatever they had.
//
// clangd's hover content is plaintext with the C/C++ declaration on a
// trailing line, after blank-separated metadata blocks (return type,
// parameters, docstring). Taking the last non-empty line is robust:
// even older clangd builds that omit the metadata still put the
// declaration there.
func hoverSignature(ctx context.Context, cli *lsp.Client, uri string, pos Position) string {
	raw, err := cli.Call(ctx, "textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     pos,
	})
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var resp struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ""
	}
	return extractSignatureFromHover(resp.Contents)
}

// extractSignatureFromHover decodes clangd's hover `contents` (which
// may be MarkupContent, MarkedString, or MarkedString[]) and returns
// the trailing declaration line.
func extractSignatureFromHover(contents json.RawMessage) string {
	if len(contents) == 0 {
		return ""
	}
	// MarkupContent: {kind, value}
	var markup struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(contents, &markup); err == nil && markup.Value != "" {
		return trailingNonEmptyLine(markup.Value)
	}
	// MarkedString[]
	var arr []json.RawMessage
	if err := json.Unmarshal(contents, &arr); err == nil {
		var last string
		for _, item := range arr {
			var ms struct {
				Language string `json:"language"`
				Value    string `json:"value"`
			}
			if err := json.Unmarshal(item, &ms); err == nil && ms.Value != "" {
				last = trailingNonEmptyLine(ms.Value)
				continue
			}
			var s string
			if err := json.Unmarshal(item, &s); err == nil && s != "" {
				last = trailingNonEmptyLine(s)
			}
		}
		return last
	}
	// Bare MarkedString (plain string)
	var s string
	if err := json.Unmarshal(contents, &s); err == nil {
		return trailingNonEmptyLine(s)
	}
	return ""
}

func trailingNonEmptyLine(s string) string {
	var last string
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Skip markdown code fences.
		if strings.HasPrefix(line, "```") {
			continue
		}
		last = line
	}
	return strings.TrimSuffix(last, ";")
}

// declarationLocation queries textDocument/declaration to find where
// the symbol at uri+pos is *declared* (typically a header) as opposed
// to *defined* (where we already are, at defAbsPath:defLine). When the
// declaration coincides with the definition — e.g. a static function
// defined and used in one TU, or any symbol without a forward declaration
// — we return ("", 0) so downstream code can treat that as "no separate
// declaration location."
//
// LSP textDocument/declaration may return null, a single Location, an
// array of Locations, or LocationLink[]. We handle all three shapes;
// the first usable location wins.
func declarationLocation(ctx context.Context, cli *lsp.Client, uri string, pos Position, defAbsPath string, defLine int, projectRoot string) (string, int) {
	raw, err := cli.Call(ctx, "textDocument/declaration", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     pos,
	})
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return "", 0
	}
	declURI, declLine, ok := decodeLocationResult(raw)
	if !ok {
		return "", 0
	}
	declAbs := uriToPath(declURI)
	if declAbs == defAbsPath && declLine == defLine {
		return "", 0
	}
	return relative(projectRoot, declAbs), declLine
}

// decodeLocationResult handles the LSP "go-to-X" result polymorphism:
// Location | Location[] | LocationLink[] | null. Returns the first
// resolvable (uri, line) pair, with line 1-based.
func decodeLocationResult(raw json.RawMessage) (uri string, line int, ok bool) {
	// Single Location: {uri, range}
	var single struct {
		URI   string `json:"uri"`
		Range Range  `json:"range"`
	}
	if err := json.Unmarshal(raw, &single); err == nil && single.URI != "" {
		return single.URI, single.Range.Start.Line + 1, true
	}
	// Array — either Location[] or LocationLink[].
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) == 0 {
		return "", 0, false
	}
	for _, item := range arr {
		var loc struct {
			URI   string `json:"uri"`
			Range Range  `json:"range"`
		}
		if err := json.Unmarshal(item, &loc); err == nil && loc.URI != "" {
			return loc.URI, loc.Range.Start.Line + 1, true
		}
		var link struct {
			TargetURI            string `json:"targetUri"`
			TargetRange          Range  `json:"targetRange"`
			TargetSelectionRange Range  `json:"targetSelectionRange"`
		}
		if err := json.Unmarshal(item, &link); err == nil && link.TargetURI != "" {
			start := link.TargetSelectionRange.Start
			if start == (Position{}) {
				start = link.TargetRange.Start
			}
			return link.TargetURI, start.Line + 1, true
		}
	}
	return "", 0, false
}

// symbolUSR uses clangd's textDocument/symbolInfo extension to recover
// the USR for a symbol at uri+pos. Standard LSP doesn't expose USRs.
func symbolUSR(ctx context.Context, cli *lsp.Client, uri string, pos Position) (string, error) {
	raw, err := cli.Call(ctx, "textDocument/symbolInfo", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     pos,
	})
	if err != nil {
		return "", err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var arr []struct {
		USR  string `json:"usr"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		return arr[0].USR, nil
	}
	var one struct {
		USR string `json:"usr"`
	}
	if err := json.Unmarshal(raw, &one); err != nil {
		return "", err
	}
	return one.USR, nil
}

// calleeRef is one resolved outgoing-call target.
type calleeRef struct {
	URI  string
	Pos  Position
	Name string
	Kind string
}

// outgoingCalls returns the direct outgoing calls from the symbol at
// uri+pos. Uses textDocument/prepareCallHierarchy +
// callHierarchy/outgoingCalls.
func outgoingCalls(ctx context.Context, cli *lsp.Client, uri string, pos Position) ([]calleeRef, error) {
	raw, err := cli.Call(ctx, "textDocument/prepareCallHierarchy", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     pos,
	})
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	var out []calleeRef
	for _, it := range items {
		raw, err := cli.Call(ctx, "callHierarchy/outgoingCalls", map[string]any{"item": json.RawMessage(it)})
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var calls []struct {
			To struct {
				Name           string `json:"name"`
				Kind           int    `json:"kind"`
				URI            string `json:"uri"`
				SelectionRange Range  `json:"selectionRange"`
			} `json:"to"`
		}
		if err := json.Unmarshal(raw, &calls); err != nil {
			return nil, err
		}
		for _, c := range calls {
			out = append(out, calleeRef{
				URI:  c.To.URI,
				Pos:  c.To.SelectionRange.Start,
				Name: c.To.Name,
				Kind: kindName(c.To.Kind),
			})
		}
	}
	return out, nil
}

// kindName maps the LSP SymbolKind numeric value to a stable string used
// in the symbols.kind column. We don't enumerate every kind — unknowns
// get rendered as "Unknown(N)".
func kindName(k int) string {
	switch k {
	case 5:
		return "Class"
	case 6:
		return "Method"
	case 7:
		return "Property"
	case 9:
		return "Constructor"
	case 12:
		return "Function"
	case 13:
		return "Variable"
	case 14:
		return "Constant"
	case 22:
		return "Struct"
	case 23:
		return "Event"
	case 26:
		return "TypeParameter"
	case 10:
		return "Enum"
	case 11:
		return "Interface"
	case 8:
		return "Field"
	default:
		return fmt.Sprintf("Unknown(%d)", k)
	}
}

func isCallable(k int) bool {
	return k == 12 /*Function*/ || k == 6 /*Method*/ || k == 9 /*Constructor*/
}

// detectLanguage picks the languageId that clangd accepts.
func detectLanguage(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".c", ".h":
		return "c"
	case ".m":
		return "objective-c"
	case ".mm":
		return "objective-cpp"
	default:
		return "cpp"
	}
}

func pathToURI(absPath string) string {
	u := url.URL{Scheme: "file", Path: absPath}
	return u.String()
}

func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return uri
	}
	if u.Scheme != "file" {
		return uri
	}
	return u.Path
}

