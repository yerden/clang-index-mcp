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
INSERT INTO call_edges (caller_id, callee_id, edge_kind) VALUES (?, ?, ?);

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
SELECT s.id, s.usr, s.name, s.kind, s.file, s.line, s.decl_file, s.decl_line, s.signature, e.edge_kind
FROM call_edges e
JOIN symbols s ON s.id = e.caller_id
WHERE e.callee_id = ?
ORDER BY e.edge_kind, s.name;

-- name: get_callees
SELECT s.id, s.usr, s.name, s.kind, s.file, s.line, s.decl_file, s.decl_line, s.signature, e.edge_kind
FROM call_edges e
JOIN symbols s ON s.id = e.callee_id
WHERE e.caller_id = ?
ORDER BY e.edge_kind, s.name;

-- name: list_symbols_in_file
SELECT id, usr, name, kind, file, line, decl_file, decl_line, signature
FROM symbols
WHERE decl_file = ?1 OR file = ?1
ORDER BY decl_file, decl_line, name
LIMIT ?2;
