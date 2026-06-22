package sessionmemory

import (
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func Search(query string, limit int) ([]SearchResult, error) {
	store, err := Open("")
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.search(query, limit)
}

func SearchHybrid(query string, limit int) ([]SearchResult, error) {
	return related(RelatedOptions{Query: query, Limit: limit})
}

func Related(opts RelatedOptions) ([]SearchResult, error) {
	return related(opts)
}

func related(opts RelatedOptions) ([]SearchResult, error) {
	if opts.Limit <= 0 {
		opts.Limit = 10
	}
	store, err := Open("")
	if err != nil {
		return nil, err
	}
	defer store.Close()
	sessions, err := store.listAll()
	if err != nil {
		return nil, err
	}
	results := make([]SearchResult, 0, len(sessions))
	for _, sess := range sessions {
		score, signals := scoreRelatedSession(sess, opts)
		if score <= 0 {
			continue
		}
		results = append(results, SearchResult{Session: sess, Score: score, Signals: signals})
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return first(results[i].UpdatedAt, results[i].CreatedAt) > first(results[j].UpdatedAt, results[j].CreatedAt)
	})
	if len(results) > opts.Limit {
		results = results[:opts.Limit]
	}
	return results, nil
}

func scoreRelatedSession(sess Session, opts RelatedOptions) (int, []string) {
	score := 0
	signals := make([]string, 0, 6)
	repoRoot := strings.TrimSpace(opts.RepoRoot)
	if repoRoot != "" {
		repoRoot = filepath.Clean(repoRoot)
		cwd := filepath.Clean(strings.TrimSpace(sess.CWD))
		switch {
		case cwd == repoRoot:
			score += 80
			signals = append(signals, "same-cwd")
		case cwd != "." && strings.HasPrefix(cwd, repoRoot+string(filepath.Separator)):
			score += 45
			signals = append(signals, "nested-cwd")
		}
	}
	if opts.GitOriginURL != "" && sess.GitOriginURL == opts.GitOriginURL {
		score += 50
		signals = append(signals, "same-origin")
	}
	for _, file := range normalizedRelatedFiles(opts.Files) {
		if sessionTouchesFile(sess, file) {
			score += 70
			signals = append(signals, "file-touch:"+file)
			continue
		}
		if sessionMentionsFile(sess, file) {
			score += 25
			signals = append(signals, "file-mention:"+file)
		}
	}
	if queryScore := scoreQueryTerms(sess, opts.Query); queryScore > 0 {
		score += queryScore
		signals = append(signals, "query-match")
	}
	if recency := recencyScore(sess); recency > 0 {
		score += recency
		signals = append(signals, "recent")
	}
	return score, uniqueStrings(signals, 0)
}

func normalizedRelatedFiles(files []string) []string {
	out := make([]string, 0, len(files))
	for _, file := range files {
		trimmed := strings.TrimSpace(file)
		if trimmed == "" {
			continue
		}
		out = append(out, filepath.ToSlash(filepath.Clean(trimmed)))
	}
	return uniqueStrings(out, 0)
}

func uniqueStrings(values []string, limit int) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func sessionTouchesFile(sess Session, file string) bool {
	for _, touched := range sess.FilesTouched {
		if relatedPathMatches(touched, file) {
			return true
		}
	}
	return false
}

func sessionMentionsFile(sess Session, file string) bool {
	needles := []string{file, filepath.Base(file)}
	for _, value := range append([]string{sess.Title, sess.FirstUserMessage, sess.LastAgentMessage, sess.CWD}, append(sess.Commands, sess.Errors...)...) {
		lower := strings.ToLower(value)
		for _, needle := range needles {
			if needle != "" && strings.Contains(lower, strings.ToLower(needle)) {
				return true
			}
		}
	}
	return false
}

func relatedPathMatches(value, file string) bool {
	value = filepath.ToSlash(filepath.Clean(strings.TrimSpace(value)))
	if value == file {
		return true
	}
	return strings.HasSuffix(value, "/"+file) || strings.HasSuffix(file, "/"+value)
}

func scoreQueryTerms(sess Session, query string) int {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return 0
	}
	haystack := strings.ToLower(strings.Join([]string{
		sess.Title,
		sess.FirstUserMessage,
		sess.LastAgentMessage,
		sess.CWD,
		strings.Join(sess.FilesTouched, " "),
		strings.Join(sess.Commands, " "),
		strings.Join(sess.Errors, " "),
	}, " "))
	score := 0
	for _, term := range terms {
		if len(term) < 2 {
			continue
		}
		if strings.Contains(haystack, term) {
			score += 8
		}
	}
	return score
}

func recencyScore(sess Session) int {
	raw := first(sess.UpdatedAt, sess.CreatedAt)
	if raw == "" {
		return 0
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.ReplaceAll(raw, "+00:00", "Z"))
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, raw)
	}
	if err != nil {
		return 0
	}
	age := time.Since(parsed)
	switch {
	case age <= 48*time.Hour:
		return 12
	case age <= 7*24*time.Hour:
		return 8
	case age <= 30*24*time.Hour:
		return 4
	default:
		return 0
	}
}

func (s *Store) search(query string, limit int) ([]SearchResult, error) {
	rows, err := s.db.Query(`SELECT cs.id,cs.machine,cs.title,cs.first_user_message,cs.last_agent_message,cs.cwd,cs.source,cs.model_provider,cs.model,cs.cli_version,cs.git_branch,cs.git_origin_url,cs.created_at,cs.updated_at,cs.tokens_used,cs.status,cs.rollout_path,cs.rollout_sha256,cs.files_touched_json,cs.commands_json,cs.tool_names_json,cs.errors_json, bm25(codex_session_fts) AS rank FROM codex_session_fts JOIN codex_sessions cs ON cs.id=codex_session_fts.session_id WHERE codex_session_fts MATCH ? ORDER BY rank LIMIT ?`, query, limit)
	if err != nil {
		quoted := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
		rows, err = s.db.Query(`SELECT cs.id,cs.machine,cs.title,cs.first_user_message,cs.last_agent_message,cs.cwd,cs.source,cs.model_provider,cs.model,cs.cli_version,cs.git_branch,cs.git_origin_url,cs.created_at,cs.updated_at,cs.tokens_used,cs.status,cs.rollout_path,cs.rollout_sha256,cs.files_touched_json,cs.commands_json,cs.tool_names_json,cs.errors_json, bm25(codex_session_fts) AS rank FROM codex_session_fts JOIN codex_sessions cs ON cs.id=codex_session_fts.session_id WHERE codex_session_fts MATCH ? ORDER BY rank LIMIT ?`, quoted, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		sess, rank, err := scanSessionRank(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, SearchResult{Session: sess, Rank: rank})
	}
	return out, rows.Err()
}
