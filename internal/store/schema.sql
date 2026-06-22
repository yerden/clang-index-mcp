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
  callee_id INTEGER REFERENCES symbols(id)
);
CREATE INDEX idx_caller ON call_edges(caller_id);
CREATE INDEX idx_callee ON call_edges(callee_id);

-- Raw facts for function-pointer dispatch (architecture §6.5).
-- These are intentionally NOT joined into synthesized edges in
-- call_edges; consumers (typically an AI agent) decide how to bridge
-- the indirect-call gap using the context column.

CREATE TABLE address_takes (
  function_id    INTEGER REFERENCES symbols(id), -- whose address is taken
  taken_at_file  TEXT,                           -- file relative to ProjectRoot
  taken_at_line  INTEGER,                        -- 1-based
  fn_ptr_type    TEXT,                           -- canonical function-pointer type
  -- One of: 'compared' | 'arg_to' | 'stored_in' | 'array_init' |
  -- 'assigned_to' | 'returned_from' | 'other'.
  -- Resolved by precedence at extract time; see categories.go.
  category       TEXT,
  -- Free-form qualifier whose syntax depends on category:
  --   arg_to        -> '<callee_name>#<arg_index>'
  --   stored_in     -> '<struct_type>.<field>'
  --   array_init    -> '<array_name>' or '<array_name>[<index>]'
  --   assigned_to   -> '<var_name>'
  --   returned_from -> '<enclosing_function_name>'
  --   compared, other -> ''
  context_detail TEXT
);
CREATE INDEX idx_at_function ON address_takes(function_id);
CREATE INDEX idx_at_category_type ON address_takes(category, fn_ptr_type);

CREATE TABLE indirect_call_sites (
  caller_id   INTEGER REFERENCES symbols(id), -- function containing the call
  file        TEXT,                           -- relative to ProjectRoot
  line        INTEGER,                        -- 1-based
  callee_type TEXT,                           -- canonical fn-ptr type at site
  callee_expr TEXT                            -- e.g. 'fn', 'ops[i]', '<base>.cb'
);
CREATE INDEX idx_ics_caller ON indirect_call_sites(caller_id);
CREATE INDEX idx_ics_type ON indirect_call_sites(callee_type);
