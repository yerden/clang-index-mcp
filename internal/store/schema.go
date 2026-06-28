package store

// SchemaSQL returns the raw CREATE TABLE/INDEX text — authoritative for
// column names, types, and indexes. Exposed for the describe_schema MCP
// tool.
func SchemaSQL() string { return schemaSQL }

// SchemaGuide is the agent-facing semantic layer over SchemaSQL: what
// each column means, what '' / 0 sentinel values mean, what enum values
// are valid, and canonical join recipes. Kept here (next to the embed,
// not in internal/mcp) so a schema change and its prose update land in
// one file — CLAUDE.md invariant #12 applies to this string too.
//
// Renames or semantic shifts here are contract-breaking. See
// TestDescribeSchemaCarriesGuidance for the fencing test.
const SchemaGuide = `# Index schema — semantic guide

All file paths are RELATIVE to ProjectRoot (architecture §5.2).
Line numbers are 1-based.

## symbols
One row per definition. ` + "`id`" + ` is stable within one build but NOT across
rebuilds (each ` + "`clang-index build`" + ` writes a fresh DB). USRs are stable.

  - usr        — clangd's symbol USR; unique. Use this if you need to compare
                 identity across rebuilds.
  - kind       — clangd SymbolKind name (Function, Variable, Struct, ...).
  - file,line  — definition site (the .c / .cpp / .m holding the body).
  - decl_file  — declaration site (typically the header). EMPTY STRING ('')
                 when the declaration coincides with the definition (static
                 function defined and used in one TU). Not NULL — ''.
  - decl_line  — 0 (not NULL) when decl_file is ''.
  - signature  — pretty-printed signature for functions; '' for non-functions.

  Index on decl_file (for "what's declared in this header" queries).

## symbols_fts
FTS5 virtual table over (name, signature), contentless-shadowed on symbols.
Query with MATCH, not LIKE. Tokenizer is unicode61 with '_' as a SEPARATOR,
so 'foo_bar' tokenizes to ['foo','bar'] — MATCH 'foo_bar' and MATCH 'foo bar'
behave the same. For prefix search by full identifier use LIKE on
symbols.name (e.g. WHERE name LIKE 'foo_bar%').

  Example: SELECT s.id, s.name FROM symbols_fts
             JOIN symbols s ON s.id = symbols_fts.rowid
            WHERE symbols_fts MATCH ? ORDER BY rank LIMIT 50;

## call_edges
Direct, statically-resolved call edges only. Indirect calls (through
function pointers) are NOT here — see address_takes + indirect_call_sites.
Rows with unresolved endpoints are dropped at write time.

  - caller_id, callee_id — FK to symbols(id).

  Indexed both directions: idx_caller, idx_callee.

## address_takes
One row per source location where a function's address is taken. Raw fact —
synthesizing "which dispatcher gets this callback" is left to the agent.

  - function_id    — FK to symbols(id); the function WHOSE address is taken.
  - taken_at_file,
    taken_at_line  — site of the address-take expression (NOT the function's
                     own definition site).
  - fn_ptr_type    — canonical post-typedef-decay form, e.g. 'int (*)(int)'.
                     Typedef-spelled forms ('op_t') are substituted away.
  - category       — precedence-classified; values:
                       compared | arg_to | stored_in | array_init | assigned_to | returned_from | other
                     Use describe_address_take_categories for the full
                     descriptor (precedence rules, examples, agent guidance).
  - context_detail — qualifier whose syntax depends on category. Does NOT carry the category prefix:
                       arg_to        -> '<callee_name>#<arg_index>'
                       stored_in     -> '<struct_type>.<field>'
                       array_init    -> '<array_name>' | '<array_name>[<i>]'
                                        (i is the InitList child position,
                                        not necessarily the actual array slot)
                       assigned_to   -> '<var_name>'
                       returned_from -> '<enclosing_function_name>'
                       compared, other -> ''

  Indexes: idx_at_function (by callback), idx_at_category_type.

## indirect_call_sites
One row per CallExpr whose callee is not a directly-named function.

  - caller_id   — FK to symbols(id); the function CONTAINING the call.
                  (Different semantics from address_takes.function_id —
                  here it's the dispatcher, there it's the callback.)
  - file,line   — site of the call.
  - callee_type — canonical fn-ptr type at the site.
  - callee_expr — surface expression, e.g. 'fn', 'ops[i]', '<base>.cb'.

  Indexes: idx_ics_caller, idx_ics_type.

## Join recipes

Direct callers of symbol X (by name):
  SELECT s.id, s.name, s.file, s.line
    FROM symbols x
    JOIN call_edges e ON e.callee_id = x.id
    JOIN symbols    s ON s.id        = e.caller_id
   WHERE x.name = ?;

Callbacks registered by passing them as arg #i of dispatcher D:
  SELECT s.id, s.name, a.fn_ptr_type, a.taken_at_file, a.taken_at_line
    FROM address_takes a
    JOIN symbols       s ON s.id = a.function_id
   WHERE a.category = 'arg_to'
     AND a.context_detail LIKE ?;  -- e.g. 'D#%' for any arg slot of D

Dispatcher → candidate callbacks via matching fn-ptr type at a site
(sound-but-noisy; narrow by naming convention and context_detail):
  SELECT ics.file, ics.line, ics.callee_expr, s.name AS candidate
    FROM indirect_call_sites ics
    JOIN address_takes        a   ON a.fn_ptr_type = ics.callee_type
    JOIN symbols              s   ON s.id          = a.function_id
   WHERE ics.caller_id = ?
     AND a.category = 'arg_to';

Transitive callers of X up to depth N:
  WITH RECURSIVE ancestors(id, depth) AS (
    SELECT callee_id, 0 FROM call_edges WHERE callee_id = ?
    UNION
    SELECT e.caller_id, a.depth + 1
      FROM call_edges e JOIN ancestors a ON e.callee_id = a.id
     WHERE a.depth < ?
  )
  SELECT DISTINCT s.id, s.name
    FROM ancestors a JOIN symbols s ON s.id = a.id;
`
