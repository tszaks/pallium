package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// KnowledgeDoc is one page of the repo's knowledge base.
//
// Verified is the field that matters. A doc is stored either way, but only a
// doc whose every cited path and symbol resolves against the content index is
// marked verified, and DroppedClaims records what was removed on the way. A
// knowledge base that cannot say which of its claims failed to check out is
// just prose with a database behind it.
type KnowledgeDoc struct {
	Slug          string    `json:"slug"`
	Kind          string    `json:"kind"`
	Title         string    `json:"title"`
	Summary       string    `json:"summary"`
	Body          string    `json:"body_md"`
	CitedPaths    []string  `json:"cited_paths"`
	CitedSymbols  []string  `json:"cited_symbols"`
	DroppedClaims []string  `json:"dropped_claims"`
	SourceCommit  string    `json:"source_commit"`
	Fingerprint   string    `json:"fingerprint"`
	Generator     string    `json:"generator"`
	Verified      bool      `json:"verified"`
	GeneratedAt   time.Time `json:"generated_at"`
}

func (s *Store) UpsertKnowledgeDoc(repoID int64, doc KnowledgeDoc) error {
	citedPaths, err := json.Marshal(nonNil(doc.CitedPaths))
	if err != nil {
		return fmt.Errorf("encode cited paths: %w", err)
	}
	citedSymbols, err := json.Marshal(nonNil(doc.CitedSymbols))
	if err != nil {
		return fmt.Errorf("encode cited symbols: %w", err)
	}
	dropped, err := json.Marshal(nonNil(doc.DroppedClaims))
	if err != nil {
		return fmt.Errorf("encode dropped claims: %w", err)
	}

	verified := 0
	if doc.Verified {
		verified = 1
	}

	// Delete then insert rather than ON CONFLICT UPDATE: the FTS mirror is
	// kept by insert and delete triggers, and an UPDATE would leave the old
	// text searchable.
	if err := s.DeleteKnowledgeDoc(repoID, doc.Slug); err != nil {
		return err
	}

	if _, err := s.q.Exec(`
INSERT INTO knowledge_docs
  (repo_id, slug, kind, title, summary, body_md, cited_paths_json, cited_symbols_json,
   dropped_claims_json, source_commit, fingerprint, generator, verified, generated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, repoID, doc.Slug, doc.Kind, doc.Title, doc.Summary, doc.Body, string(citedPaths),
		string(citedSymbols), string(dropped), doc.SourceCommit, doc.Fingerprint,
		doc.Generator, verified, doc.GeneratedAt.UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("insert knowledge doc %s: %w", doc.Slug, err)
	}
	return nil
}

func (s *Store) DeleteKnowledgeDoc(repoID int64, slug string) error {
	if _, err := s.q.Exec(`DELETE FROM knowledge_docs WHERE repo_id = ? AND slug = ?`, repoID, slug); err != nil {
		return fmt.Errorf("delete knowledge doc %s: %w", slug, err)
	}
	return nil
}

func (s *Store) KnowledgeDoc(repoID int64, slug string) (KnowledgeDoc, bool, error) {
	docs, err := s.scanKnowledgeDocs(`
SELECT slug, kind, title, summary, body_md, cited_paths_json, cited_symbols_json,
       dropped_claims_json, source_commit, fingerprint, generator, verified, generated_at
FROM knowledge_docs
WHERE repo_id = ? AND slug = ?
`, repoID, slug)
	if err != nil {
		return KnowledgeDoc{}, false, err
	}
	if len(docs) == 0 {
		return KnowledgeDoc{}, false, nil
	}
	return docs[0], true, nil
}

func (s *Store) KnowledgeDocs(repoID int64) ([]KnowledgeDoc, error) {
	return s.scanKnowledgeDocs(`
SELECT slug, kind, title, summary, body_md, cited_paths_json, cited_symbols_json,
       dropped_claims_json, source_commit, fingerprint, generator, verified, generated_at
FROM knowledge_docs
WHERE repo_id = ?
ORDER BY kind, slug
`, repoID)
}

// SearchKnowledge is the counterpart to Greptile's substring search, except it
// is BM25-ranked over title, summary and body. Prefix terms so a search for
// "migrat" finds "migrations".
func (s *Store) SearchKnowledge(repoID int64, query string, limit int) ([]KnowledgeDoc, error) {
	if limit <= 0 {
		limit = 10
	}
	expression := ftsPrefixExpression(query)
	if expression == "" {
		return []KnowledgeDoc{}, nil
	}
	return s.scanKnowledgeDocs(`
SELECT d.slug, d.kind, d.title, d.summary, d.body_md, d.cited_paths_json, d.cited_symbols_json,
       d.dropped_claims_json, d.source_commit, d.fingerprint, d.generator, d.verified, d.generated_at
FROM knowledge_fts f
JOIN knowledge_docs d ON d.id = f.rowid
WHERE knowledge_fts MATCH ? AND d.repo_id = ?
ORDER BY bm25(knowledge_fts, 8.0, 4.0, 1.0)
LIMIT ?
`, expression, repoID, limit)
}

func (s *Store) scanKnowledgeDocs(query string, args ...any) ([]KnowledgeDoc, error) {
	rows, err := s.q.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query knowledge docs: %w", err)
	}
	defer rows.Close()

	out := make([]KnowledgeDoc, 0)
	for rows.Next() {
		var doc KnowledgeDoc
		var citedPaths, citedSymbols, dropped, generatedAt string
		var verified int
		if err := rows.Scan(&doc.Slug, &doc.Kind, &doc.Title, &doc.Summary, &doc.Body,
			&citedPaths, &citedSymbols, &dropped, &doc.SourceCommit, &doc.Fingerprint,
			&doc.Generator, &verified, &generatedAt); err != nil {
			return nil, fmt.Errorf("scan knowledge doc: %w", err)
		}
		doc.Verified = verified == 1
		_ = json.Unmarshal([]byte(citedPaths), &doc.CitedPaths)
		_ = json.Unmarshal([]byte(citedSymbols), &doc.CitedSymbols)
		_ = json.Unmarshal([]byte(dropped), &doc.DroppedClaims)
		if parsed, err := time.Parse(time.RFC3339, generatedAt); err == nil {
			doc.GeneratedAt = parsed
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}

// ResolvedImports returns every import edge that lands inside the repo. Used
// to derive module-level dependencies without one query per file.
func (s *Store) ResolvedImports(repoID int64) ([]CodeImport, error) {
	return s.scanImports(`
SELECT from_path, raw_spec, to_path, kind, external
FROM code_imports
WHERE repo_id = ? AND to_path != ''
`, repoID)
}

// RefCounts is how many distinct files reference each identifier. It is the
// ranking signal for "which symbols in this module matter", which beats
// alphabetical order and needs no model to compute.
func (s *Store) RefCounts(repoID int64) (map[string]int, error) {
	rows, err := s.q.Query(`
SELECT name, COUNT(DISTINCT path)
FROM code_refs
WHERE repo_id = ?
GROUP BY name
`, repoID)
	if err != nil {
		return nil, fmt.Errorf("query ref counts: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			return nil, fmt.Errorf("scan ref count: %w", err)
		}
		out[name] = count
	}
	return out, rows.Err()
}

// SymbolsUnderPrefix returns the symbols declared in a directory subtree. A
// LIKE on the path prefix avoids a parameter list the size of the module.
func (s *Store) SymbolsUnderPrefix(repoID int64, prefix string) ([]CodeSymbol, error) {
	pattern, exact := prefixPattern(prefix)
	return s.scanSymbols(`
SELECT path, name, kind, receiver, signature, doc, start_line, end_line, exported
FROM code_symbols
WHERE repo_id = ? AND (path LIKE ? ESCAPE '\' OR path = ?)
ORDER BY path, start_line
`, repoID, pattern, exact)
}

// RecentSubjectsUnderPrefix returns the newest commit subjects touching a
// directory subtree: the module's own change history, in one query.
func (s *Store) RecentSubjectsUnderPrefix(repoID int64, prefix string, limit int) ([]CommitRecord, error) {
	if limit <= 0 {
		limit = 5
	}
	pattern, exact := prefixPattern(prefix)
	rows, err := s.q.Query(`
SELECT c.sha, c.subject, c.committed_at
FROM file_commits fc
JOIN commits c ON c.repo_id = fc.repo_id AND c.sha = fc.commit_sha
WHERE fc.repo_id = ? AND (fc.file_path LIKE ? ESCAPE '\' OR fc.file_path = ?)
GROUP BY c.sha
ORDER BY c.committed_at DESC
LIMIT ?
`, repoID, pattern, exact, limit)
	if err != nil {
		return nil, fmt.Errorf("query module commits: %w", err)
	}
	defer rows.Close()

	out := make([]CommitRecord, 0)
	for rows.Next() {
		var record CommitRecord
		var committedAt string
		if err := rows.Scan(&record.SHA, &record.Subject, &committedAt); err != nil {
			return nil, fmt.Errorf("scan module commit: %w", err)
		}
		if parsed, err := time.Parse(time.RFC3339, committedAt); err == nil {
			record.CommittedAt = parsed
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// CommitsMatchingSubjects finds commits whose subject contains any of the
// given fragments. This is the incident list's whole mechanism: a revert or a
// hotfix announces itself in its own subject line.
func (s *Store) CommitsMatchingSubjects(repoID int64, fragments []string, limit int) ([]CommitRecord, error) {
	if len(fragments) == 0 {
		return []CommitRecord{}, nil
	}
	if limit <= 0 {
		limit = 50
	}

	clauses := make([]string, 0, len(fragments))
	args := []any{repoID}
	for _, fragment := range fragments {
		clauses = append(clauses, "LOWER(subject) LIKE ?")
		args = append(args, "%"+strings.ToLower(fragment)+"%")
	}
	args = append(args, limit)

	rows, err := s.q.Query(`
SELECT sha, author_name, committed_at, subject, body
FROM commits
WHERE repo_id = ? AND (`+strings.Join(clauses, " OR ")+`)
ORDER BY committed_at DESC
LIMIT ?
`, args...)
	if err != nil {
		return nil, fmt.Errorf("query incident commits: %w", err)
	}
	defer rows.Close()

	out := make([]CommitRecord, 0)
	for rows.Next() {
		var record CommitRecord
		var committedAt string
		if err := rows.Scan(&record.SHA, &record.AuthorName, &committedAt, &record.Subject, &record.Body); err != nil {
			return nil, fmt.Errorf("scan incident commit: %w", err)
		}
		if parsed, err := time.Parse(time.RFC3339, committedAt); err == nil {
			record.CommittedAt = parsed
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// FilesForCommit lists what a commit touched, so an incident entry can name
// the files rather than just the subject line.
func (s *Store) FilesForCommit(repoID int64, sha string) ([]string, error) {
	rows, err := s.q.Query(`
SELECT file_path FROM file_commits WHERE repo_id = ? AND commit_sha = ? ORDER BY file_path
`, repoID, sha)
	if err != nil {
		return nil, fmt.Errorf("query commit files: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0)
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan commit file: %w", err)
		}
		out = append(out, path)
	}
	return out, rows.Err()
}

// prefixPattern builds a LIKE pattern for a directory subtree. The repo root
// is spelled "." by filepath.Dir, and matching everything is the right answer
// there. Escapes the LIKE wildcards so a directory with an underscore in its
// name does not silently match its neighbours.
func prefixPattern(prefix string) (string, string) {
	if prefix == "" || prefix == "." {
		return "%", ""
	}
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	return escaped + "/%", prefix
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// ExternalImports returns the import edges that leave the repo: third-party
// packages and stdlib. Which libraries a module reaches for is a fact about
// what it does, and it costs one query.
func (s *Store) ExternalImports(repoID int64) ([]CodeImport, error) {
	return s.scanImports(`
SELECT from_path, raw_spec, to_path, kind, external
FROM code_imports
WHERE repo_id = ? AND external = 1
`, repoID)
}

// AllSymbolNames is the verification vocabulary: every name the index knows.
// A synthesized claim citing a symbol outside this set is not stored.
func (s *Store) AllSymbolNames(repoID int64) ([]string, error) {
	rows, err := s.q.Query(`SELECT DISTINCT name FROM code_symbols WHERE repo_id = ?`, repoID)
	if err != nil {
		return nil, fmt.Errorf("query symbol names: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan symbol name: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
