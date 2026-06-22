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
	ID   int64
	USR  string
	Name string
	Kind string
	// File / Line — definition location (the .c/.cpp/.m file holding the body).
	// Relative to ProjectRoot per architecture §5.2.
	File string
	Line int
	// DeclFile / DeclLine — declaration location (typically the header).
	// Empty / 0 when the declaration coincides with the definition (e.g.
	// a static function defined and used in one TU).
	DeclFile  string
	DeclLine  int
	Signature string
}

// Edge is one row of call_edges, by USR. The writer resolves USRs to ids
// once both endpoints exist.
type Edge struct {
	CallerUSR string
	CalleeUSR string
}

// Address-take category values. See categories.go in internal/extract
// for the canonical vocabulary surfaced to MCP consumers; these mirror
// it. The writer normalizes unknown values to CategoryOther.
const (
	CategoryCompared     = "compared"
	CategoryArgTo        = "arg_to"
	CategoryStoredIn     = "stored_in"
	CategoryArrayInit    = "array_init"
	CategoryAssignedTo   = "assigned_to"
	CategoryReturnedFrom = "returned_from"
	CategoryOther        = "other"
)

func isKnownCategory(c string) bool {
	switch c {
	case CategoryCompared, CategoryArgTo, CategoryStoredIn,
		CategoryArrayInit, CategoryAssignedTo, CategoryReturnedFrom, CategoryOther:
		return true
	}
	return false
}

// AddressTake is one row of address_takes, by USR. Position is the
// source location of the address-take expression itself, not of the
// function whose address is taken.
type AddressTake struct {
	FunctionUSR   string `json:"function_usr"`
	TakenAtFile   string `json:"taken_at_file"`   // relative to ProjectRoot
	TakenAtLine   int    `json:"taken_at_line"`   // 1-based
	FnPtrType     string `json:"fn_ptr_type"`
	Category      string `json:"category"`
	ContextDetail string `json:"context_detail"`
}

// IndirectCallSite is one row of indirect_call_sites, by USR.
type IndirectCallSite struct {
	CallerUSR  string `json:"caller_usr"`
	File       string `json:"file"` // relative to ProjectRoot
	Line       int    `json:"line"` // 1-based
	CalleeType string `json:"callee_type"`
	CalleeExpr string `json:"callee_expr"`
}

// AddressTakeRow is the read-side hydrated form: Symbol joined with the
// fact's per-row columns.
type AddressTakeRow struct {
	Symbol
	TakenAtFile   string `json:"taken_at_file"`
	TakenAtLine   int    `json:"taken_at_line"`
	FnPtrType     string `json:"fn_ptr_type"`
	Category      string `json:"category"`
	ContextDetail string `json:"context_detail"`
}

