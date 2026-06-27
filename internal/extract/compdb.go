package extract

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"github.com/yerden/clang-index-mcp/internal/cache"
)

// CompDBEntry is one record of the JSON Compilation Database. We accept
// either the `arguments` or `command` form; `Arguments()` returns a
// normalized slice for digesting purposes.
type CompDBEntry struct {
	Directory string   `json:"directory"`
	File      string   `json:"file"`
	Command   string   `json:"command,omitempty"`
	Arguments []string `json:"arguments,omitempty"`
	Output    string   `json:"output,omitempty"`
}

// AbsFile returns the absolute path to the TU referenced by this entry.
// Per the JSON Compilation Database spec, `file` may already be absolute;
// otherwise it is resolved against `directory`.
func (e CompDBEntry) AbsFile() string {
	if filepath.IsAbs(e.File) {
		return e.File
	}
	return filepath.Clean(filepath.Join(e.Directory, e.File))
}

// LoadCompDB reads compile_commands.json from path.
func LoadCompDB(path string) ([]CompDBEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []CompDBEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("extract: compdb is empty")
	}
	return entries, nil
}

// CompDBDigest returns a content-digest of the parsed compdb's command
// set, insensitive to JSON serialization noise (indentation, key order,
// entry order) but sensitive to any change in (file, directory,
// arguments). Used as the compdb half of the whole-build cache key.
//
// Why parse-and-canonicalize here rather than hash raw bytes
// (architecture §7.1): build systems regenerate compile_commands.json
// on every reconfigure with timestamps, dependency-graph traversal
// order, and formatting choices that vary across runs even when no
// actual compile command changed. Hashing raw bytes makes every CI
// run miss the whole-build cache. The compdb is structured metadata
// produced by a build system — not source code — so canonicalizing
// the structure is well-defined and matches what we actually want the
// cache key to track. This is a conscious divergence from §7.2's
// "no normalization" rule, which applies to source-file bytes.
//
// The canonical form per entry comes from commandDigest, which is
// also what the per-file cache key uses — so the two layers stay
// consistent about which (Directory, Arguments) tuples count as "the
// same command."
func CompDBDigest(entries []CompDBEntry) cache.Digest {
	type pair struct {
		abs string
		cmd cache.Digest
	}
	pairs := make([]pair, len(entries))
	for i, e := range entries {
		pairs[i] = pair{abs: e.AbsFile(), cmd: commandDigest(e)}
	}
	// Sort by (abs, cmd) so entry order in the JSON doesn't matter and
	// duplicate file entries with different commands are still
	// distinguished deterministically.
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].abs != pairs[j].abs {
			return pairs[i].abs < pairs[j].abs
		}
		return pairs[i].cmd < pairs[j].cmd
	})
	parts := make([]string, 0, len(pairs)*2)
	for _, p := range pairs {
		parts = append(parts, p.abs, string(p.cmd))
	}
	return cache.SumStrings(parts...)
}
