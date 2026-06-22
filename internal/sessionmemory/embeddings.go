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
	"os"
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
	if model == "" {
		model = DefaultEmbeddingModel
	}
	if batchSize <= 0 {
		batchSize = 64
	}
	if limit <= 0 {
		limit = 1000000
	}
	store, err := Open("")
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
		rows, err = store.db.Query(`SELECT c.id,c.text,c.text_sha256 FROM codex_session_chunks c LEFT JOIN codex_session_embeddings e ON e.chunk_id=c.id AND e.provider='openai' AND e.model=? AND e.text_sha256=c.text_sha256 WHERE e.chunk_id IS NULL AND c.session_id=? ORDER BY c.session_id,c.chunk_index LIMIT ?`, model, resolvedID, limit)
	} else {
		rows, err = store.db.Query(`SELECT c.id,c.text,c.text_sha256 FROM codex_session_chunks c LEFT JOIN codex_session_embeddings e ON e.chunk_id=c.id AND e.provider='openai' AND e.model=? AND e.text_sha256=c.text_sha256 WHERE e.chunk_id IS NULL ORDER BY c.session_id,c.chunk_index LIMIT ?`, model, limit)
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
			if _, err := store.db.Exec(`INSERT OR REPLACE INTO codex_session_embeddings(chunk_id,provider,model,dim,vector_blob,text_sha256,embedded_at) VALUES(?,?,?,?,?,?,?)`, c.id, "openai", model, len(vec), packVector(vec), c.sha, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return total, err
			}
			total++
		}
	}
	return total, nil
}

func Semantic(ctx context.Context, query, model string, limit int, sessionsOnly bool) ([]SemanticResult, error) {
	if model == "" {
		model = DefaultEmbeddingModel
	}
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
	rows, err := store.db.Query(`SELECT e.vector_blob,c.id,c.session_id,c.kind,c.text,s.title,s.cwd,s.updated_at FROM codex_session_embeddings e JOIN codex_session_chunks c ON c.id=e.chunk_id JOIN codex_sessions s ON s.id=c.session_id WHERE e.provider='openai' AND e.model=?`, model)
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

func openAIEmbeddings(ctx context.Context, model string, texts []string) ([][]float64, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		key = os.Getenv("OPENAI_ADMIN_API_KEY")
	}
	if key == "" {
		return nil, errors.New("OPENAI_API_KEY is required for embeddings")
	}
	body, _ := json.Marshal(map[string]any{"model": model, "input": texts})
	var payload []byte
	var status string
	for attempt := 0; attempt < 10; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
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
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return nil, fmt.Errorf("openai embeddings failed: %s: %s", resp.Status, short(string(payload), 500))
		}
		wait := retryDelay(resp.Header.Get("Retry-After"), string(payload), attempt)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if status == "" || !strings.HasPrefix(status, "2") {
		return nil, fmt.Errorf("openai embeddings failed: %s: %s", status, short(string(payload), 500))
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
	overview := strings.Join([]string{"Title: " + p.Session.Title, "CWD: " + p.Session.CWD, "Git: " + p.Session.GitOriginURL + " " + p.Session.GitBranch, "First ask: " + p.Session.FirstUserMessage, "Last agent message: " + p.Session.LastAgentMessage, "Files touched:\n" + strings.Join(p.Session.FilesTouched, "\n"), "Commands:\n" + strings.Join(p.Session.Commands, "\n"), "Errors:\n" + strings.Join(p.Session.Errors, "\n")}, "\n")
	var transcriptParts []string
	for _, m := range p.Messages {
		if m.Text != "" {
			transcriptParts = append(transcriptParts, fmt.Sprintf("[%s/%s line %d]\n%s", m.Role, m.Kind, m.LineNo, m.Text))
		}
	}
	var out []chunkRecord
	idx := 0
	for _, piece := range []struct{ kind, text string }{{"overview", overview}, {"transcript", strings.Join(transcriptParts, "\n\n")}} {
		for _, text := range chunkText(piece.text, 6000, 600) {
			sha := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
			out = append(out, chunkRecord{ID: fmt.Sprintf("%s:%04d", p.Session.ID, idx), SessionID: p.Session.ID, Index: idx, Kind: piece.kind, Text: text, TextSHA256: sha, TokenEstimate: max(1, len(text)/4), Metadata: map[string]any{"session_title": p.Session.Title, "cwd": p.Session.CWD}})
			idx++
		}
	}
	return out
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
