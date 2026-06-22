package extract

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/yerden/clang-index-mcp/internal/lsp"
	"github.com/yerden/clang-index-mcp/internal/store"
)

// astNode is the decoded shape of clangd's textDocument/ast response.
// clangd 15+ returns this for any open TU. Each node carries both a
// short Kind (e.g. "Call", "DeclRef") and the full clang Decl::dump()
// line in Arcana, which is what we mine for static expression types
// and the decl kind referenced by a DeclRefExpr.
type astNode struct {
	Kind     string    `json:"kind"`
	Detail   string    `json:"detail"`
	Arcana   string    `json:"arcana"`
	Role     string    `json:"role"`
	Range    Range     `json:"range"`
	Children []astNode `json:"children"`
}

// fetchAST asks clangd for the full TU AST. Returns nil (no error) when
// the extension isn't supported (older clangd) or the response is null.
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

// walker collects two kinds of facts while traversing one TU's AST:
//   - address_takes: one row per DeclRefExpr→Function in a non-callee
//     position, classified by precedence (see classifyAddressTake).
//   - indirect_call_sites: one row per CallExpr whose callee, after
//     peeling cast/paren wrappers, is not a DeclRefExpr→Function.
type walker struct {
	ctx context.Context
	cli *lsp.Client
	uri string

	// nameToNamePos maps a function's name (as seen in AST node Detail)
	// to its identifier SelectionRange.Start, harvested from
	// documentSymbol. AST FunctionDecl nodes' own Range covers the
	// whole declaration; clangd's symbolInfo only answers at the
	// identifier position.
	nameToNamePos map[string]Position

	// stack of frames; top is the current node. Each frame records its
	// position (childIndex) in its parent's children, which we walk to
	// classify the context of an address-take.
	stack []frame

	// enclosing function context for the current node, indexed by stack
	// depth at which it was pushed. We use a separate slice to make
	// pop/push deterministic — Function nodes appear at varying depths.
	enclosingFnUSR string

	// USR cache so we don't re-query symbolInfo at the same position.
	usrCache map[Position]string

	addressTakes      []addressTakeFact
	indirectCallSites []indirectCallSiteFact
}

type frame struct {
	kind        string
	detail      string
	arcana      string
	rangeStart  Position
	rangeEnd    Position
	childIndex  int // our position in our parent's children; -1 at root
	numChildren int

	// Sibling hint for BinaryOperator etc. Populated when child[0] is
	// visited so that classifyAssignmentLHS can read it later when
	// child[1] (the RHS) is visited.
	sibling0Kind siblingHint

	// For CallExpr: the resolved name of the function being called
	// (child[0] after peeling wrappers). Populated when child[0] is
	// visited so that arg_to detail can reference it when an arg is
	// processed later.
	calleeName string
}

// addressTakeFact mirrors store.AddressTake but with absolute paths
// (Run relativizes them when writing to the DB).
type addressTakeFact struct {
	FunctionUSR   string `json:"function_usr"`
	TakenAtFile   string `json:"taken_at_file"`
	TakenAtLine   int    `json:"taken_at_line"`
	FnPtrType     string `json:"fn_ptr_type"`
	Category      string `json:"category"`
	ContextDetail string `json:"context_detail"`
}

