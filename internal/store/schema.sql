CREATE TABLE symbols (
  id        INTEGER PRIMARY KEY,
  usr       TEXT UNIQUE,
  name      TEXT,
  kind      TEXT,
  file      TEXT,    -- definition location, relative to ProjectRoot
  line      INTEGER, -- definition line, 1-based
  decl_file TEXT,    -- declaration location (header), relative to ProjectRoot; "" if same as file
  decl_line INTEGER, -- declaration line, 1-based; 0 if same as line
  signature TEXT
);
CREATE INDEX idx_decl_file ON symbols(decl_file);

CREATE VIRTUAL TABLE symbols_fts USING fts5(
  name, signature, content='symbols', content_rowid='id',
  tokenize='unicode61 separators _'
);

CREATE TABLE call_edges (
  caller_id INTEGER REFERENCES symbols(id),
  callee_id INTEGER REFERENCES symbols(id),
  -- 'direct'   = clangd-confirmed direct call, see callHierarchy.
  -- 'indirect' = synthesized: an indirect call site of a function-pointer
  --             type T inside the caller, paired with an address-taken
  --             function of matching T. Sound over-approximation
  --             (Andersen-style); see architecture §6.5.
  edge_kind TEXT NOT NULL DEFAULT 'direct'
);
CREATE INDEX idx_caller ON call_edges(caller_id, edge_kind);
CREATE INDEX idx_callee ON call_edges(callee_id, edge_kind);
