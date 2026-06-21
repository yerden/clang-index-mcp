CREATE TABLE symbols (
  id        INTEGER PRIMARY KEY,
  usr       TEXT UNIQUE,
  name      TEXT,
  kind      TEXT,
  file      TEXT,
  line      INTEGER,
  signature TEXT
);

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