// indirectCallSiteFact mirrors store.IndirectCallSite, abs paths.
type indirectCallSiteFact struct {
	CallerUSR  string `json:"caller_usr"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	CalleeType string `json:"callee_type"`
	CalleeExpr string `json:"callee_expr"`
}

func newWalker(ctx context.Context, cli *lsp.Client, uri string) *walker {
	return &walker{ctx: ctx, cli: cli, uri: uri, usrCache: map[Position]string{}}
}

// walk is the top-level entry point. Call once with the TU root.
func (w *walker) walk(root astNode) {
	w.visit(root, -1)
}

// visit traverses one node. childIdx is this node's position in its
// parent's children (or -1 at the root).
func (w *walker) visit(n astNode, childIdx int) {
	// Push frame.
	w.stack = append(w.stack, frame{
		kind: n.Kind, detail: n.Detail, arcana: n.Arcana,
		rangeStart: n.Range.Start, rangeEnd: n.Range.End,
		childIndex: childIdx, numChildren: len(n.Children),
	})

	// Track enclosing function. FunctionDecl in declaration role pushes
	// a new enclosing function; we pop on exit.
	var poppedFnUSR string
	var didPush bool
	if n.Kind == "Function" && n.Role == "declaration" {
		if pos, ok := w.nameToNamePos[n.Detail]; ok {
			if usr := w.resolveUSR(pos); usr != "" {
				poppedFnUSR = w.enclosingFnUSR
				w.enclosingFnUSR = usr
				didPush = true
			}
		}
	}

	// Classify the current node.
	switch n.Kind {
	case "Call":
		w.handleCall(n)
	case "DeclRef":
		if referencesFunction(n.Arcana) {
			if cat, detail, ok := w.classifyAddressTake(n); ok {
				usr := w.resolveUSR(n.Range.Start)
				if usr != "" {
					w.addressTakes = append(w.addressTakes, addressTakeFact{
						FunctionUSR:   usr,
						TakenAtFile:   uriToPath(w.uri),
						TakenAtLine:   n.Range.Start.Line + 1,
						FnPtrType:     fnPtrTypeFromContext(n.Arcana),
						Category:      cat,
						ContextDetail: detail,
					})
				}
			}
		}
	}

	// Recurse. After visiting child[0] of nodes that need sibling
	// hints (BinaryOperator, Call), populate the parent frame with
	// what we learned about that child so later children can read it.
	for i, ch := range n.Children {
		w.visit(ch, i)
		if i == 0 {
			top := len(w.stack) - 1
			switch n.Kind {
			case "BinaryOperator":
				w.stack[top].sibling0Kind = inspectLHS(ch)
			case "Call":
				w.stack[top].calleeName = inspectCallee(ch)
			}
		}
	}

	// Pop.
	if didPush {
		w.enclosingFnUSR = poppedFnUSR
	}
	w.stack = w.stack[:len(w.stack)-1]
}

// handleCall records an indirect_call_site when the CallExpr's callee
// resolves to anything other than a direct DeclRefExpr→Function.
func (w *walker) handleCall(n astNode) {
	if len(n.Children) == 0 || w.enclosingFnUSR == "" {
		return
	}
	callee := n.Children[0]
	stripped := drillThroughCastWrappers(callee)
	if stripped.Kind == "DeclRef" && referencesFunction(stripped.Arcana) {
		return // direct call — covered by callHierarchy elsewhere
	}
	calleeType := normalizeFnPtrType(firstQuotedType(callee.Arcana))
	if calleeType == "" {
		return
	}
	w.indirectCallSites = append(w.indirectCallSites, indirectCallSiteFact{
		CallerUSR:  w.enclosingFnUSR,
		File:       uriToPath(w.uri),
		Line:       n.Range.Start.Line + 1,
		CalleeType: calleeType,
		CalleeExpr: calleeExprText(stripped),
	})
}

// classifyAddressTake walks up the stack from the current DeclRef node
// to find the first matching category. The caller has already verified
// the node references a Function; this routine decides what to record.
//
// Precedence (highest first): compared > arg_to > stored_in >
// array_init > assigned_to > returned_from > other. Implemented as a
// stack-walk where the first decisive ancestor wins — see the design
// rationale in architecture §6.5 and CLAUDE.md.
//
// Returns (category, detail, ok); ok=false means "skip this site" (we
// only return false when the node is in callee position of a CallExpr
// — that's a direct call, not an address-take).
func (w *walker) classifyAddressTake(n astNode) (cat, detail string, ok bool) {
	// Stack has the current node on top.
	if len(w.stack) < 2 {
		return store.CategoryOther, "", true
	}

	// First check: are we in the callee subtree of an enclosing CallExpr?
	// If yes, this DeclRef is a direct call, not an address-take.
	if w.inCalleeSubtree() {
		return "", "", false
	}

	// Walk up the stack from the parent toward the root, returning the
	// first match. Pass parentIdx = our index in our parent's children
	// so that pattern-match callbacks can read sibling info.
	for i := len(w.stack) - 2; i >= 0; i-- {
		anc := &w.stack[i]
		descendantIdx := w.stack[i+1].childIndex

		switch anc.kind {
		case "ImplicitCast", "CStyleCast", "Paren":
			// Skip — these are syntactic noise around the address-take.
			continue
		case "BinaryOperator":
			op := binaryOpKind(anc.arcana)
			switch op {
			case "==", "!=", "<", ">", "<=", ">=":
				return store.CategoryCompared, "", true
			case "=":
				// Assignment: classify by the LHS sibling.
				return w.classifyAssignmentLHS(i, descendantIdx)
			}
			// Other binary operators (arithmetic, logical) → other.
			return store.CategoryOther, "", true
		case "Return":
			fn := w.currentEnclosingFunctionName()
			return store.CategoryReturnedFrom, fn, true
		case "Call":
			// We're in some arg slot (descendantIdx >= 1, since
			// inCalleeSubtree was false). The CallExpr frame has
			// already been populated with calleeName when child[0]
			// was visited.
			if descendantIdx >= 1 {
				calleeName := anc.calleeName
				if calleeName == "" {
					calleeName = "<call>"
				}
				return store.CategoryArgTo, calleeName + "#" + itoa(descendantIdx-1), true
			}
			return store.CategoryOther, "", true
		case "InitList":
			return w.classifyInitListContext(i)
		case "Var":
			// VarDecl cinit with a scalar function-pointer type.
			return store.CategoryAssignedTo, anc.detail, true
		}
	}
	return store.CategoryOther, "", true
}

// inCalleeSubtree reports whether the nearest CallExpr ancestor was
// entered via its child[0] — i.e. we're inside the callee expression
// rather than an argument.
func (w *walker) inCalleeSubtree() bool {
	for i := len(w.stack) - 2; i >= 0; i-- {
		if w.stack[i].kind == "Call" {
			return w.stack[i+1].childIndex == 0
		}
	}
	return false
}

// classifyAssignmentLHS reads sibling info captured when child[0] of
// the BinaryOperator was visited.
func (w *walker) classifyAssignmentLHS(binOpIdx, descendantIdx int) (string, string, bool) {
	if descendantIdx == 0 {
		return store.CategoryOther, "", true
	}
	lhs := w.stack[binOpIdx].sibling0Kind
	switch lhs.kind {
	case "Member":
		return store.CategoryStoredIn, lhs.structType + "." + lhs.field, true
	case "ArraySubscript":
		if lhs.arrayIndex != "" {
			return store.CategoryArrayInit, lhs.arrayName + "[" + lhs.arrayIndex + "]", true
		}
		return store.CategoryArrayInit, lhs.arrayName, true
	case "DeclRef":
		return store.CategoryAssignedTo, lhs.varName, true
	}
	return store.CategoryAssignedTo, "", true
}

// classifyInitListContext walks the stack above an InitList to find the
// enclosing VarDecl, classifying based on its type (array vs. struct).
func (w *walker) classifyInitListContext(initListIdx int) (string, string, bool) {
	// Find enclosing VarDecl on the stack.
	for j := initListIdx - 1; j >= 0; j-- {
		anc := w.stack[j]
		if anc.kind != "Var" {
			continue
		}
		t := firstQuotedType(anc.arcana)
		varName := anc.detail
		// Detect array type via trailing "[N]" pattern.
		if strings.Contains(t, "[") && strings.Contains(t, "]") {
			// position within InitList = our index within it
			idx := w.stack[initListIdx+1].childIndex
			return store.CategoryArrayInit, varName + "[" + itoa(idx) + "]", true
		}
		// Otherwise a struct-or-aggregate init.
		return store.CategoryStoredIn, t + ".<init>", true
	}
	return store.CategoryAssignedTo, "", true
}

// currentEnclosingFunctionName returns the name (Detail) of the
// nearest enclosing FunctionDecl frame, or "" if none.
func (w *walker) currentEnclosingFunctionName() string {
	for i := len(w.stack) - 1; i >= 0; i-- {
		if w.stack[i].kind == "Function" {
			return w.stack[i].detail
		}
	}
	return ""
}

// resolveUSR consults clangd's symbolInfo at a position, caching by
// Position.
func (w *walker) resolveUSR(p Position) string {
	if cached, ok := w.usrCache[p]; ok {
		return cached
	}
	usr, _ := symbolUSR(w.ctx, w.cli, w.uri, p)
	w.usrCache[p] = usr
	return usr
}

// drillThroughCastWrappers peels off the AST nodes that clangd inserts
// between a CallExpr and its underlying DeclRefExpr: ImplicitCast for
// FunctionToPointerDecay, Paren for parenthesized callees, CStyleCast
// for explicit casts.
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

// referencesFunction reports whether an arcana line denotes a DeclRef
// (or similar) that points at a function-like decl. clangd writes the
// referenced decl kind on the same line, e.g. ` Function 0x...`.
var fnRefRE = regexp.MustCompile(` (Function|CXXMethod|CXXConstructor|CXXDestructor) 0x[0-9a-f]+`)

func referencesFunction(arcana string) bool {
	return fnRefRE.MatchString(arcana)
}

// firstQuotedType extracts an expression's static type from its
// arcana. clang dumps types in one of two shapes:
//
//	'int (int)'                    (no typedef)
//	'op_t':'int (*)(int)'          (typedef + canonical)
//
// We prefer the canonical form (the second '...') whenever it's
// present so that type-narrowing matches across typedef sites.
var typedefCanonicalRE = regexp.MustCompile(`'([^']*)':'([^']*)'`)
var firstQuoteRE = regexp.MustCompile(`'([^']*)'`)

