package store

import (
	"path/filepath"
	"testing"
)

func TestWriteIndexAndQuery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ix.db")

	symbols := []Symbol{
		{USR: "c:@F@foo", Name: "foo", Kind: "Function", File: "a.c", Line: 1, Signature: "int foo()"},
		{USR: "c:@F@bar", Name: "bar", Kind: "Function", File: "a.c", Line: 5, Signature: "int bar()"},
		{USR: "c:@F@baz_widget", Name: "baz_widget", Kind: "Function", File: "b.c", Line: 9, Signature: "void baz_widget()"},
	}
	edges := []Edge{
		{CallerUSR: "c:@F@foo", CalleeUSR: "c:@F@bar"},
		{CallerUSR: "c:@F@bar", CalleeUSR: "c:@F@foo"}, // cycle: A→B→A
		{CallerUSR: "c:@F@foo", CalleeUSR: "c:@F@foo"}, // self
		{CallerUSR: "c:@F@foo", CalleeUSR: "c:@F@nonexistent"},
	}
	if err := WriteIndex(path, symbols, edges); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	hits, err := st.SearchSymbol("foo", 10)
	if err != nil {
		t.Fatalf("SearchSymbol: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "foo" {
		t.Fatalf("expected one hit for 'foo', got %+v", hits)
	}

	// tokenize='unicode61 separators _' should split baz_widget into baz, widget
	hits, err = st.SearchSymbol("widget", 10)
	if err != nil {
		t.Fatalf("SearchSymbol widget: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "baz_widget" {
		t.Fatalf("expected baz_widget hit for 'widget', got %+v", hits)
	}

	fooID := hits[0].ID
	_ = fooID
	hits, _ = st.SearchSymbol("foo", 10)
	fooID = hits[0].ID

	sym, err := st.GetSymbol(fooID)
	if err != nil {
		t.Fatalf("GetSymbol: %v", err)
	}
	if sym.USR != "c:@F@foo" {
		t.Fatalf("wrong sym: %+v", sym)
	}

	callers, err := st.Callers(fooID)
	if err != nil {
		t.Fatalf("Callers: %v", err)
	}
	// foo is called by bar and foo itself
	if len(callers) != 2 {
		t.Fatalf("expected 2 callers of foo, got %+v", callers)
	}

	callees, err := st.Callees(fooID)
	if err != nil {
		t.Fatalf("Callees: %v", err)
	}
	// foo calls bar and itself; the unresolved edge to nonexistent is dropped
	if len(callees) != 2 {
		t.Fatalf("expected 2 callees of foo, got %+v", callees)
	}
}

func TestSwap(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.db")
	b := filepath.Join(dir, "b.db")

	if err := WriteIndex(a, []Symbol{{USR: "u1", Name: "in_a", Kind: "Function", File: "a.c"}}, nil); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := WriteIndex(b, []Symbol{{USR: "u2", Name: "in_b", Kind: "Function", File: "b.c"}}, nil); err != nil {
		t.Fatalf("write b: %v", err)
	}

	st, err := Open(a)
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	defer st.Close()

	hits, _ := st.SearchSymbol("in_a", 5)
	if len(hits) != 1 {
		t.Fatalf("pre-swap: expected in_a, got %+v", hits)
	}

	if err := st.Swap(b); err != nil {
		t.Fatalf("swap: %v", err)
	}
	hits, _ = st.SearchSymbol("in_b", 5)
	if len(hits) != 1 {
		t.Fatalf("post-swap: expected in_b, got %+v", hits)
	}
	hits, _ = st.SearchSymbol("in_a", 5)
	if len(hits) != 0 {
		t.Fatalf("post-swap: should not see in_a, got %+v", hits)
	}
}
