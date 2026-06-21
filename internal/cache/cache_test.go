package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSum(t *testing.T) {
	if Sum([]byte("abc")) != Sum([]byte("abc")) {
		t.Fatal("non-deterministic")
	}
	if Sum([]byte("abc")) == Sum([]byte("abd")) {
		t.Fatal("collided")
	}
}

func TestInputDigestStable(t *testing.T) {
	a := InputDigest("cdb", map[string]Digest{"a.c": "h1", "b.c": "h2"})
	b := InputDigest("cdb", map[string]Digest{"b.c": "h2", "a.c": "h1"})
	if a != b {
		t.Fatalf("order-sensitive: %s vs %s", a, b)
	}
}

func TestWholeBuildRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c, err := NewWholeBuild(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	d := Sum([]byte("input"))
	if _, err := c.Lookup(d); err != ErrMiss {
		t.Fatalf("expected miss, got %v", err)
	}
	src := filepath.Join(dir, "src.db")
	if err := os.WriteFile(src, []byte("db-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(d, src); err != nil {
		t.Fatal(err)
	}
	got, err := c.Lookup(d)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	b, _ := os.ReadFile(got)
	if string(b) != "db-bytes" {
		t.Fatalf("wrong bytes: %s", b)
	}
}

func TestPerFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c, err := NewPerFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	k := PerFileKey{FileDigest: "f", CommandDigest: "c"}
	if _, err := c.Lookup(k); err != ErrMiss {
		t.Fatalf("expected miss, got %v", err)
	}
	payload, _ := json.Marshal(map[string]any{"hello": "world"})
	if err := c.Put(k, &PerFileEntry{Payload: payload}); err != nil {
		t.Fatal(err)
	}
	got, err := c.Lookup(k)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload) != string(payload) {
		t.Fatalf("payload mismatch: %s", got.Payload)
	}
}

func TestEmptyRootDisables(t *testing.T) {
	c, err := NewWholeBuild("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Lookup("anything"); err != ErrMiss {
		t.Fatalf("disabled cache must miss, got %v", err)
	}
	if err := c.Put("x", "y"); err != nil {
		t.Fatalf("disabled Put should noop, got %v", err)
	}

	pf, _ := NewPerFile("")
	if _, err := pf.Lookup(PerFileKey{}); err != ErrMiss {
		t.Fatalf("disabled per-file must miss, got %v", err)
	}
	if err := pf.Put(PerFileKey{}, &PerFileEntry{}); err != nil {
		t.Fatalf("disabled per-file Put should noop, got %v", err)
	}
}
