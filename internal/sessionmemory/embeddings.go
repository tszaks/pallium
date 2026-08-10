package sessionmemory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func Embed(ctx context.Context, model string, limit, batchSize int) (int, error) {
	return EmbedSession(ctx, "", model, limit, batchSize)
}

func EmbedSession(ctx context.Context, sessionID, model string, limit, batchSize int) (int, error) {
	return EmbedSessionPath(ctx, "", sessionID, model, limit, batchSize, nil)
}

func EmbedSessionPath(ctx context.Context, dbPath, sessionID, model string, limit, batchSize int, progress func(completed, total int)) (int, error) {
	model = resolveEmbeddingModel(model)
	provider := embeddingProvider()
	if batchSize <= 0 {
		batchSize = 64
	}
	if limit <= 0 {
		limit = 1000000
	}
	store, err := Open(dbPath)
	if err != nil {
		return 0, err
	}
	defer store.Close()
	var rows *sql.Rows
	if sessionID != "" {
		resolvedID, err := store.resolveID(sessionID)
		if err != nil {
			return 0, err
		}
		rows, err = store.db.Query(`SELECT c.id,c.text,c.text_sha256 FROM codex_session_chunks c LEFT JOIN codex_session_embeddings e ON e.chunk_id=c.id AND e.provider=? AND e.model=? AND e.text_sha256=c.text_sha256 WHERE e.chunk_id IS NULL AND c.session_id=? ORDER BY c.session_id,c.chunk_index LIMIT ?`, provider, model, resolvedID, limit)
	} else {
		rows, err = store.db.Query(`SELECT c.id,c.text,c.text_sha256 FROM codex_session_chunks c LEFT JOIN codex_session_embeddings e ON e.chunk_id=c.id AND e.provider=? AND e.model=? AND e.text_sha256=c.text_sha256 WHERE e.chunk_id IS NULL ORDER BY c.session_id,c.chunk_index LIMIT ?`, provider, model, limit)
	}
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type chunk struct{ id, text, sha string }
	var chunks []chunk
	for rows.Next() {
		var c chunk
		if err := rows.Scan(&c.id, &c.text, &c.sha); err != nil {
			return 0, err
		}
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if progress != nil {
		progress(0, len(chunks))
	}
	total := 0
	for i := 0; i < len(chunks); i += batchSize {
		end := i + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		texts := make([]string, 0, end-i)
		for _, c := range chunks[i:end] {
			texts = append(texts, c.text)
		}
		vecs, err := embedTexts(ctx, model, texts)
		if err != nil {
			return total, err
		}
		for j, vec := range vecs {
			c := chunks[i+j]
			if _, err := store.db.Exec(`INSERT OR REPLACE INTO codex_session_embeddings(chunk_id,provider,model,dim,vector_blob,text_sha256,embedded_at) VALUES(?,?,?,?,?,?,?)`, c.id, provider, model, len(vec), packVector(vec), c.sha, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return total, err
			}
			total++
		}
		if progress != nil {
			progress(total, len(chunks))
		}
	}
	if sessionID == "" && total < limit {
		backlog, err := store.embeddingBacklog(model)
		if err != nil {
			return total, err
		}
		if backlog == 0 {
			if err := store.setEmbeddingCursor(model, time.Now()); err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

func Semantic(ctx context.Context, query, model string, limit int, sessionsOnly bool) ([]SemanticResult, error) {
	model = resolveEmbeddingModel(model)
	provider := embeddingProvider()
	if limit <= 0 {
		limit = 10
	}
	qvecs, err := embedTexts(ctx, model, []string{query})
	if err != nil {
		return nil, err
	}
	store, err := Open("")
	if err != nil {
		return nil, err
	}
	defer store.Close()
	rows, err := store.db.Query(`SELECT e.vector_blob,c.id,c.session_id,c.kind,c.text,s.title,s.cwd,s.updated_at FROM codex_session_embeddings e JOIN codex_session_chunks c ON c.id=e.chunk_id AND e.text_sha256=c.text_sha256 JOIN codex_sessions s ON s.id=c.session_id WHERE e.provider=? AND e.model=?`, provider, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var scored []SemanticResult
	for rows.Next() {
		var blob []byte
		var r SemanticResult
		var text string
		if err := rows.Scan(&blob, &r.ChunkID, &r.SessionID, &r.Kind, &text, &r.Title, &r.CWD, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Score = cosine(qvecs[0], unpackVector(blob))
		r.Snippet = short(text, 600)
		scored = append(scored, r)
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	seen := map[string]bool{}
	out := []SemanticResult{}
	for _, r := range scored {
		if sessionsOnly && seen[r.SessionID] {
			continue
		}
		seen[r.SessionID] = true
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// openAICompatibleEmbeddings calls any OpenAI-compatible /v1/embeddings endpoint. The API key is
// optional, so local runtimes (Ollama, LM Studio, llama.cpp) work without one.
func openAICompatibleEmbeddings(ctx context.Context, model string, texts []string) ([][]float64, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	s := resolveEmbeddingSettings()
	if s.configError != nil {
		return nil, s.configError
	}
	if s.credentialError != nil {
		return nil, s.credentialError
	}
	if s.apiKey == "" && strings.Contains(s.baseURL, "api.openai.com") {
		return nil, errors.New("OpenAI embeddings require OPENAI_API_KEY or PALLIUM_EMBED_API_KEY; set PALLIUM_EMBED_PROVIDER=ollama (or another local provider) to run without a key")
	}
	url := s.baseURL + "/embeddings"
	body, _ := json.Marshal(map[string]any{"model": model, "input": texts})
	var payload []byte
	var status string
	for attempt := 0; attempt < 10; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if s.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+s.apiKey)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		payload, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		status = resp.Status
		if resp.StatusCode < 300 {
			break
		}
		if summary, permanent := permanentEmbeddingFailure(payload); permanent {
			return nil, fmt.Errorf("%s embeddings failed: %s: %s", s.provider, resp.Status, summary)
		}
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return nil, fmt.Errorf("%s embeddings failed: %s: %s", s.provider, resp.Status, short(string(payload), 500))
		}
		wait := retryDelay(resp.Header.Get("Retry-After"), string(payload), attempt)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("%s embeddings failed: %s: %s", s.provider, status, short(string(payload), 500))
		case <-timer.C:
		}
	}
	if status == "" || !strings.HasPrefix(status, "2") {
		return nil, fmt.Errorf("%s embeddings failed: %s: %s", s.provider, status, short(string(payload), 500))
	}
	var decoded struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, err
	}
	out := make([][]float64, len(decoded.Data))
	for i, item := range decoded.Data {
		out[i] = item.Embedding
	}
	return out, nil
}

func permanentEmbeddingFailure(payload []byte) (string, bool) {
	var decoded struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &decoded) != nil {
		return "", false
	}
	permanent := decoded.Error.Type == "insufficient_quota" || decoded.Error.Code == "credit_balance_exhausted"
	if !permanent {
		return "", false
	}
	return short(strings.TrimSpace(decoded.Error.Code+": "+decoded.Error.Message), 500), true
}

func retryDelay(retryAfter, payload string, attempt int) time.Duration {
	if retryAfter != "" {
		if seconds, err := strconv.ParseFloat(strings.TrimSpace(retryAfter), 64); err == nil && seconds > 0 {
			return time.Duration(seconds*1000) * time.Millisecond
		}
	}
	re := regexp.MustCompile(`(?i)try again in ([0-9.]+)\s*(ms|s)`)
	matches := re.FindStringSubmatch(payload)
	if len(matches) == 3 {
		if n, err := strconv.ParseFloat(matches[1], 64); err == nil && n > 0 {
			if strings.EqualFold(matches[2], "ms") {
				return time.Duration(n) * time.Millisecond
			}
			return time.Duration(n*1000) * time.Millisecond
		}
	}
	delay := time.Duration(1<<min(attempt, 5)) * time.Second
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func packVector(vec []float64) []byte {
	buf := new(bytes.Buffer)
	for _, v := range vec {
		_ = binary.Write(buf, binary.LittleEndian, float32(v))
	}
	return buf.Bytes()
}

func unpackVector(blob []byte) []float64 {
	out := make([]float64, len(blob)/4)
	for i := range out {
		out[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:])))
	}
	return out
}

func cosine(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return -1
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return -1
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

type chunkRecord struct {
	ID, SessionID, Kind, Text, TextSHA256 string
	Index, TokenEstimate                  int
	Metadata                              map[string]any
}

func buildChunks(p ParsedSession) []chunkRecord {
	if p.Session.Status == "" {
		p.Session.Status = "seen"
	}
	return buildContinuityChunks(p, buildSessionCapsule(p))
}

func buildContinuityChunks(p ParsedSession, capsule SessionCapsule) []chunkRecord {
	parts := []string{
		"Title: " + short(p.Session.Title, 240),
		"CWD: " + short(p.Session.CWD, 400),
		"Repository: " + short(strings.TrimSpace(p.Session.GitOriginURL+" "+p.Session.GitBranch), 300),
		"Status: " + short(capsule.Status, 60),
		"Goal: " + short(capsule.Goal, 700),
		"Stopped at: " + short(capsule.StoppedAt, 700),
		"Next action: " + short(capsule.NextAction, 350),
		"Completed: " + compactContinuityItems(capsule.Completed, 3, 400),
		"Remaining: " + compactContinuityItems(capsule.Remaining, 3, 400),
		"Blockers: " + compactContinuityItems(capsule.Blockers, 3, 400),
		"Conversation evidence: " + continuityEvidenceText(p.Messages, 4, 6, 700),
		"Files: " + compactContinuityItems(p.Session.FilesTouched, 8, 800),
		"Commands: " + compactContinuityItems(p.Session.Commands, 4, 600),
	}
	text := truncate(strings.TrimSpace(strings.Join(nonEmptyContinuityParts(parts), "\n")), 6000)
	if text == "" {
		return nil
	}
	sha := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	return []chunkRecord{{
		ID:            fmt.Sprintf("%s:%04d", p.Session.ID, 0),
		SessionID:     p.Session.ID,
		Index:         0,
		Kind:          "continuity",
		Text:          text,
		TextSHA256:    sha,
		TokenEstimate: max(1, len(text)/4),
		Metadata:      map[string]any{"session_title": p.Session.Title, "cwd": p.Session.CWD, "coverage": capsule.Coverage.Mode},
	}}
}

func nonEmptyContinuityParts(parts []string) []string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		separator := strings.Index(part, ":")
		if separator >= 0 && strings.TrimSpace(part[separator+1:]) == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func compactContinuityItems(items []string, limit, maxChars int) string {
	if limit <= 0 || maxChars <= 0 {
		return ""
	}
	values := make([]string, 0, min(limit, len(items)))
	for _, item := range items {
		item = short(redact(item), maxChars)
		if item == "" {
			continue
		}
		values = append(values, item)
		if len(values) >= limit {
			break
		}
	}
	return truncate(strings.Join(values, " | "), maxChars)
}

func continuityEvidenceText(messages []Message, headLimit, tailLimit, maxChars int) string {
	conversation := make([]Message, 0, len(messages))
	for _, message := range messages {
		if (message.Role == "user" || message.Role == "assistant") && strings.TrimSpace(message.Text) != "" {
			conversation = append(conversation, message)
		}
	}
	selected := make([]Message, 0, headLimit+tailLimit)
	selected = append(selected, conversation[:min(headLimit, len(conversation))]...)
	tailStart := max(len(selected), len(conversation)-tailLimit)
	selected = append(selected, conversation[tailStart:]...)
	parts := make([]string, 0, len(selected))
	for _, message := range selected {
		parts = append(parts, fmt.Sprintf("[%s line %d] %s", message.Role, message.LineNo, short(redact(message.Text), 240)))
	}
	return truncate(strings.Join(parts, " | "), maxChars)
}

func chunkText(text string, maxChars, overlap int) []string {
	text = strings.TrimSpace(redact(text))
	if text == "" {
		return nil
	}
	if len(text) <= maxChars {
		return []string{text}
	}
	var chunks []string
	for start := 0; start < len(text); {
		end := min(len(text), start+maxChars)
		chunks = append(chunks, strings.TrimSpace(text[start:end]))
		if end >= len(text) {
			break
		}
		start = max(0, end-overlap)
	}
	return chunks
}