func firstQuotedType(arcana string) string {
	if m := typedefCanonicalRE.FindStringSubmatch(arcana); m != nil {
		return m[2]
	}
	if m := firstQuoteRE.FindStringSubmatch(arcana); m != nil {
		return m[1]
	}
	return ""
}

// fnPtrTypeFromContext returns the function-pointer type associated
// with an address-taken DeclRef. The DeclRef itself usually carries
// the *function type* (e.g. 'int (int)'); after FunctionToPointerDecay
// the surrounding ImplicitCast carries 'int (*)(int)'. We can derive
// the pointer form from the function type alone, which is what we do
// here: insert "(*)" after the return type.
func fnPtrTypeFromContext(arcana string) string {
	return normalizeFnPtrType(firstQuotedType(arcana))
}

// normalizeFnPtrType returns the canonical-ish function-pointer form.
// "int (int)" -> "int (*)(int)"; "int (*)(int)" stays as-is.
func normalizeFnPtrType(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	if strings.Contains(t, "(*") {
		return t
	}
	idx := strings.Index(t, " (")
	if idx <= 0 {
		return t
	}
	return t[:idx] + " (*)" + t[idx+1:]
}

// binaryOpKind reads the operator out of a BinaryOperator arcana.
// clangd writes: "BinaryOperator addr <loc> 'type' '<op>'".
var binOpRE = regexp.MustCompile(`'(==|!=|<=|>=|<|>|=|\+|-|\*|/|%|&&|\|\||&|\||\^|<<|>>)'`)

