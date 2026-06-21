// Package store owns the SQLite index: schema definition, write path
// (rebuild-from-scratch, never migrate), and read path used by MCP tools.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

//go:embed queries.sql
var queriesSQL string

// queries maps a `-- name: foo` block from queries.sql to its SQL body.
var queries = parseQueries(queriesSQL)

func parseQueries(src string) map[string]string {
	out := map[string]string{}
	var name string
	var buf strings.Builder
	flush := func() {
		if name != "" {
			out[name] = strings.TrimSpace(buf.String())
		}
		buf.Reset()
	}
	for line := range strings.SplitSeq(src, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "-- name:") {
			flush()
			name = strings.TrimSpace(strings.TrimPrefix(trim, "-- name:"))
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	flush()
	return out
}

// q looks up a named query parsed from queries.sql. Panics on a typo —
// these names are compile-time constants in our code.
func q(name string) string {
	s, ok := queries[name]
	if !ok {
		panic("store: unknown query: " + name)
	}
	return s
}

// Symbol is one row of the symbols table, hydrated for callers.
type Symbol struct {
	ID        int64
	USR       string
	Name      string
	Kind      string
	File      string // relative to ProjectRoot, see architecture §5.2
	Line      int
	Signature string
}

// Edge is one row of call_edges, by USR. The writer resolves USRs to ids
// once both endpoints exist.
type Edge struct {
	CallerUSR string
	CalleeUSR string
}

// Store wraps an *sql.DB and is safe for concurrent reads. The daemon may
// swap the underlying DB at any time via Swap; readers hold a snapshot
// pointer for the duration of one call, so an in-flight query is never
// torn down mid-flight.
type Store struct {
	db atomic.Pointer[sql.DB]

	swapMu sync.Mutex // serializes Swap so we never leak the old DB
}

// Open opens an existing index database read-only.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}
	s := &Store{}
	s.db.Store(db)
	return s, nil
}

// Create creates a fresh database at path, applies the schema, and
// returns it open read/write. Caller must Close when done writing.
func Create(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return db, nil
}

// WriteIndex writes symbols+edges to a fresh DB at path. The DB is
// rebuilt from scratch every time — never migrate (architecture §8.2).
// Edges whose endpoints don't both exist in symbols are silently
// dropped; that situation only arises for external/unresolved symbols
// the extractor couldn't define.
func WriteIndex(path string, symbols []Symbol, edges []Edge) error {
	db, err := Create(path)
	if err != nil {
		return err
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	insSym, err := tx.Prepare(q("insert_symbol"))
	if err != nil {
		return fmt.Errorf("prepare insert_symbol: %w", err)
	}
	defer insSym.Close()
	insFTS, err := tx.Prepare(q("insert_symbol_fts"))
	if err != nil {
		return fmt.Errorf("prepare insert_symbol_fts: %w", err)
	}
	defer insFTS.Close()

	usrToID := make(map[string]int64, len(symbols))
	for _, sym := range symbols {
		var id int64
		if err := insSym.QueryRow(sym.USR, sym.Name, sym.Kind, sym.File, sym.Line, sym.Signature).Scan(&id); err != nil {
			return fmt.Errorf("insert symbol %q: %w", sym.USR, err)
		}
		usrToID[sym.USR] = id
		if _, err := insFTS.Exec(id, sym.Name, sym.Signature); err != nil {
			return fmt.Errorf("insert fts %q: %w", sym.USR, err)
		}
	}

	insEdge, err := tx.Prepare(q("insert_call_edge"))
	if err != nil {
		return fmt.Errorf("prepare insert_call_edge: %w", err)
	}
	defer insEdge.Close()

	for _, e := range edges {
		caller, okC := usrToID[e.CallerUSR]
		callee, okE := usrToID[e.CalleeUSR]
		if !okC || !okE {
			continue
		}
		if _, err := insEdge.Exec(caller, callee); err != nil {
			return fmt.Errorf("insert edge: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Swap atomically replaces the live DB handle with the one for the file
// at newPath, closing the previous handle. Safe to call concurrently with
// reads — see Store doc.
func (s *Store) Swap(newPath string) error {
	s.swapMu.Lock()
	defer s.swapMu.Unlock()

	newDB, err := sql.Open("sqlite", "file:"+newPath+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open swap target %s: %w", newPath, err)
	}
	if err := newDB.Ping(); err != nil {
		newDB.Close()
		return fmt.Errorf("ping swap target %s: %w", newPath, err)
	}
	old := s.db.Swap(newDB)
	if old != nil {
		// Best-effort: in-flight queries hold their own *Rows; closing the
		// handle just stops new statements being prepared on it.
		old.Close()
	}
	return nil
}

// Close releases the underlying handle.
func (s *Store) Close() error {
	if db := s.db.Load(); db != nil {
		return db.Close()
	}
	return nil
}

// SearchSymbol runs an FTS5 MATCH against name/signature.
func (s *Store) SearchSymbol(query string, limit int) ([]Symbol, error) {
	if limit <= 0 {
		limit = 50
	}
	db := s.db.Load()
	rows, err := db.Query(q("search_symbol_fts"), query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSymbols(rows)
}

// GetSymbol returns the symbol with this id, or sql.ErrNoRows.
func (s *Store) GetSymbol(id int64) (Symbol, error) {
	db := s.db.Load()
	row := db.QueryRow(q("get_symbol"), id)
	return scanSymbol(row)
}

// Callers returns symbols that directly call id.
func (s *Store) Callers(id int64) ([]Symbol, error) {
	db := s.db.Load()
	rows, err := db.Query(q("get_callers"), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSymbols(rows)
}

// Callees returns symbols directly called by id.
func (s *Store) Callees(id int64) ([]Symbol, error) {
	db := s.db.Load()
	rows, err := db.Query(q("get_callees"), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSymbols(rows)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSymbol(r rowScanner) (Symbol, error) {
	var s Symbol
	err := r.Scan(&s.ID, &s.USR, &s.Name, &s.Kind, &s.File, &s.Line, &s.Signature)
	return s, err
}

func scanSymbols(rows *sql.Rows) ([]Symbol, error) {
	var out []Symbol
	for rows.Next() {
		s, err := scanSymbol(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
