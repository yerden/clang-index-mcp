package extract

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
func LoadCompDB(path string) ([]CompDBEntry, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var entries []CompDBEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, nil, err
	}
	if len(entries) == 0 {
		return nil, nil, errors.New("extract: compdb is empty")
	}
	return entries, raw, nil
}