func binaryOpKind(arcana string) string {
	matches := binOpRE.FindAllStringSubmatch(arcana, -1)
	if len(matches) == 0 {
		return ""
	}
	// The OP appears after the type, so prefer the last match — the
	// first quoted segment is the result type ('int', '_Bool', etc.).
	return matches[len(matches)-1][1]
}

// calleeExprText produces a short, human-readable label for an
// indirect callee expression, used as indirect_call_sites.callee_expr.
func calleeExprText(n astNode) string {
	switch n.Kind {
	case "DeclRef":
		if n.Detail != "" {
			return n.Detail
		}
	case "Member":
		base := ""
		if len(n.Children) > 0 {
			base = calleeExprText(n.Children[0])
		}
		if base == "" {
			return n.Detail
		}
		return base + "." + n.Detail
	case "ArraySubscript":
		arr := ""
		idx := ""
		if len(n.Children) >= 1 {
			arr = calleeExprText(n.Children[0])
		}
		if len(n.Children) >= 2 {
			idx = calleeExprText(n.Children[1])
		}
		if arr != "" {
			if idx == "" {
				return arr + "[?]"
			}
			return arr + "[" + idx + "]"
		}
	case "Call":
		if len(n.Children) > 0 {
			return calleeExprText(n.Children[0]) + "()"
		}
	case "IntegerLiteral":
		return n.Detail
	}
	return "<expr>"
}

