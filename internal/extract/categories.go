package extract

// AddressTakeCategoryVocabulary is the canonical, human-readable
// description of the address-take category enum. It is embedded
// verbatim in the relevant MCP tool descriptions so the AI agent's
// runtime prompt carries the precedence rule directly.
//
// This is the single source of truth for the vocabulary. README,
// architecture doc, and the describe_address_take_categories MCP tool
// all reference it (the tool returns it as the prose body of a
// structured response; the markdown docs link to "see the live tool
// description for the canonical form").
//
// Stability: category names are a published contract — renaming them
// would break agent prompts. Adding new categories at the end is safe;
// changing the precedence order is not.
const AddressTakeCategoryVocabulary = `Each row in find_address_takes / get_address_take_sites carries a
` + "`category`" + ` field that has ALREADY been resolved via the precedence
rule below. You do not need to re-apply precedence; treat the value
you receive as the definitive classification.

Each row represents ONE use of a function pointer; the same function
typically has multiple rows in different categories. Aggregate across
rows to understand a function's full set of dispatcher relationships.

Categories, in precedence order (highest first):

  1. compared           — fn pointer in == / != / < / > / <= / >=.
                          NEGATIVE signal: not being invoked here,
                          just tested. e.g. ` + "`if (fn == square)`" + `,
                          ` + "`assert(fn != null_op)`" + `. Always exclude when
                          looking for dispatcher candidates.

  2. arg_to:F#i         — fn ptr is the i-th argument (0-indexed) of
                          a CallExpr to F. Strongest dispatcher
                          signal: callback registration.
                          e.g. ` + "`register_handler(square)`" + ` →
                                arg_to:register_handler#0.

  3. stored_in:T.f      — written into struct field T.f, by
                          assignment or designated initializer.
                          Registry-style code (file_operations etc.).

  4. array_init:N[i?]   — stored into array N at slot i (index omitted
                          when it isn't a literal/named constant).
                          Dispatch tables.

  5. assigned_to:v      — plain scalar assignment / variable cinit.
                          Local flow; weaker signal.

  6. returned_from:F    — returned by F. Factory pattern: callers of
                          F receive this fn pointer.

  7. other              — void* casts, debug stringification, hash
                          keys, etc. Not a dispatcher signal.

Precedence is load-bearing: ` + "`assert(fn == square)`" + ` is ` + "`compared`" + `,
NOT ` + "`arg_to:assert#1`" + `, because the assertion isn't invoking square.
Misreading this is the most common way to over-connect a dispatcher
graph.`

// addressTakeCategoryDescriptors is the structured form of the
// vocabulary, used by the describe_address_take_categories tool.
type addressTakeCategoryDescriptor struct {
	Name        string `json:"name"`
	Rank        int    `json:"rank"`
	Description string `json:"description"`
	Example     string `json:"example"`
	Guidance    string `json:"agent_guidance"`
}

// AddressTakeCategoryDescriptors returns the structured vocabulary
// for the describe_address_take_categories tool. Order is precedence
// order (highest first).
func AddressTakeCategoryDescriptors() []addressTakeCategoryDescriptor {
	return []addressTakeCategoryDescriptor{
		{Name: "compared", Rank: 1,
			Description: "Function pointer is part of an equality/inequality comparison.",
			Example:     "if (fn == square), assert(fn != null_op)",
			Guidance:    "Negative signal — exclude when looking for dispatcher candidates."},
		{Name: "arg_to", Rank: 2,
			Description: "Function pointer is the i-th argument (0-indexed) of a CallExpr.",
			Example:     "register_handler(square)  →  arg_to:register_handler#0",
			Guidance:    "Strongest dispatcher signal: callback registration."},
		{Name: "stored_in", Rank: 3,
			Description: "Written into a struct field by assignment or designated initializer.",
			Example:     "ops.cb = square  →  stored_in:struct_ops.cb",
			Guidance:    "Registry-style C (file_operations, GTK signals, etc.)."},
		{Name: "array_init", Rank: 4,
			Description: "Stored into an array slot. Index omitted when not a literal.",
			Example:     "static op_t ops[] = {square, cube}  →  array_init:ops[0]",
			Guidance:    "Canonical dispatch table pattern."},
		{Name: "assigned_to", Rank: 5,
			Description: "Plain scalar assignment or variable cinit.",
			Example:     "op_t fn = square  →  assigned_to:fn",
			Guidance:    "Weaker signal; usually local flow."},
		{Name: "returned_from", Rank: 6,
			Description: "Returned from the enclosing function.",
			Example:     "return square;  inside pick_op()  →  returned_from:pick_op",
			Guidance:    "Factory pattern."},
		{Name: "other", Rank: 7,
			Description: "Fallback for casts to void*, debug uses, hash keys, etc.",
			Example:     "(void*)square",
			Guidance:    "Usually not a dispatcher signal."},
	}
}

// AddressTakeCategoryPrecedence returns the precedence-ordered list of
// category names.
func AddressTakeCategoryPrecedence() []string {
	return []string{
		"compared", "arg_to", "stored_in",
		"array_init", "assigned_to", "returned_from", "other",
	}
}
