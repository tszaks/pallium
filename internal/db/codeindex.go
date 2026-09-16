package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// CodeFile is one indexed source file. ContentSHA is what makes reindexing
// incremental: a file whose hash is unchanged is never reparsed.
type CodeFile struct {
	Path        string `json:"path"`
	Lang        string `json:"lang"`
	SizeBytes   int64  `json:"size_bytes"`
	ContentSHA  string `json:"content_sha"`
	SymbolCount int    `json:"symbol_count"`
	Parser      string `json:"parser"`
}

// CodeSymbol is a declaration found in a file. Kind is a small vocabulary
// shared across languages (func, method, type, class, interface, const, var,
// enum) so a caller can filter without knowing the language.
type CodeSymbol struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Receiver  string `json:"receiver,omitempty"`
	Signature string `json:"signature,omitempty"`
	Doc       string `json:"doc,omitempty"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Exported  bool   `json:"exported"`
}

// CodeImport is one import edge. ToPath is empty for a third-party or stdlib
// import; External records that explicitly so an unresolved in-repo import
// (a bug in resolution) stays distinguishable from a genuine dependency.
type CodeImport struct {
	FromPath string `json:"from_path"`
	RawSpec  string `json:"raw_spec"`
	ToPath   string `json:"to_path,omitempty"`
	Kind     string `json:"kind"`
	External bool   `json:"external"`
}

// CodeRef is an identifier a file references. Stored per distinct name, not
// per occurrence: the question is "does this file touch that symbol", and
// counting call sites would multiply rows for no gain.
type CodeRef struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

type CodeIndexStats struct {
	Files   int `json:"files"`
	Symbols int `json:"symbols"`
	Imports int `json:"imports"`
	Refs    int `json:"refs"`
}

// ReplaceCodeFile writes one file's parse result, replacing whatever was
// stored for that path. Callers wrap batches of these in WithTx.
func (s *Store) ReplaceCodeFile(repoID int64, file CodeFile, symbols []CodeSymbol, imports []CodeImport, refs []CodeRef, indexedAt time.Time) error {
	if err := s.DeleteCodeFile(repoID, file.Path); err != nil {
		return err
	}

	if _, err := s.q.Exec(`
INSERT INTO code_files (repo_id, path, lang, size_bytes, content_sha, symbol_count, parser, indexed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`, repoID, file.Path, file.Lang, file.SizeBytes, file.ContentSHA, len(symbols), file.Parser,
		indexedAt.UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("insert code file %s: %w", file.Path, err)
	}

	for _, symbol := range symbols {
		exported := 0
		if symbol.Exported {
			exported = 1
		}
		if _, err := s.q.Exec(`
INSERT INTO code_symbols (repo_id, path, name, kind, receiver, signature, doc, start_line, end_line, exported)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, repoID, file.Path, symbol.Name, symbol.Kind, symbol.Receiver, symbol.Signature, symbol.Doc,
			symbol.StartLine, symbol.EndLine, exported); err != nil {
			return fmt.Errorf("insert symbol %s in %s: %w", symbol.Name, file.Path, err)
		}
	}

	for _, imp := range imports {
		external := 0
		if imp.External {
			external = 1
		}
		if _, err := s.q.Exec(`
INSERT OR REPLACE INTO code_imports (repo_id, from_path, raw_spec, to_path, kind, external)
VALUES (?, ?, ?, ?, ?, ?)
`, repoID, file.Path, imp.RawSpec, imp.ToPath, imp.Kind, external); err != nil {
			return fmt.Errorf("insert import %s from %s: %w", imp.RawSpec, file.Path, err)
		}
	}

	for _, ref := range refs {
		if _, err := s.q.Exec(`
INSERT OR REPLACE INTO code_refs (repo_id, path, name, name_lower)
VALUES (?, ?, ?, ?)
`, repoID, file.Path, ref.Name, strings.ToLower(ref.Name)); err != nil {
			return fmt.Errorf("insert ref %s in %s: %w", ref.Name, file.Path, err)
		}
	}

	return nil
}

// DeleteCodeFile removes a path from the content index. The FTS rows go with
// the symbols via the schema's delete trigger.
func (s *Store) DeleteCodeFile(repoID int64, path string) error {
	statements := []string{
		"DELETE FROM code_symbols WHERE repo_id = ? AND path = ?",
		"DELETE FROM code_imports WHERE repo_id = ? AND from_path = ?",
		"DELETE FROM code_refs WHERE repo_id = ? AND path = ?",
		"DELETE FROM code_files WHERE repo_id = ? AND path = ?",
	}
	for _, statement := range statements {
		if _, err := s.q.Exec(statement, repoID, path); err != nil {
			return fmt.Errorf("delete code file %s: %w", path, err)
		}
	}
	return nil
}