// itoa is strconv.Itoa without the import — used so this file's import
// list stays focused on what's analytically interesting.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// Sibling-hint plumbing -------------------------------------------------

// siblingHint is what we remember about the LHS of a binary expression
// while walking. Populated when descending into child[0] of a
// BinaryOp(=); read by classifyAssignmentLHS when visiting child[1].
type siblingHint struct {
	kind       string // "Member" | "ArraySubscript" | "DeclRef" | ""
	structType string
	field      string
	arrayName  string
	arrayIndex string
	varName    string
}

// inspectLHS classifies a BinaryOperator's left-hand side for later
// use by the address-take classifier on the RHS subtree.
func inspectLHS(n astNode) siblingHint {
	// Peel off lvalue-to-rvalue / paren wrappers that don't change
	// what the LHS refers to.
	for n.Kind == "Paren" {
		if len(n.Children) == 0 {
			break
		}
		n = n.Children[0]
	}
	switch n.Kind {
	case "Member":
		base := ""
		if len(n.Children) > 0 {
			base = firstQuotedType(n.Children[0].Arcana)
		}
		return siblingHint{kind: "Member", structType: base, field: n.Detail}
	case "ArraySubscript":
		arr := ""
		idx := ""
		if len(n.Children) >= 1 {
			arr = inspectArrayName(n.Children[0])
		}
		if len(n.Children) >= 2 {
			idx = inspectArrayIndex(n.Children[1])
		}
		return siblingHint{kind: "ArraySubscript", arrayName: arr, arrayIndex: idx}
	case "DeclRef":
		return siblingHint{kind: "DeclRef", varName: n.Detail}
	}
	return siblingHint{}
}

func inspectArrayName(n astNode) string {
	for n.Kind == "ImplicitCast" || n.Kind == "Paren" {
		if len(n.Children) == 0 {
			break
		}
		n = n.Children[0]
	}
	if n.Kind == "DeclRef" {
		return n.Detail
	}
	return ""
}

func inspectArrayIndex(n astNode) string {
	for n.Kind == "ImplicitCast" || n.Kind == "Paren" {
		if len(n.Children) == 0 {
			break
		}
		n = n.Children[0]
	}
	switch n.Kind {
	case "IntegerLiteral":
		return n.Detail
	case "DeclRef":
		return n.Detail
	}
	return ""
}

// inspectCallee returns the textual name of the function being called
// when a CallExpr's child[0] resolves to a direct DeclRef→Function.
// For indirect callees (parameter, member access, etc.), returns "".
func inspectCallee(n astNode) string {
	n = drillThroughCastWrappers(n)
	if n.Kind == "DeclRef" && referencesFunction(n.Arcana) {
		return n.Detail
	}
	return ""
}
