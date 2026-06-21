-- name: insert_symbol
INSERT INTO symbols (usr, name, kind, file, line, signature)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(usr) DO UPDATE SET
  name=excluded.name,
  kind=excluded.kind,
  file=excluded.file,
  line=excluded.line,
  signature=excluded.signature
RETURNING id;

-- name: insert_symbol_fts
INSERT INTO symbols_fts (rowid, name, signature) VALUES (?, ?, ?);

-- name: insert_call_edge
INSERT INTO call_edges (caller_id, callee_id) VALUES (?, ?);

-- name: lookup_symbol_id_by_usr
SELECT id FROM symbols WHERE usr = ?;

-- name: search_symbol_fts
SELECT s.id, s.usr, s.name, s.kind, s.file, s.line, s.signature
FROM symbols_fts f
JOIN symbols s ON s.id = f.rowid
WHERE symbols_fts MATCH ?
ORDER BY rank
LIMIT ?;

-- name: get_symbol
SELECT id, usr, name, kind, file, line, signature
FROM symbols
WHERE id = ?;

-- name: get_callers
SELECT s.id, s.usr, s.name, s.kind, s.file, s.line, s.signature
FROM call_edges e
JOIN symbols s ON s.id = e.caller_id
WHERE e.callee_id = ?;

-- name: get_callees
SELECT s.id, s.usr, s.name, s.kind, s.file, s.line, s.signature
FROM call_edges e
JOIN symbols s ON s.id = e.callee_id
WHERE e.caller_id = ?;
