package extract

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/yerden/clang-index-mcp/internal/lsp"
)

// astNode is the decoded shape of clangd's textDocument/ast response.
// clangd 15+ returns this for any TU; nodes carry both a short Kind
// (e.g. "Call", "DeclRef") and the full clang `Decl::dump()` line in
// Arcana, which is what we mine for static expression types and the
// decl kind referenced by a DeclRefExpr.
type astNode struct {
	Kind     string    `json:"kind"`
	Detail   string    `json:"detail"`
	Arcana   string    `json:"arcana"`
	Role     string    `json:"role"`
	Range    Range     `json:"range"`
	Children []astNode `json:"children"`
}

// indirectSite is one CallExpr whose callee resolves to something other
// than a direct FunctionDecl reference — typically a function-pointer-
// typed parameter or variable.
type indirectSite struct {
	CallerUSR string `json:"caller_usr"`
	Type      string `json:"type"` // function-pointer type, e.g. "int (*)(int)"
}

// addressTakenRef is one use of a function's address in a non-call
// context: passed as an argument, assigned to a variable, taken with
// unary &, etc.
type addressTakenRef struct {
	FunctionUSR string `json:"function_usr"`
	Type        string `json:"type"`
}

// fetchAST asks clangd for the full TU AST. clangd returns null if the
// extension is unsupported (older builds); we tolerate that and return
// nil so callers can skip Tier 2.
func fetchAST(ctx context.Context, cli *lsp.Client, uri string) (*astNode, error) {
	raw, err := cli.Call(ctx, "textDocument/ast", map[string]any{
		"textDocument": map[string]any{"uri": uri},
	})
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return nil, err
	}
	var n astNode
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// walker accumulates indirect-call sites and address-taken refs while
// walking one TU's AST.
type walker struct {
	ctx context.Context
	cli *lsp.Client
	uri string

	enclosingFnUSR string

	// nameToNamePos maps a function's name (as seen in AST node Detail)
	// to its identifier SelectionRange.Start, harvested from
	// documentSymbol. AST nodes' own Range covers the whole declaration,
	// which is not where clangd's symbolInfo answers.
	nameToNamePos map[string]Position

	indirect     []indirectSite
	addressTaken []addressTakenRef

	// USR cache so we don't re-query symbolInfo at the same position.
	usrCache map[Position]string
}

func newWalker(ctx context.Context, cli *lsp.Client, uri string) *walker {
	return &walker{ctx: ctx, cli: cli, uri: uri, usrCache: map[Position]string{}}
}

// walk descends n recursively. inCalleeSubtree is true for any node
// reached only through the child-0 chain of an enclosing CallExpr, so
// a DeclRefExpr that names a function in that position is part of a
// direct call and must NOT be counted as address-taken.
func (w *walker) walk(n astNode, inCalleeSubtree bool) {
	switch n.Kind {
	case "Function":
		// FunctionDecl — entering one updates the enclosing-function
		// context for any indirect site we find inside. The AST node's
		// own Range covers the whole declaration; the identifier
		// position lives in the docSyms map we were given.
		if n.Role == "declaration" {
			pos, ok := w.nameToNamePos[n.Detail]
			if !ok {
				break
			}
			usr := w.resolveUSR(pos)
			if usr != "" {
				save := w.enclosingFnUSR
				w.enclosingFnUSR = usr
				for i, ch := range n.Children {
					w.walk(ch, childInCalleeSubtree(n, i, inCalleeSubtree))
				}
				w.enclosingFnUSR = save
				return
			}
		}
	case "Call":
		w.handleCall(n)
	case "DeclRef":
		if referencesFunction(n.Arcana) && !inCalleeSubtree {
			usr := w.resolveUSR(n.Range.Start)
			if usr != "" {
				w.addressTaken = append(w.addressTaken, addressTakenRef{
					FunctionUSR: usr,
					Type:        functionPointerType(n.Arcana),
				})
			}
		}
	}
	for i, ch := range n.Children {
		w.walk(ch, childInCalleeSubtree(n, i, inCalleeSubtree))
	}
}

// childInCalleeSubtree reports whether the i-th child of parent should
// inherit "we're inside a CallExpr's callee subtree" status.
//
// Rule: a CallExpr resets the flag for every child. Child 0 (the
// callee) gets true; all argument children get false even if the
// surrounding context was a callee subtree. For non-Call nodes, the
// flag is propagated unchanged.
func childInCalleeSubtree(parent astNode, i int, inherited bool) bool {
	if parent.Kind == "Call" {
		return i == 0
	}
	return inherited
}

// handleCall classifies one CallExpr and records an indirect-call site
// when the callee isn't a direct FunctionDecl reference.
func (w *walker) handleCall(n astNode) {
	if len(n.Children) == 0 || w.enclosingFnUSR == "" {
		return
	}
	callee := n.Children[0]
	stripped := drillThroughCastWrappers(callee)
	if stripped.Kind == "DeclRef" && referencesFunction(stripped.Arcana) {
		return // direct call — callHierarchy already covers it
	}
	t := firstQuotedString(callee.Arcana)
	if t == "" {
		return
	}
	w.indirect = append(w.indirect, indirectSite{
		CallerUSR: w.enclosingFnUSR,
		Type:      normalizeFnPtrType(t),
	})
}

// drillThroughCastWrappers peels off the AST nodes that clangd inserts
// between a CallExpr and its underlying DeclRefExpr — ImplicitCast for
// FunctionToPointerDecay, Paren for parenthesized callees, CStyleCast
// for explicit casts. None of these affect whether the call is direct.
func drillThroughCastWrappers(n astNode) astNode {
	for {
		switch n.Kind {
		case "ImplicitCast", "Paren", "CStyleCast":
			if len(n.Children) == 0 {
				return n
			}
			n = n.Children[0]
		default:
			return n
		}
	}
}

