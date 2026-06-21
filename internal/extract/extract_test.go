package extract

import (
	"encoding/json"
	"testing"
)

func TestDecodeDocumentSymbolsHierarchical(t *testing.T) {
	raw := json.RawMessage(`[
	  {"name":"foo","kind":12,"detail":"int foo()","range":{"start":{"line":0,"character":0},"end":{"line":2,"character":1}},"selectionRange":{"start":{"line":0,"character":4},"end":{"line":0,"character":7}},"children":[
	    {"name":"x","kind":13,"range":{"start":{"line":1,"character":2},"end":{"line":1,"character":5}},"selectionRange":{"start":{"line":1,"character":2},"end":{"line":1,"character":3}}}
	  ]},
	  {"name":"bar","kind":12,"range":{"start":{"line":3,"character":0},"end":{"line":4,"character":1}},"selectionRange":{"start":{"line":3,"character":4},"end":{"line":3,"character":7}}}
	]`)
	got, err := decodeDocumentSymbols(raw, "file:///x.c")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d symbols, want 3", len(got))
	}
	if got[0].Name != "foo" || got[1].Name != "x" || got[2].Name != "bar" {
		t.Fatalf("walk order wrong: %+v", got)
	}
	if got[0].Detail != "int foo()" {
		t.Fatalf("lost detail: %q", got[0].Detail)
	}
}

func TestDecodeDocumentSymbolsLegacy(t *testing.T) {
	raw := json.RawMessage(`[
	  {"name":"foo","kind":12,"location":{"uri":"file:///x.c","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":3}}}},
	  {"name":"elsewhere","kind":12,"location":{"uri":"file:///other.c","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":3}}}}
	]`)
	got, err := decodeDocumentSymbols(raw, "file:///x.c")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "foo" {
		t.Fatalf("legacy filter wrong: %+v", got)
	}
}

func TestCompDBAbsFile(t *testing.T) {
	e := CompDBEntry{Directory: "/work", File: "a/b.c"}
	if e.AbsFile() != "/work/a/b.c" {
		t.Fatalf("got %s", e.AbsFile())
	}
	e2 := CompDBEntry{Directory: "/work", File: "/already/abs.c"}
	if e2.AbsFile() != "/already/abs.c" {
		t.Fatalf("got %s", e2.AbsFile())
	}
}

func TestRelative(t *testing.T) {
	if got := relative("/work", "/work/sub/a.c"); got != "sub/a.c" {
		t.Fatalf("got %q", got)
	}
	if got := relative("/work", "/other/a.c"); got != "../other/a.c" {
		t.Fatalf("got %q", got)
	}
}