// CodeFileSHAs returns path to content hash for the whole repo, which is how
// the indexer decides what to reparse.
func (s *Store) CodeFileSHAs(repoID int64) (map[string]string, error) {
	rows, err := s.q.Query(`SELECT path, content_sha FROM code_files WHERE repo_id = ?`, repoID)
	if err != nil {
		return nil, fmt.Errorf("query code file hashes: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var path, sha string
		if err := rows.Scan(&path, &sha); err != nil {
			return nil, fmt.Errorf("scan code file hash: %w", err)
		}
		out[path] = sha
	}
	return out, rows.Err()
}

func (s *Store) CodeIndexedPaths(repoID int64) ([]string, error) {
	rows, err := s.q.Query(`SELECT path FROM code_files WHERE repo_id = ? ORDER BY path`, repoID)
	if err != nil {
		return nil, fmt.Errorf("query indexed paths: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0)
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan indexed path: %w", err)
		}
		out = append(out, path)
	}
	return out, rows.Err()
}

func (s *Store) Stats(repoID int64) (CodeIndexStats, error) {
	var stats CodeIndexStats
	row := s.q.QueryRow(`
SELECT
  (SELECT COUNT(*) FROM code_files WHERE repo_id = ?),
  (SELECT COUNT(*) FROM code_symbols WHERE repo_id = ?),
  (SELECT COUNT(*) FROM code_imports WHERE repo_id = ?),
  (SELECT COUNT(*) FROM code_refs WHERE repo_id = ?)
`, repoID, repoID, repoID, repoID)
	if err := row.Scan(&stats.Files, &stats.Symbols, &stats.Imports, &stats.Refs); err != nil {
		return CodeIndexStats{}, fmt.Errorf("read code index stats: %w", err)
	}
	return stats, nil
}

func (s *Store) SymbolsInFile(repoID int64, path string) ([]CodeSymbol, error) {
	return s.scanSymbols(`
SELECT path, name, kind, receiver, signature, doc, start_line, end_line, exported
FROM code_symbols
WHERE repo_id = ? AND path = ?
ORDER BY start_line, name
`, repoID, path)
}

// SymbolsNamed finds every declaration of a name across the repo. Two files
// declaring the same name is normal (an interface and its implementation), so
// this returns all of them rather than guessing.
func (s *Store) SymbolsNamed(repoID int64, name string) ([]CodeSymbol, error) {
	return s.scanSymbols(`
SELECT path, name, kind, receiver, signature, doc, start_line, end_line, exported
FROM code_symbols
WHERE repo_id = ? AND name = ?
ORDER BY path, start_line
`, repoID, name)
}

// SearchSymbols runs an FTS5 query over symbol names, receivers, signatures
// and doc comments, best match first.
func (s *Store) SearchSymbols(repoID int64, query string, limit int) ([]CodeSymbol, error) {
	if limit <= 0 {
		limit = 20
	}
	expression := ftsPrefixExpression(query)
	if expression == "" {
		return []CodeSymbol{}, nil
	}
	return s.scanSymbols(`
SELECT cs.path, cs.name, cs.kind, cs.receiver, cs.signature, cs.doc, cs.start_line, cs.end_line, cs.exported
FROM code_symbol_fts f
JOIN code_symbols cs ON cs.id = f.rowid
WHERE code_symbol_fts MATCH ? AND cs.repo_id = ?
ORDER BY bm25(code_symbol_fts, 10.0, 2.0, 1.0, 1.0)
LIMIT ?
`, expression, repoID, limit)
}

func (s *Store) scanSymbols(query string, args ...any) ([]CodeSymbol, error) {
	rows, err := s.q.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query symbols: %w", err)
	}
	defer rows.Close()

	out := make([]CodeSymbol, 0)
	for rows.Next() {
		var symbol CodeSymbol
		var exported int
		if err := rows.Scan(&symbol.Path, &symbol.Name, &symbol.Kind, &symbol.Receiver,
			&symbol.Signature, &symbol.Doc, &symbol.StartLine, &symbol.EndLine, &exported); err != nil {
			return nil, fmt.Errorf("scan symbol: %w", err)
		}
		symbol.Exported = exported == 1
		out = append(out, symbol)
	}
	return out, rows.Err()
}