// resolveUSR consults clangd's symbolInfo for a position, caching the
// result per Position. Used both to identify the enclosing function and
// to identify the target of a DeclRefExpr.
func (w *walker) resolveUSR(p Position) string {
	if cached, ok := w.usrCache[p]; ok {
		return cached
	}
	usr, _ := symbolUSR(w.ctx, w.cli, w.uri, p)
	w.usrCache[p] = usr
	return usr
}

// referencesFunction reports whether an arcana line denotes a
// DeclRefExpr (or similar) that points at a function-like decl. clangd
// puts the referenced decl kind on the same line as ` Function 0x...`,
// ` CXXMethod 0x...`, etc., so substring matching on that is reliable
// enough.
var fnRefRE = regexp.MustCompile(` (Function|CXXMethod|CXXConstructor|CXXDestructor) 0x[0-9a-f]+`)

func referencesFunction(arcana string) bool {
	return fnRefRE.MatchString(arcana)
}

// firstQuotedString extracts the static type of an expression from the
// arcana. clang dumps types in one of two shapes:
//
//	'int (int)'                    (no typedef)
//	'op_t':'int (*)(int)'          (typedef + canonical)
//
// In the typedef'd case we want the *canonical* type for matching,
// since the address-taken side of the synthesis sees the canonical form
// after function-to-pointer decay. So we prefer the second quoted
// substring whenever both are present.
var typedefCanonicalRE = regexp.MustCompile(`'([^']*)':'([^']*)'`)
var firstQuoteRE = regexp.MustCompile(`'([^']*)'`)

func firstQuotedString(arcana string) string {
	if m := typedefCanonicalRE.FindStringSubmatch(arcana); m != nil {
		return m[2]
	}
	if m := firstQuoteRE.FindStringSubmatch(arcana); m != nil {
		return m[1]
	}
	return ""
}

// functionPointerType derives a function-pointer type from a DeclRefExpr
// arcana that refers to a Function. Two pieces are useful:
//
//  1. The expression's own static type (first '...'). For a bare
//     function reference this is the function type, like "int (int)".
//  2. The referenced function's declared type — also the same.
//
// Both equal "int (int)". For matching against an indirect call site's
// callee type we want the pointer form: "int (*)(int)". We synthesize
// it by inserting "(*)" after the return type if a `(` is present and
// it isn't already in pointer form.
func functionPointerType(arcana string) string {
	t := firstQuotedString(arcana)
	return normalizeFnPtrType(t)
}

// normalizeFnPtrType returns the canonical-ish function-pointer form of
// a type string. Examples:
//
//	"int (int)"     -> "int (*)(int)"
//	"int (*)(int)"  -> "int (*)(int)"  (unchanged)
//	"op_t"          -> "op_t"          (typedef, can't expand without
//	                                    chasing — left alone; matching
//	                                    against the call site relies on
//	                                    clangd having canonicalized that
//	                                    end too)
//
// This is a deliberate trade-off: we don't chase typedefs because doing
// so reliably would require extra LSP roundtrips per type. The cost is
// some indirect edges may be missed when one side uses the typedef and
// the other the underlying type.
func normalizeFnPtrType(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	if strings.Contains(t, "(*)") || strings.Contains(t, "(*") {
		return t
	}
	// "ret (args)" -> "ret (*)(args)" — only when there's exactly one
	// "(args)" group, which is the shape of a bare function type.
	idx := strings.Index(t, " (")
	if idx <= 0 {
		return t
	}
	return t[:idx] + " (*)" + t[idx+1:]
}

// synthesizeIndirectEdges turns collected indirect-call sites and
// address-taken refs into edges. The synthesis rule is type-narrowed
// Andersen: for each indirect site (G, T), one edge G --indirect--> F
// for every address-taken function F whose type matches T. Pairs are
// deduped — multiple indirect sites of the same type inside G with the
// same target F still produce only one edge.
//
// This is intentionally a sound over-approximation, not precise: a
// function pointer that's set then unset (or branches by `if`) still
// produces all the candidate edges. Architecture §6.5.
func synthesizeIndirectEdges(sites []indirectSite, taken []addressTakenRef) []indirectEdge {
	// Group taken refs by type for O(sites+taken) join.
	byType := map[string][]string{}
	seenTaken := map[string]bool{}
	for _, ref := range taken {
		if ref.FunctionUSR == "" || ref.Type == "" {
			continue
		}
		dedupe := ref.FunctionUSR + "\x00" + ref.Type
		if seenTaken[dedupe] {
			continue
		}
		seenTaken[dedupe] = true
		byType[ref.Type] = append(byType[ref.Type], ref.FunctionUSR)
	}
	seenEdge := map[string]bool{}
	var out []indirectEdge
	for _, site := range sites {
		if site.CallerUSR == "" || site.Type == "" {
			continue
		}
		for _, calleeUSR := range byType[site.Type] {
			if calleeUSR == site.CallerUSR {
				// Filter self-loops introduced by an obvious case
				// where a dispatcher takes its own address — usually
				// nonsense and just noise.
				continue
			}
			key := site.CallerUSR + "\x00" + calleeUSR
			if seenEdge[key] {
				continue
			}
			seenEdge[key] = true
			out = append(out, indirectEdge{Caller: site.CallerUSR, Callee: calleeUSR})
		}
	}
	return out
}

// indirectEdge is just a (caller, callee) USR pair tagged 'indirect'.
type indirectEdge struct {
	Caller string
	Callee string
}
