-- name: insert_symbol
INSERT INTO symbols (usr, name, kind, file, line, decl_file, decl_line, signature)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(usr) DO UPDATE SET
  name=excluded.name,
  kind=excluded.kind,
  file=excluded.file,
  line=excluded.line,
  decl_file=excluded.decl_file,
  decl_line=excluded.decl_line,
  signature=excluded.signature
RETURNING id;

-- name: insert_symbol_fts
INSERT INTO symbols_fts (rowid, name, signature) VALUES (?, ?, ?);

-- name: insert_call_edge
INSERT INTO call_edges (caller_id, callee_id) VALUES (?, ?);

-- name: lookup_symbol_id_by_usr
SELECT id FROM symbols WHERE usr = ?;

-- name: search_symbol_fts
SELECT s.id, s.usr, s.name, s.kind, s.file, s.line, s.decl_file, s.decl_line, s.signature
FROM symbols_fts f
JOIN symbols s ON s.id = f.rowid
WHERE symbols_fts MATCH ?
ORDER BY rank
LIMIT ?;

-- name: get_symbol
SELECT id, usr, name, kind, file, line, decl_file, decl_line, signature
FROM symbols
WHERE id = ?;

-- name: get_callers
SELECT s.id, s.usr, s.name, s.kind, s.file, s.line, s.decl_file, s.decl_line, s.signature
FROM call_edges e
JOIN symbols s ON s.id = e.caller_id
WHERE e.callee_id = ?;

-- name: get_callees
SELECT s.id, s.usr, s.name, s.kind, s.file, s.line, s.decl_file, s.decl_line, s.signature
FROM call_edges e
JOIN symbols s ON s.id = e.callee_id
WHERE e.caller_id = ?;

-- name: list_symbols_in_file
SELECT id, usr, name, kind, file, line, decl_file, decl_line, signature
FROM symbols
WHERE decl_file = ?1 OR file = ?1
ORDER BY decl_file, decl_line, name
LIMIT ?2;

-- name: insert_address_take
INSERT INTO address_takes
  (function_id, taken_at_file, taken_at_line, fn_ptr_type, category, context_detail)
VALUES (?, ?, ?, ?, ?, ?);

-- name: insert_indirect_call_site
INSERT INTO indirect_call_sites
  (caller_id, file, line, callee_type, callee_expr)
VALUES (?, ?, ?, ?, ?);

-- name: get_address_take_sites
SELECT a.taken_at_file, a.taken_at_line, a.fn_ptr_type, a.category, a.context_detail,
       s.id, s.usr, s.name, s.kind, s.file, s.line, s.decl_file, s.decl_line, s.signature
FROM address_takes a
JOIN symbols s ON s.id = a.function_id
WHERE a.function_id = ?
ORDER BY a.taken_at_file, a.taken_at_line
LIMIT ?;

-- name: find_address_takes
SELECT a.taken_at_file, a.taken_at_line, a.fn_ptr_type, a.category, a.context_detail,
       s.id, s.usr, s.name, s.kind, s.file, s.line, s.decl_file, s.decl_line, s.signature
FROM address_takes a
JOIN symbols s ON s.id = a.function_id
WHERE (?1 = '' OR a.fn_ptr_type = ?1)
  AND (?2 = '' OR a.category    = ?2)
  AND (?3 = '' OR a.context_detail LIKE ?3)
ORDER BY a.category, s.name, a.taken_at_file, a.taken_at_line
LIMIT ?4;

-- name: get_indirect_call_sites_by_caller
SELECT i.file, i.line, i.callee_type, i.callee_expr,
       s.id, s.usr, s.name, s.kind, s.file, s.line, s.decl_file, s.decl_line, s.signature
FROM indirect_call_sites i
JOIN symbols s ON s.id = i.caller_id
WHERE i.caller_id = ?1
  AND (?2 = '' OR i.callee_expr LIKE ?2)
ORDER BY i.file, i.line
LIMIT ?3;

-- name: list_indirect_call_sites
SELECT i.file, i.line, i.callee_type, i.callee_expr,
       s.id, s.usr, s.name, s.kind, s.file, s.line, s.decl_file, s.decl_line, s.signature
FROM indirect_call_sites i
JOIN symbols s ON s.id = i.caller_id
WHERE (?1 = '' OR i.callee_type = ?1)
  AND (?2 = '' OR i.callee_expr LIKE ?2)
ORDER BY s.name, i.file, i.line
LIMIT ?3;
