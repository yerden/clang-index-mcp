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

	// sourceLines is the TU source split by '\n'. Used to recover
	// designated-initializer field names — clangd's textDocument/ast
	// does not expose them in either Detail or Arcana, but the source
	// at the DesignatedInit node's range start begins with `.<field>`.
	sourceLines []string

	// typedefs maps a typedef name (as appearing in expression types)
	// to its canonical body. Populated during the walk from
	// TypedefDecl nodes (whose arcana is `TypedefDecl ... '<body>'`).
	// Used by canonicalizeType to bring address-take and indirect-
	// call-site types into a single canonical form, which is what
	// makes the documented "join by type" workflow actually work.
	typedefs map[string]string

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
	return &walker{
		ctx: ctx, cli: cli, uri: uri,
		usrCache: map[Position]string{},
		typedefs: map[string]string{},
	}
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

	// Collect typedef bodies as we encounter them. clangd dumps a
	// TypedefDecl as `TypedefDecl 0x... <loc> [referenced] <name>
	// '<canonical body>'` — Detail is the name, the last quoted
	// substring in Arcana is the canonical body. Done early so that
	// any expressions DESCENDING from here can use the resolved form.
	if n.Kind == "Typedef" && n.Role == "declaration" && n.Detail != "" {
		if body := lastQuotedString(n.Arcana); body != "" && body != n.Detail {
			if _, exists := w.typedefs[n.Detail]; !exists {
				w.typedefs[n.Detail] = body
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
						FnPtrType:     w.canonicalizeType(fnPtrTypeFromContext(n.Arcana)),
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
	calleeType := w.canonicalizeType(firstQuotedType(callee.Arcana))
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
		case "DesignatedInit":
			// `.field = expr` inside an InitListExpr. clangd's AST does
			// NOT expose the field name in detail or arcana — we read
			// it from the source at the node's range start, which by
			// clang's source-location convention begins at the `.`.
			// The enclosing InitListExpr (next frame up) carries the
			// aggregate type; we look further up to the VarDecl for it
			// since clangd writes "struct foo" with the right canonical
			// spelling there.
			field := w.designatedFieldName(anc.rangeStart)
			structType := w.enclosingAggregateType(i)
			if field != "" {
				if structType != "" {
					return store.CategoryStoredIn, structType + "." + field, true
				}
				return store.CategoryStoredIn, "." + field, true
			}
			// Couldn't recover the field — fall back to the old
			// InitList classifier (which uses the <init> placeholder).
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

// designatedFieldName reads the TU source at pos and returns the
// designator field name. clangd places the range start at the `.`,
// so we slice from pos+1 and read identifier bytes.
//
// Returns "" if the source isn't available or the slice doesn't look
// like `.<ident>`. We never fabricate a name on bad input.
func (w *walker) designatedFieldName(pos Position) string {
	if pos.Line < 0 || pos.Line >= len(w.sourceLines) {
		return ""
	}
	line := w.sourceLines[pos.Line]
	if pos.Character < 0 || pos.Character >= len(line) {
		return ""
	}
	slice := line[pos.Character:]
	if len(slice) < 2 || slice[0] != '.' {
		return ""
	}
	end := 1
	for end < len(slice) && isIdentByte(slice[end]) {
		end++
	}
	if end == 1 {
		return ""
	}
	return slice[1:end]
}

// enclosingAggregateType returns the canonical name of the aggregate
// being initialized by the InitList that encloses the DesignatedInit
// at stack[initIdx]. Walks up to the nearest VarDecl and reads its
// type from arcana.
func (w *walker) enclosingAggregateType(initIdx int) string {
	for j := initIdx - 1; j >= 0; j-- {
		if w.stack[j].kind == "Var" {
			t := firstQuotedType(w.stack[j].arcana)
			// Strip CV-qualifiers; agents will match against the
			// unqualified name. "const struct foo" → "struct foo".
			t = strings.TrimPrefix(t, "const ")
			t = strings.TrimPrefix(t, "volatile ")
			return t
		}
	}
	return ""
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
// present so that type-narrowing matches across typedef sites. clangd
// only emits the typedef:canonical pair when the *outermost* type is
// a typedef; when the typedef is nested (e.g. `lcore_function_t *`,
// where the outermost type is the pointer), only the typedef-spelled
// form appears, and walker.canonicalizeType has to do the substitution.
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

// collectTypedefsFromAST walks an AST tree and harvests TypedefDecl
// entries into out. Used at extract.Run scope to build a shared
// typedef table that survives across TU walks — without this, headers'
// typedef bodies (invisible to the per-TU AST) never reach the
// canonicalizer. Safe to call multiple times against the same `out`.
func collectTypedefsFromAST(n astNode, out map[string]string) {
	if n.Kind == "Typedef" && n.Role == "declaration" && n.Detail != "" {
		if body := lastQuotedString(n.Arcana); body != "" && body != n.Detail {
			if _, ok := out[n.Detail]; !ok {
				out[n.Detail] = body
			}
		}
	}
	for _, ch := range n.Children {
		collectTypedefsFromAST(ch, out)
	}
}

// lastQuotedString returns the final '...' substring of a clangd
// arcana line. For TypedefDecls that's the canonical type body, e.g.
// in `TypedefDecl 0x... <loc> referenced lcore_function_t 'int (void *)'`
// it returns `int (void *)`.
func lastQuotedString(arcana string) string {
	all := firstQuoteRE.FindAllStringSubmatch(arcana, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1][1]
}

// canonicalizeType is the function that brings types from the two fact
// tables into a single shape so the join-by-type workflow works. It:
//
//  1. Substitutes any known typedef name (whole-word match) with its
//     canonical body, collected at extract time from TypedefDecls.
//  2. Applies normalizeFnPtrType to reshape `ret (args)` function
//     types into `ret (*)(args)` function-pointer types — once for the
//     direct case and again for the "typedef expanded next to a `*`"
//     case where the substitution leaves a bare function type with a
//     trailing pointer.
//
// Idempotent and safe to call on an already-canonical type: if no
// typedef names appear, the input passes through (only normalizeFnPtrType
// touches it, which is also idempotent for already-canonical forms).
func (w *walker) canonicalizeType(t string) string {
	if t == "" {
		return ""
	}
	if len(w.typedefs) == 0 {
		return normalizeFnPtrType(t)
	}
	// Substitute typedef names. Whole-word only — `lcore_function_t`
	// must not match inside `my_lcore_function_t_extra` etc.
	for name, body := range w.typedefs {
		t = substituteTypedefWord(t, name, body)
	}
	// After substitution we may have a function type followed by a
	// pointer: `int (void *) *` — reshape to `int (*)(void *)`. Do
	// this BEFORE the regular fn-ptr normalization so the latter
	// doesn't double-insert (*).
	t = reshapeFunctionTypePointer(t)
	return normalizeFnPtrType(t)
}

// substituteTypedefWord replaces whole-word occurrences of name with
// body inside t. "Whole word" = neither neighbor is an identifier
// character. We don't iterate to a fixed point because typedef bodies
// themselves can reference other typedefs and the walker's collection
// happens during traversal — a single pass on a single type string is
// enough for the cases we care about; deeper aliasing would need a
// proper resolver.
func substituteTypedefWord(t, name, body string) string {
	if !strings.Contains(t, name) {
		return t
	}
	var out strings.Builder
	out.Grow(len(t))
	i := 0
	for i < len(t) {
		j := strings.Index(t[i:], name)
		if j < 0 {
			out.WriteString(t[i:])
			break
		}
		j += i
		// Boundary checks.
		leftOK := j == 0 || !isIdentByte(t[j-1])
		end := j + len(name)
		rightOK := end == len(t) || !isIdentByte(t[end])
		out.WriteString(t[i:j])
		if leftOK && rightOK {
			out.WriteString(body)
		} else {
			out.WriteString(t[j:end])
		}
		i = end
	}
	return out.String()
}

func isIdentByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

// reshapeFunctionTypePointer turns `ret (args) *` (substituted from a
// `<func_typedef> *` shape) into `ret (*)(args)`. Multiple trailing
// pointers compound the inner-stars: `ret (args) **` → `ret (**)(args)`.
//
// Detection rule: find a function-shape `<ret> (<args>)` followed by
// optional whitespace and one or more `*`. clangd's canonical syntax
// always has a single space between return type and the `(args)`.
var fnTypeWithTailPtrRE = regexp.MustCompile(`([^()]+\([^()]*\))\s*(\*+)`)

func reshapeFunctionTypePointer(t string) string {
	return fnTypeWithTailPtrRE.ReplaceAllStringFunc(t, func(m string) string {
		sub := fnTypeWithTailPtrRE.FindStringSubmatch(m)
		if sub == nil {
			return m
		}
		head := sub[1]
		stars := sub[2]
		// `head` is `<ret> (<args>)`. Insert `(<stars>)` between
		// return type and `(args)`.
		idx := strings.LastIndex(head, " (")
		if idx <= 0 {
			return m
		}
		return head[:idx] + " (" + stars + ")" + head[idx+1:]
	})
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
