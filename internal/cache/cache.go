// Package cache provides content-digest keyed reuse at two granularities:
//
//   - WholeBuild  — used by `clang-index build` to skip the entire clangd
//     pipeline when an identical input snapshot was already indexed.
//   - PerFile     — used inside `extract` to skip re-querying clangd for
//     individual translation units whose content+flags haven't moved.
//
// Both layers key purely on raw-bytes content digests; no source-control
// identity is involved (architecture §7). No eviction policy is provided —
// manual nuke-and-rebuild is the accepted fallback (architecture §6.3).
//
// Layout on disk: both layers live under one user-facing cache root,
// split into "whole/" and "per-file/" subdirs (see WholeBuildSubdir /
// PerFileSubdir). The two subdirs are independent on the hot path — a
// whole-build hit short-circuits before clangd starts and never consults
// per-file; per-file is consulted inside extract.Run only on whole-build
// miss. Sharing the parent dir means a single nuke clears both layers,
// which is the right behavior after a schema change (CLAUDE.md §17).
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// ErrMiss is returned by lookups when the requested key is absent.
var ErrMiss = errors.New("cache: miss")

// Digest is a hex-encoded SHA-256 of some byte stream.
type Digest string

// Sum returns the SHA-256 digest of b as hex.
func Sum(b []byte) Digest {
	h := sha256.Sum256(b)
	return Digest(hex.EncodeToString(h[:]))
}

// SumFile returns the SHA-256 of a file's raw bytes (no normalization).
// architecture §7.2: keys are over raw bytes.
func SumFile(path string) (Digest, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return Digest(hex.EncodeToString(h.Sum(nil))), nil
}

// SumStrings returns the SHA-256 of a deterministic concatenation of strings.
// Order matters: the caller is responsible for sorting if order shouldn't.
func SumStrings(parts ...string) Digest {
	h := sha256.New()
	for _, p := range parts {
		// length prefix prevents joiner-collision aliasing
		fmt.Fprintf(h, "%d:", len(p))
		h.Write([]byte(p))
	}
	return Digest(hex.EncodeToString(h.Sum(nil)))
}

// WholeBuildSubdir returns the conventional whole-build subdirectory
// under a unified cache root. Empty root → empty (cache disabled).
func WholeBuildSubdir(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, "whole")
}

// PerFileSubdir returns the conventional per-file subdirectory under a
// unified cache root. Empty root → empty (cache disabled).
func PerFileSubdir(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, "per-file")
}

// WholeBuild is the build-level cache. The on-disk layout is one file per
// digest under root, holding the cached *index.db verbatim. Callers either
// move/symlink the hit into place or copy it.
type WholeBuild struct {
	root string
}

// NewWholeBuild prepares the on-disk directory. Empty root disables the
// cache (Lookup always misses, Put is a no-op).
func NewWholeBuild(root string) (*WholeBuild, error) {
	if root == "" {
		return &WholeBuild{}, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &WholeBuild{root: root}, nil
}

// InputDigest digests a sorted list of (path, fileDigest) plus an opaque
// compdb digest. Sorting is required so identical inputs produce the
// identical key regardless of walk order.
func InputDigest(compdbDigest Digest, files map[string]Digest) Digest {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	fmt.Fprintf(h, "compdb:%s\n", compdbDigest)
	for _, k := range keys {
		fmt.Fprintf(h, "%s\t%s\n", k, files[k])
	}
	return Digest(hex.EncodeToString(h.Sum(nil)))
}

func (c *WholeBuild) path(d Digest) string {
	return filepath.Join(c.root, string(d)+".db")
}

// Lookup returns the on-disk path of a cached index.db for this digest,
// or ErrMiss if absent.
func (c *WholeBuild) Lookup(d Digest) (string, error) {
	if c.root == "" {
		return "", ErrMiss
	}
	p := c.path(d)
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return "", ErrMiss
		}
		return "", err
	}
	return p, nil
}

// Put copies the freshly-built dbPath into the cache under d. The source
// file must already exist and be a complete index.
func (c *WholeBuild) Put(d Digest, dbPath string) error {
	if c.root == "" {
		return nil
	}
	src, err := os.Open(dbPath)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(c.root, "cache-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, c.path(d))
}

// PerFileKey couples a TU's own content digest with the digest of its
// compile command (architecture §7.2). Note: this does *not* include
// transitive header content — accepted gap per architecture §7.2 caveat.
type PerFileKey struct {
	FileDigest    Digest
	CommandDigest Digest
}

func (k PerFileKey) String() string {
	return string(k.FileDigest) + ":" + string(k.CommandDigest)
}

// PerFileEntry is whatever the extractor wants to memoize per TU. It's
// stored opaquely as JSON; extract decides the schema.
type PerFileEntry struct {
	Payload json.RawMessage
}

// PerFile is the per-TU cache. Same simple per-file storage layout as
// WholeBuild — one JSON file per key.
type PerFile struct {
	root string
}

// NewPerFile prepares the on-disk directory. Empty root disables the cache.
func NewPerFile(root string) (*PerFile, error) {
	if root == "" {
		return &PerFile{}, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &PerFile{root: root}, nil
}

func (c *PerFile) path(k PerFileKey) string {
	// File names should be safe; both halves are hex.
	return filepath.Join(c.root, string(k.FileDigest)+"_"+string(k.CommandDigest)+".json")
}

// Lookup returns the entry for k, or ErrMiss.
func (c *PerFile) Lookup(k PerFileKey) (*PerFileEntry, error) {
	if c.root == "" {
		return nil, ErrMiss
	}
	b, err := os.ReadFile(c.path(k))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrMiss
		}
		return nil, err
	}
	return &PerFileEntry{Payload: b}, nil
}

// Put writes the entry for k. Atomic via temp+rename to avoid torn reads.
func (c *PerFile) Put(k PerFileKey, e *PerFileEntry) error {
	if c.root == "" {
		return nil
	}
	tmp, err := os.CreateTemp(c.root, "pf-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(e.Payload); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, c.path(k))
}