func (s *Store) ImportsFrom(repoID int64, path string) ([]CodeImport, error) {
	return s.scanImports(`
SELECT from_path, raw_spec, to_path, kind, external
FROM code_imports
WHERE repo_id = ? AND from_path = ?
ORDER BY raw_spec
`, repoID, path)
}

// ImportsTo returns the edges pointing at a path: its dependents.
func (s *Store) ImportsTo(repoID int64, path string) ([]CodeImport, error) {
	return s.scanImports(`
SELECT from_path, raw_spec, to_path, kind, external
FROM code_imports
WHERE repo_id = ? AND to_path = ?
ORDER BY from_path
`, repoID, path)
}

func (s *Store) scanImports(query string, args ...any) ([]CodeImport, error) {
	rows, err := s.q.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query imports: %w", err)
	}
	defer rows.Close()

	out := make([]CodeImport, 0)
	for rows.Next() {
		var imp CodeImport
		var external int
		if err := rows.Scan(&imp.FromPath, &imp.RawSpec, &imp.ToPath, &imp.Kind, &external); err != nil {
			return nil, fmt.Errorf("scan import: %w", err)
		}
		imp.External = external == 1
		out = append(out, imp)
	}
	return out, rows.Err()
}

// RefNamesLower returns the lowercased identifiers a file references, which is
// the shape the stem-matching heuristic needs.
func (s *Store) RefNamesLower(repoID int64, path string) (map[string]struct{}, error) {
	rows, err := s.q.Query(`SELECT name_lower FROM code_refs WHERE repo_id = ? AND path = ?`, repoID, path)
	if err != nil {
		return nil, fmt.Errorf("query refs: %w", err)
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan ref: %w", err)
		}
		out[name] = struct{}{}
	}
	return out, rows.Err()
}

// PathsReferencingLower lists files that reference any of the given lowercased
// identifiers. Used to answer the reverse direction of the stem heuristic in
// one query instead of one per candidate file.
func (s *Store) PathsReferencingLower(repoID int64, names []string) (map[string][]string, error) {
	if len(names) == 0 {
		return map[string][]string{}, nil
	}

	placeholders := make([]string, 0, len(names))
	args := []any{repoID}
	for _, name := range names {
		placeholders = append(placeholders, "?")
		args = append(args, name)
	}

	rows, err := s.q.Query(`
SELECT name_lower, path
FROM code_refs
WHERE repo_id = ? AND name_lower IN (`+strings.Join(placeholders, ",")+`)
`, args...)
	if err != nil {
		return nil, fmt.Errorf("query paths referencing: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]string)
	for rows.Next() {
		var name, path string
		if err := rows.Scan(&name, &path); err != nil {
			return nil, fmt.Errorf("scan path referencing: %w", err)
		}
		out[name] = append(out[name], path)
	}
	return out, rows.Err()
}

// PathsReferencing lists files that reference an exact identifier, excluding
// the files that declare it. That exclusion is what makes the result read as
// "callers" rather than "mentions".
func (s *Store) PathsReferencing(repoID int64, name string) ([]string, error) {
	rows, err := s.q.Query(`
SELECT r.path
FROM code_refs r
WHERE r.repo_id = ? AND r.name = ?
  AND NOT EXISTS (
    SELECT 1 FROM code_symbols cs
    WHERE cs.repo_id = r.repo_id AND cs.path = r.path AND cs.name = r.name
  )
ORDER BY r.path
`, repoID, name)
	if err != nil {
		return nil, fmt.Errorf("query callers: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0)
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan caller: %w", err)
		}
		out = append(out, path)
	}
	return out, rows.Err()
}

// HasCodeIndex reports whether content indexing has run for this repo. Callers
// use it to decide between a SQL lookup and the legacy filesystem scan, so an
// index written by an older Pallium keeps working.
func (s *Store) HasCodeIndex(repoID int64) bool {
	var count int
	row := s.q.QueryRow(`SELECT COUNT(*) FROM code_files WHERE repo_id = ? LIMIT 1`, repoID)
	if err := row.Scan(&count); err != nil {
		if err == sql.ErrNoRows {
			return false
		}
		return false
	}
	return count > 0
}

// ftsPrefixExpression turns a user query into an FTS5 expression. Every term
// gets a prefix wildcard, because searching for "Explain" should find
// "ExplainReport", and terms are quoted so punctuation in an identifier does
// not read as FTS syntax.
func ftsPrefixExpression(query string) string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return !(r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	})
	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(field, `"`, `""`)+`"*`)
	}
	return strings.Join(terms, " OR ")
}