// IndirectCallSiteRow is the read-side hydrated form: caller Symbol
// (under field Caller) plus per-site columns. We don't embed Symbol
// because its File/Line columns would collide with the site's own
// SiteFile/SiteLine.
type IndirectCallSiteRow struct {
	Caller     Symbol `json:"caller"`
	SiteFile   string `json:"site_file"`
	SiteLine   int    `json:"site_line"`
	CalleeType string `json:"callee_type"`
	CalleeExpr string `json:"callee_expr"`
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

// WriteIndex writes symbols+edges plus the raw function-pointer facts
// (address_takes + indirect_call_sites) to a fresh DB at path. The DB
// is rebuilt from scratch every time — never migrate (architecture
// §8.2). Rows whose USR endpoints aren't present in symbols are
// silently dropped; that arises for external/unresolved symbols.
//
// Pass nil for facts you don't have — old callers (tests, etc.) can
// invoke the four-argument WriteIndexWithFacts variant when they need
// to wire those tables.
func WriteIndex(path string, symbols []Symbol, edges []Edge) error {
	return WriteIndexWithFacts(path, symbols, edges, nil, nil)
}

// WriteIndexWithFacts is the full writer.
func WriteIndexWithFacts(path string, symbols []Symbol, edges []Edge, addressTakes []AddressTake, indirectSites []IndirectCallSite) error {
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
		if err := insSym.QueryRow(sym.USR, sym.Name, sym.Kind, sym.File, sym.Line, sym.DeclFile, sym.DeclLine, sym.Signature).Scan(&id); err != nil {
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

	if len(addressTakes) > 0 {
		insAT, err := tx.Prepare(q("insert_address_take"))
		if err != nil {
			return fmt.Errorf("prepare insert_address_take: %w", err)
		}
		defer insAT.Close()
		for _, a := range addressTakes {
			fnID, ok := usrToID[a.FunctionUSR]
			if !ok {
				continue
			}
			cat := a.Category
			if !isKnownCategory(cat) {
				cat = CategoryOther
			}
			if _, err := insAT.Exec(fnID, a.TakenAtFile, a.TakenAtLine, a.FnPtrType, cat, a.ContextDetail); err != nil {
				return fmt.Errorf("insert address_take: %w", err)
			}
		}
	}

	if len(indirectSites) > 0 {
		insICS, err := tx.Prepare(q("insert_indirect_call_site"))
		if err != nil {
			return fmt.Errorf("prepare insert_indirect_call_site: %w", err)
		}
		defer insICS.Close()
		for _, s := range indirectSites {
			callerID, ok := usrToID[s.CallerUSR]
			if !ok {
				continue
			}
			if _, err := insICS.Exec(callerID, s.File, s.Line, s.CalleeType, s.CalleeExpr); err != nil {
				return fmt.Errorf("insert indirect_call_site: %w", err)
			}
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
	err := r.Scan(&s.ID, &s.USR, &s.Name, &s.Kind, &s.File, &s.Line, &s.DeclFile, &s.DeclLine, &s.Signature)
	return s, err
}

// ListSymbolsInFile returns all symbols whose declaration file or
// definition file matches path. Path is matched exactly against
// symbols.decl_file / symbols.file — callers must pass the same
// repo-relative form used at write time.
func (s *Store) ListSymbolsInFile(path string, limit int) ([]Symbol, error) {
	if limit <= 0 {
		limit = 200
	}
	db := s.db.Load()
	rows, err := db.Query(q("list_symbols_in_file"), path, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSymbols(rows)
}

// GetAddressTakeSites returns every recorded address-take whose
// function_id matches the given symbol id.
func (s *Store) GetAddressTakeSites(functionID int64, limit int) ([]AddressTakeRow, error) {
	if limit <= 0 {
		limit = 200
	}
	db := s.db.Load()
	rows, err := db.Query(q("get_address_take_sites"), functionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAddressTakeRows(rows)
}

// FindAddressTakes filters address_takes by exact fn_ptr_type, exact
// category, and SQL LIKE pattern over context_detail. Empty filters
// match everything.
func (s *Store) FindAddressTakes(typeFilter, category, detailPattern string, limit int) ([]AddressTakeRow, error) {
	if limit <= 0 {
		limit = 200
	}
	db := s.db.Load()
	rows, err := db.Query(q("find_address_takes"), typeFilter, category, detailPattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAddressTakeRows(rows)
}

// GetIndirectCallSitesByCaller returns indirect call sites contained
// in the given caller function.
func (s *Store) GetIndirectCallSitesByCaller(callerID int64, limit int) ([]IndirectCallSiteRow, error) {
	if limit <= 0 {
		limit = 200
	}
	db := s.db.Load()
	rows, err := db.Query(q("get_indirect_call_sites_by_caller"), callerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanIndirectCallSiteRows(rows)
}

// ListIndirectCallSites enumerates all indirect call sites, optionally
// filtered by exact callee_type.
func (s *Store) ListIndirectCallSites(typeFilter string, limit int) ([]IndirectCallSiteRow, error) {
	if limit <= 0 {
		limit = 200
	}
	db := s.db.Load()
	rows, err := db.Query(q("list_indirect_call_sites"), typeFilter, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanIndirectCallSiteRows(rows)
}

func scanAddressTakeRows(rows *sql.Rows) ([]AddressTakeRow, error) {
	var out []AddressTakeRow
	for rows.Next() {
		var r AddressTakeRow
		if err := rows.Scan(
			&r.TakenAtFile, &r.TakenAtLine, &r.FnPtrType, &r.Category, &r.ContextDetail,
			&r.ID, &r.USR, &r.Name, &r.Kind, &r.File, &r.Line, &r.DeclFile, &r.DeclLine, &r.Signature,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanIndirectCallSiteRows(rows *sql.Rows) ([]IndirectCallSiteRow, error) {
	var out []IndirectCallSiteRow
	for rows.Next() {
		var r IndirectCallSiteRow
		if err := rows.Scan(
			&r.SiteFile, &r.SiteLine, &r.CalleeType, &r.CalleeExpr,
			&r.Caller.ID, &r.Caller.USR, &r.Caller.Name, &r.Caller.Kind,
			&r.Caller.File, &r.Caller.Line, &r.Caller.DeclFile, &r.Caller.DeclLine, &r.Caller.Signature,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
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
