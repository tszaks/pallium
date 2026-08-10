package sessionmemory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func Search(query string, limit int) ([]SearchResult, error) {
	return SearchWithOptions(context.Background(), SessionSearchOptions{Query: query, Limit: limit})
}

func SearchHybrid(query string, limit int) ([]SearchResult, error) {
	return SearchWithOptions(context.Background(), SessionSearchOptions{Query: query, Limit: limit, Hybrid: true})
}

func SearchWithOptions(ctx context.Context, opts SessionSearchOptions) ([]SearchResult, error) {
	if strings.TrimSpace(opts.Query) == "" {
		return nil, fmt.Errorf("session search query is required")
	}
	if opts.Limit <= 0 {
		opts.Limit = 10
	}
	store, err := Open(opts.DBPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	candidateLimit := min(250, opts.Limit)
	if opts.Hybrid {
		if opts.Limit >= 32 {
			candidateLimit = 250
		} else {
			candidateLimit = max(50, opts.Limit*8)
		}
	}
	lexical, err := store.lexicalSearch(opts, candidateLimit)
	if err != nil {
		return nil, err
	}
	if !opts.Hybrid {
		if len(lexical) > opts.Limit {
			lexical = lexical[:opts.Limit]
		}
		return lexical, nil
	}
	return store.hybridSearch(ctx, opts, lexical)
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
		path := sess.RolloutPath
		results = append(results, SearchResult{
			Session:  compactSearchSession(sess),
			Score:    score,
			Signals:  signals,
			Citation: SearchCitation{SessionID: sess.ID, Source: sess.Source, UpdatedAt: first(sess.UpdatedAt, sess.CreatedAt), RolloutPath: path},
			Coverage: SessionCoverage{Mode: "unknown"},
			Warnings: []string{},
		})
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
	return s.lexicalSearch(SessionSearchOptions{Query: query, Limit: limit}, limit)
}

type lexicalRow struct {
	sessionID string
	rank      float64
	snippet   string
	capsule   string
}

var searchTermPattern = regexp.MustCompile(`[\pL\pN_./-]+`)

func (s *Store) lexicalSearch(opts SessionSearchOptions, candidateLimit int) ([]SearchResult, error) {
	terms := searchTermPattern.FindAllString(strings.ToLower(opts.Query), -1)
	terms = uniqueStrings(terms, 24)
	if len(terms) == 0 {
		return []SearchResult{}, nil
	}
	if candidateLimit <= 0 {
		candidateLimit = max(10, opts.Limit)
	}
	candidateLimit = min(250, candidateLimit)
	searchExpression := func(expression string) ([]SearchResult, error) {
		out := make([]SearchResult, 0, candidateLimit)
		offset := 0
		pageSize := max(50, candidateLimit)
		for len(out) < candidateLimit {
			rows, err := s.lexicalRows(opts, expression, pageSize, offset)
			if err != nil {
				return nil, err
			}
			if len(rows) == 0 {
				break
			}
			offset += len(rows)
			for _, row := range rows {
				sess, err := s.loadSession(row.sessionID)
				if err != nil {
					return nil, err
				}
				if !matchesFileFilters(sess, opts.Files) {
					continue
				}
				path := sess.RolloutPath
				result := SearchResult{
					Session:      compactSearchSession(sess),
					Rank:         row.rank,
					LexicalScore: 1 / (1 + math.Abs(row.rank)),
					Snippet:      short(row.snippet, 700),
					Signals:      []string{"lexical"},
					Citation:     SearchCitation{SessionID: sess.ID, Source: sess.Source, UpdatedAt: first(sess.UpdatedAt, sess.CreatedAt), RolloutPath: path},
					Coverage:     SessionCoverage{Mode: "unknown"},
					Warnings:     []string{},
				}
				applyCapsuleToSearchResult(&result, row.capsule)
				out = append(out, result)
				if len(out) >= candidateLimit {
					break
				}
			}
			if len(rows) < pageSize {
				break
			}
		}
		return out, nil
	}

	out, err := searchExpression(quotedFTSTerms(terms, " AND "))
	if err != nil {
		return nil, err
	}
	if len(out) == 0 && len(terms) > 1 {
		return searchExpression(quotedFTSTerms(terms, " OR "))
	}
	return out, nil
}

func (s *Store) lexicalRows(opts SessionSearchOptions, expression string, limit, offset int) ([]lexicalRow, error) {
	filterSQL, filterArgs := sessionFilterSQL(opts)
	query := `SELECT codex_session_fts.session_id,
		bm25(codex_session_fts,0.0,8.0,4.0,3.0,3.0,3.0,2.0,1.0) AS rank,
		snippet(codex_session_fts,7,'[',']',' ... ',32),
		COALESCE(c.capsule_json,'')
	FROM codex_session_fts
	JOIN codex_sessions s ON s.id=codex_session_fts.session_id
	LEFT JOIN codex_session_capsules c ON c.session_id=s.id
	WHERE codex_session_fts MATCH ?` + filterSQL + ` ORDER BY rank,codex_session_fts.session_id LIMIT ? OFFSET ?`
	args := []any{expression}
	args = append(args, filterArgs...)
	args = append(args, limit, offset)
	dbRows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer dbRows.Close()
	rows := []lexicalRow{}
	for dbRows.Next() {
		var row lexicalRow
		if err := dbRows.Scan(&row.sessionID, &row.rank, &row.snippet, &row.capsule); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, dbRows.Err()
}

func quotedFTSTerms(terms []string, separator string) string {
	quoted := make([]string, 0, len(terms))
	for _, term := range terms {
		quoted = append(quoted, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, separator)
}

func sessionFilterSQL(opts SessionSearchOptions) (string, []any) {
	var clauses []string
	var args []any
	if source := strings.TrimSpace(opts.Source); source != "" {
		clauses = append(clauses, `LOWER(s.source)=LOWER(?)`)
		args = append(args, source)
	}
	if cwd := strings.TrimRight(strings.TrimSpace(opts.CWD), string(filepath.Separator)); cwd != "" {
		clauses = append(clauses, `(s.cwd=? OR s.cwd LIKE ?)`)
		args = append(args, cwd, cwd+string(filepath.Separator)+"%")
	}
	repoRoot := strings.TrimRight(strings.TrimSpace(opts.RepoRoot), string(filepath.Separator))
	origin := strings.TrimSpace(opts.GitOriginURL)
	if repoRoot != "" && origin != "" {
		clauses = append(clauses, `(s.cwd=? OR s.cwd LIKE ? OR s.git_origin_url=?)`)
		args = append(args, repoRoot, repoRoot+string(filepath.Separator)+"%", origin)
	} else if repoRoot != "" {
		clauses = append(clauses, `(s.cwd=? OR s.cwd LIKE ?)`)
		args = append(args, repoRoot, repoRoot+string(filepath.Separator)+"%")
	} else if origin != "" {
		clauses = append(clauses, `s.git_origin_url=?`)
		args = append(args, origin)
	}
	if !opts.After.IsZero() {
		clauses = append(clauses, `COALESCE(NULLIF(s.updated_at,''),NULLIF(s.created_at,''),s.indexed_at)>=?`)
		args = append(args, opts.After.UTC().Format(time.RFC3339Nano))
	}
	if !opts.Before.IsZero() {
		clauses = append(clauses, `COALESCE(NULLIF(s.updated_at,''),NULLIF(s.created_at,''),s.indexed_at)<?`)
		args = append(args, opts.Before.UTC().Format(time.RFC3339Nano))
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(clauses, " AND "), args
}

func (s *Store) loadSession(id string) (Session, error) {
	row := s.db.QueryRow(`SELECT id,machine,title,first_user_message,last_agent_message,cwd,source,model_provider,model,cli_version,git_branch,git_origin_url,created_at,updated_at,tokens_used,status,rollout_path,rollout_sha256,files_touched_json,commands_json,tool_names_json,errors_json FROM codex_sessions WHERE id=?`, id)
	return scanSession(row)
}

func matchesFileFilters(sess Session, files []string) bool {
	files = normalizedRelatedFiles(files)
	if len(files) == 0 {
		return true
	}
	for _, file := range files {
		if sessionTouchesFile(sess, file) || sessionMentionsFile(sess, file) {
			return true
		}
	}
	return false
}

func compactSearchSession(sess Session) Session {
	sess.FirstUserMessage = ""
	sess.LastAgentMessage = ""
	sess.Commands = compactStringSlice(sess.Commands, 8, 500)
	sess.ToolNames = compactStringSlice(sess.ToolNames, 12, 120)
	sess.Errors = compactStringSlice(sess.Errors, 5, 300)
	sess.FilesTouched = append([]string{}, limitStringSlice(sess.FilesTouched, 12)...)
	sess.RolloutPath = ""
	sess.RolloutSHA256 = ""
	return sess
}

func compactStringSlice(values []string, limit, maxChars int) []string {
	values = limitStringSlice(values, limit)
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, short(value, maxChars))
	}
	return out
}

func limitStringSlice(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	return values[:limit]
}

func applyCapsuleToSearchResult(result *SearchResult, raw string) {
	if raw == "" {
		result.Warnings = append(result.Warnings, "Continuity capsule is missing; run a forced session sync.")
		return
	}
	capsule, err := decodeSessionCapsule(raw)
	if err != nil {
		result.Warnings = append(result.Warnings, "Continuity capsule could not be decoded.")
		return
	}
	result.Coverage = capsule.Coverage
	if capsule.Coverage.Warning != "" {
		result.Warnings = append(result.Warnings, capsule.Coverage.Warning)
	}
	if len(capsule.Evidence) > 0 {
		result.Citation.LineNo = capsule.Evidence[len(capsule.Evidence)-1].LineNo
	}
}

func (s *Store) hybridSearch(ctx context.Context, opts SessionSearchOptions, lexical []SearchResult) ([]SearchResult, error) {
	model := resolveEmbeddingModel(opts.Model)
	provider := embeddingProvider()
	var currentEmbeddings int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM codex_session_embeddings e JOIN codex_session_chunks c ON c.id=e.chunk_id AND c.text_sha256=e.text_sha256 WHERE e.provider=? AND e.model=?`, provider, model).Scan(&currentEmbeddings); err != nil {
		return nil, err
	}
	warning := ""
	semantic := []SearchResult{}
	if currentEmbeddings == 0 {
		warning = fmt.Sprintf("Semantic retrieval unavailable: no current %s/%s embeddings; lexical results were returned.", provider, model)
	} else {
		semanticContext, cancel := context.WithTimeout(ctx, 8*time.Second)
		var err error
		semantic, err = s.semanticSearch(semanticContext, opts, min(250, max(50, opts.Limit*8)))
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			warning = "Semantic retrieval failed or timed out; lexical results were returned: " + short(err.Error(), 240)
			semantic = []SearchResult{}
		}
	}
	if opts.MinimumSemanticScore > 0 {
		credible := semantic[:0]
		for _, result := range semantic {
			if result.SemanticScore < opts.MinimumSemanticScore {
				break
			}
			credible = append(credible, result)
		}
		semantic = credible
	}
	type fusedResult struct {
		result SearchResult
		score  float64
	}
	fused := map[string]fusedResult{}
	for index, result := range lexical {
		entry := fused[result.ID]
		entry.result = result
		entry.score += 1 / float64(60+index+1)
		fused[result.ID] = entry
	}
	for index, result := range semantic {
		entry, exists := fused[result.ID]
		if !exists {
			entry.result = result
		} else {
			entry.result.SemanticScore = result.SemanticScore
			entry.result.Signals = uniqueStrings(append(entry.result.Signals, "semantic"), 0)
			if entry.result.Snippet == "" {
				entry.result.Snippet = result.Snippet
			}
		}
		entry.score += 1 / float64(60+index+1)
		fused[result.ID] = entry
	}
	out := make([]SearchResult, 0, len(fused))
	for _, entry := range fused {
		entry.result.HybridScore = entry.score
		if warning != "" {
			entry.result.Warnings = append(entry.result.Warnings, warning)
		}
		out = append(out, entry.result)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].HybridScore != out[j].HybridScore {
			return out[i].HybridScore > out[j].HybridScore
		}
		return out[i].Citation.UpdatedAt > out[j].Citation.UpdatedAt
	})
	if len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

func (s *Store) semanticSearch(ctx context.Context, opts SessionSearchOptions, candidateLimit int) ([]SearchResult, error) {
	model := resolveEmbeddingModel(opts.Model)
	provider := embeddingProvider()
	queryVectors, err := embedTexts(ctx, model, []string{opts.Query})
	if err != nil {
		return nil, err
	}
	if len(queryVectors) != 1 {
		return nil, fmt.Errorf("embedding provider returned %d query vectors, want 1", len(queryVectors))
	}
	filterSQL, filterArgs := sessionFilterSQL(opts)
	query := `SELECT e.vector_blob,c.id,c.kind,c.text,s.id,s.machine,s.title,s.cwd,s.source,s.git_origin_url,s.created_at,s.updated_at,s.status,s.rollout_path,s.files_touched_json,COALESCE(cap.capsule_json,'')
	FROM codex_session_embeddings e
	JOIN codex_session_chunks c ON c.id=e.chunk_id AND c.text_sha256=e.text_sha256
	JOIN codex_sessions s ON s.id=c.session_id
	LEFT JOIN codex_session_capsules cap ON cap.session_id=s.id
	WHERE e.provider=? AND e.model=?` + filterSQL
	args := []any{provider, model}
	args = append(args, filterArgs...)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	best := map[string]SearchResult{}
	for rows.Next() {
		var blob []byte
		var chunkID, kind, text, filesJSON, capsuleJSON string
		var sess Session
		if err := rows.Scan(&blob, &chunkID, &kind, &text, &sess.ID, &sess.Machine, &sess.Title, &sess.CWD, &sess.Source, &sess.GitOriginURL, &sess.CreatedAt, &sess.UpdatedAt, &sess.Status, &sess.RolloutPath, &filesJSON, &capsuleJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(filesJSON), &sess.FilesTouched)
		if !matchesFileFilters(sess, opts.Files) {
			continue
		}
		score := cosine(queryVectors[0], unpackVector(blob))
		if existing, ok := best[sess.ID]; ok && existing.SemanticScore >= score {
			continue
		}
		path := sess.RolloutPath
		result := SearchResult{
			Session:       compactSearchSession(sess),
			SemanticScore: score,
			Snippet:       short(text, 700),
			Signals:       []string{"semantic"},
			Citation:      SearchCitation{SessionID: sess.ID, Source: sess.Source, UpdatedAt: first(sess.UpdatedAt, sess.CreatedAt), RolloutPath: path},
			Coverage:      SessionCoverage{Mode: "unknown"},
			Warnings:      []string{},
		}
		applyCapsuleToSearchResult(&result, capsuleJSON)
		best[sess.ID] = result
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0, len(best))
	for _, result := range best {
		out = append(out, result)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SemanticScore > out[j].SemanticScore })
	if len(out) > candidateLimit {
		out = out[:candidateLimit]
	}
	return out, nil
}
