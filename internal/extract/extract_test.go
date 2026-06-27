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

func TestCompDBDigest_StableUnderReordering(t *testing.T) {
	a := []CompDBEntry{
		{Directory: "/w", File: "a.c", Arguments: []string{"clang", "-c", "a.c"}},
		{Directory: "/w", File: "b.c", Arguments: []string{"clang", "-c", "b.c"}},
		{Directory: "/w", File: "c.c", Arguments: []string{"clang", "-c", "c.c"}},
	}
	b := []CompDBEntry{a[2], a[0], a[1]}
	if CompDBDigest(a) != CompDBDigest(b) {
		t.Fatalf("entry order changed the digest:\n  a=%s\n  b=%s", CompDBDigest(a), CompDBDigest(b))
	}
}

func TestCompDBDigest_SensitiveToCommandChange(t *testing.T) {
	a := []CompDBEntry{
		{Directory: "/w", File: "a.c", Arguments: []string{"clang", "-c", "a.c"}},
	}
	b := []CompDBEntry{
		{Directory: "/w", File: "a.c", Arguments: []string{"clang", "-O2", "-c", "a.c"}},
	}
	if CompDBDigest(a) == CompDBDigest(b) {
		t.Fatalf("argument change didn't change digest")
	}
}

func TestCompDBDigest_SensitiveToFileChange(t *testing.T) {
	a := []CompDBEntry{
		{Directory: "/w", File: "a.c", Arguments: []string{"clang", "-c", "a.c"}},
	}
	b := []CompDBEntry{
		{Directory: "/w", File: "b.c", Arguments: []string{"clang", "-c", "a.c"}},
	}
	if CompDBDigest(a) == CompDBDigest(b) {
		t.Fatalf("file change didn't change digest")
	}
}

func TestExtractSignatureFromHover_PlaintextMarkup(t *testing.T) {
	body := []byte(`{"kind":"plaintext","value":"function dispatch\n\nprovided by \"shared.h\"\n\n→ int\n\nParameters:\n\n- op_t fn (aka int (*)(int))\n- int x\n\nint dispatch(op_t fn, int x)"}`)
	got := extractSignatureFromHover(body)
	want := "int dispatch(op_t fn, int x)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestExtractSignatureFromHover_MarkdownWithCodeFence(t *testing.T) {
	body := []byte(`{"kind":"markdown","value":"### function foo\n\n` + "```" + `cpp\nint foo(int x);\n` + "```" + `"}`)
	got := extractSignatureFromHover(body)
	want := "int foo(int x)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestExtractSignatureFromHover_MarkedStringArray(t *testing.T) {
	body := []byte(`[{"language":"cpp","value":"int foo(int x)"}, "// trailing prose"]`)
	got := extractSignatureFromHover(body)
	// The last item is plain prose; the first contained the signature.
	// We take the last non-empty line per item then keep the most recent one.
	want := "// trailing prose"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestExtractSignatureFromHover_Empty(t *testing.T) {
	if got := extractSignatureFromHover(nil); got != "" {
		t.Fatalf("nil → %q want empty", got)
	}
	if got := extractSignatureFromHover([]byte(`""`)); got != "" {
		t.Fatalf("empty string → %q want empty", got)
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
