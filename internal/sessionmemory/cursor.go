package sessionmemory

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Cursor session indexing. Both the Cursor IDE agent and the cursor-agent
// CLI write one transcript per chat:
//
//	<cursor-root>/projects/<slug>/agent-transcripts/<id>/<id>.jsonl
//	<cursor-root>/projects/<slug>/agent-transcripts/<id>/subagents/*.jsonl
//
// Lines are Claude-like {role, message:{content:[text|tool_use]}} plus a bare
// {type:"turn_ended", status} marker per turn. There are no tool_result
// lines and no per-line timestamps; the only clock inside a transcript is the
// human-readable <timestamp> tag on each user message. Assistant reasoning is
// stored as "[REDACTED]" and is not indexed. The slug is the workspace path
// without its leading "/" and with "/" turned into "-" (lossy; resolved
// against the filesystem, see cursorSlugToDir).
//
// A chat is several files, so (like devin) the record uses a synthetic
// rollout_path, "cursor://<slug>/<id>", as its stable identity, and
// rollout_sha256 holds a fingerprint of every file's name, size and mtime
// rather than a content hash. Subagent transcripts are folded into the parent
// chat: their assistant/tool lines are indexed, their delegated-prompt user
// lines are not (they would pollute first_user_message).
const (
	cursorRootRelative     = ".cursor"
	cursorRolloutPrefix    = "cursor://"
	cursorIndexQuietPeriod = activeRolloutQuietPeriod
	cursorMaxLineBytes     = 8 * 1024 * 1024
)

var (
	cursorUserQueryPattern = regexp.MustCompile(`(?s)<user_query>\s*(.*?)\s*</user_query>`)
	cursorTimestampPattern = regexp.MustCompile(`(?s)<timestamp>(.*?)</timestamp>`)
	cursorUTCOffsetPattern = regexp.MustCompile(`\s*\(UTC([+-])(\d{1,2})(?::?(\d{2}))?\)\s*$`)
)

// DefaultCursorRoot resolves the Cursor data directory (~/.cursor).
func DefaultCursorRoot() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, cursorRootRelative)
	}
	return ""
}

func cursorRolloutPath(slug, id string) string {
	return cursorRolloutPrefix + slug + "/" + id
}

type cursorChat struct {
	slug      string
	id        string
	mainFile  string
	subagents []string
	modTime   time.Time
}

// findCursorChats lists chats whose newest file changed at or after cutoff.
func findCursorChats(root string, cutoff time.Time) []cursorChat {
	slugs, err := os.ReadDir(filepath.Join(root, "projects"))
	if err != nil {
		return nil
	}
	var chats []cursorChat
	for _, slug := range slugs {
		if !slug.IsDir() {
			continue
		}
		base := filepath.Join(root, "projects", slug.Name(), "agent-transcripts")
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			chat := cursorChat{slug: slug.Name(), id: entry.Name(), mainFile: filepath.Join(base, entry.Name(), entry.Name()+".jsonl")}
			st, err := os.Stat(chat.mainFile)
			if err != nil || st.IsDir() {
				continue
			}
			chat.modTime = st.ModTime()
			if subs, err := filepath.Glob(filepath.Join(base, entry.Name(), "subagents", "*.jsonl")); err == nil {
				sort.Strings(subs)
				for _, sub := range subs {
					if sst, err := os.Stat(sub); err == nil && !sst.IsDir() {
						chat.subagents = append(chat.subagents, sub)
						if sst.ModTime().After(chat.modTime) {
							chat.modTime = sst.ModTime()
						}
					}
				}
			}
			if !cutoff.IsZero() && chat.modTime.Before(cutoff) {
				continue
			}
			chats = append(chats, chat)
		}
	}
	sort.Slice(chats, func(i, j int) bool { return chats[i].modTime.Before(chats[j].modTime) })
	return chats
}

func cursorChatFingerprint(chat cursorChat) string {
	h := sha256.New()
	fmt.Fprintf(h, "cursor|%s|%s", chat.slug, chat.id)
	for _, path := range append([]string{chat.mainFile}, chat.subagents...) {
		if st, err := os.Stat(path); err == nil {
			fmt.Fprintf(h, "|%s:%d:%d", filepath.Base(path), st.Size(), st.ModTime().UnixNano())
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// indexCursorSessions is the cursor branch of Index. A missing root is not
// an error: the provider simply has nothing to index.
func indexCursorSessions(ctx context.Context, store *Store, opts Options, cutoff time.Time) (int, error) {
	root := opts.CursorRoot
	if root == "" {
		root = DefaultCursorRoot()
	}
	if root == "" {
		return 0, nil
	}
	count := 0
	for _, chat := range findCursorChats(root, cutoff) {
		select {
		case <-ctx.Done():
			return count, ctx.Err()
		default:
		}
		rolloutPath := cursorRolloutPath(chat.slug, chat.id)
		tombstoned, err := store.sessionTombstoned(chat.id, rolloutPath)
		if err != nil {
			return count, err
		}
		if tombstoned {
			continue
		}
		if !opts.Force && time.Since(chat.modTime) < cursorIndexQuietPeriod {
			continue
		}
		fingerprint := cursorChatFingerprint(chat)
		if !opts.Force {
			unchanged, err := store.cursorChatUnchanged(rolloutPath, fingerprint)
			if err != nil {
				return count, err
			}
			if unchanged {
				continue
			}
		}
		parsed, err := parseCursorChat(chat, fingerprint)
		if err != nil {
			return count, fmt.Errorf("parse cursor chat %s: %w", chat.id, err)
		}
		parsed.Session.Machine = opts.Machine
		if !opts.StoreRawEvents {
			parsed.RawEvents = nil
		}
		if err := store.upsert(parsed, map[string]any{"provider": "cursor"}); err != nil {
			if errors.Is(err, errSessionTombstoned) {
				continue
			}
			return count, err
		}
		count++
	}
	return count, nil
}

func (s *Store) cursorChatUnchanged(rolloutPath, fingerprint string) (bool, error) {
	row := s.db.QueryRow(`SELECT rollout_sha256 FROM codex_sessions WHERE rollout_path=? LIMIT 1`, rolloutPath)
	var stored string
	switch err := row.Scan(&stored); err {
	case nil:
		return stored != "" && stored == fingerprint, nil
	case sql.ErrNoRows:
		return false, nil
	default:
		return false, fmt.Errorf("lookup indexed cursor chat %s: %w", rolloutPath, err)
	}
}

type cursorLine struct {
	Role    string `json:"role"`
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type cursorItem struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

func cursorItems(raw json.RawMessage) []cursorItem {
	var items []cursorItem
	if json.Unmarshal(raw, &items) == nil {
		return items
	}
	var text string
	if json.Unmarshal(raw, &text) == nil && text != "" {
		return []cursorItem{{Type: "text", Text: text}}
	}
	return nil
}

// cursorUserParts splits a user message into its query text and the
// wall-clock time from the <timestamp> tag (zero when absent or unparseable).
func cursorUserParts(items []cursorItem) (string, time.Time) {
	var parts []string
	for _, item := range items {
		if item.Type == "text" {
			parts = append(parts, item.Text)
		}
	}
	text := strings.Join(parts, "\n")
	var at time.Time
	if m := cursorTimestampPattern.FindStringSubmatch(text); m != nil {
		at = parseCursorTimestamp(m[1])
	}
	if m := cursorUserQueryPattern.FindStringSubmatch(text); m != nil {
		text = m[1]
	} else {
		text = cursorTimestampPattern.ReplaceAllString(text, "")
	}
	return strings.TrimSpace(text), at
}

// parseCursorTimestamp reads "Tuesday, Oct 27, 2026, 2:50 PM (UTC-4)".
func parseCursorTimestamp(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	loc := time.UTC
	if m := cursorUTCOffsetPattern.FindStringSubmatchIndex(raw); m != nil {
		sub := cursorUTCOffsetPattern.FindStringSubmatch(raw)
		hours, _ := strconv.Atoi(sub[2])
		minutes, _ := strconv.Atoi(sub[3])
		offset := hours*3600 + minutes*60
		if sub[1] == "-" {
			offset = -offset
		}
		loc = time.FixedZone("", offset)
		raw = raw[:m[0]]
	}
	for _, layout := range []string{"Monday, Jan 2, 2006, 3:04 PM", "Monday, January 2, 2006, 3:04 PM"} {
		if t, err := time.ParseInLocation(layout, raw, loc); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func parseCursorChat(chat cursorChat, fingerprint string) (ParsedSession, error) {
	p := newParsedSession(0)
	p.Session.ID = chat.id
	p.Session.RolloutPath = cursorRolloutPath(chat.slug, chat.id)
	p.Session.RolloutSHA256 = fingerprint
	p.Session.Source = "cursor"
	p.Session.ModelProvider = "cursor"
	p.Session.CWD = cursorSlugToDir(chat.slug)
	p.Session.Status = "seen"
	p.Session.UpdatedAt = chat.modTime.UTC().Format(time.RFC3339Nano)

	files := map[string]bool{}
	tools := map[string]bool{}
	lineNo := 0
	var firstTurnAt time.Time
	turnTS := ""
	var rawBytes int64
	for fileIdx, path := range append([]string{chat.mainFile}, chat.subagents...) {
		isSub := fileIdx > 0
		f, err := os.Open(path)
		if err != nil {
			if isSub {
				continue
			}
			return ParsedSession{}, err
		}
		reader := bufio.NewReaderSize(f, 64*1024)
		for {
			raw, readErr := reader.ReadBytes('\n')
			rawBytes += int64(len(raw))
			if len(raw) > 0 && len(raw) <= cursorMaxLineBytes {
				lineNo++
				var line cursorLine
				if json.Unmarshal(raw, &line) == nil {
					handleCursorLine(&p, line, raw, lineNo, isSub, &turnTS, &firstTurnAt, files, tools)
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				f.Close()
				return p, readErr
			}
		}
		f.Close()
	}
	p.Coverage.RawBytes = rawBytes
	if !firstTurnAt.IsZero() {
		p.Session.CreatedAt = firstTurnAt.UTC().Format(time.RFC3339Nano)
	}
	finalizeParsedMessages(&p)
	for f := range files {
		p.Session.FilesTouched = append(p.Session.FilesTouched, f)
	}
	sort.Strings(p.Session.FilesTouched)
	for t := range tools {
		p.Session.ToolNames = append(p.Session.ToolNames, t)
	}
	sort.Strings(p.Session.ToolNames)
	p.Session.Title = short(first(p.Session.Title, p.Session.FirstUserMessage, p.Session.ID), 240)
	p.SearchBlob = truncate(strings.Join([]string{p.Session.Title, p.Session.CWD, strings.Join(p.Session.Commands, "\n"), strings.Join(p.Session.FilesTouched, "\n"), messagesText(p.Messages)}, "\n"), maxSearchBlobText)
	return p, nil
}

// handleCursorLine folds one transcript line into the parsed session.
// Unknown line shapes are ignored. Messages carry the most recent user
// message's wall-clock time (the only timestamp Cursor records); before the
// first one they are untimed.
func handleCursorLine(p *ParsedSession, line cursorLine, raw []byte, lineNo int, isSub bool, turnTS *string, firstTurnAt *time.Time, files, tools map[string]bool) {
	label := first(line.Role, line.Type, "unknown")
	if isSub {
		label = "subagent:" + label
	}
	p.EventCounts["cursor:"+label]++
	if len(p.RawEvents) < maxStoredRawEventsPerSession {
		rawJSON := strings.TrimSpace(string(raw))
		if len(rawJSON) > maxStoredRawEventJSON {
			rawJSON = truncate(rawJSON, maxStoredRawEventJSON) + fmt.Sprintf("\n...[truncated raw event from %d bytes]", len(raw))
		}
		p.RawEvents = append(p.RawEvents, RawEvent{lineNo, *turnTS, "cursor:" + label, "", redact(rawJSON)})
	}
	items := cursorItems(line.Message.Content)
	subLine := 0
	emit := func(role, kind, text string) {
		subLine++
		appendParsedMessage(p, Message{lineNo*1000 + subLine, *turnTS, role, kind, text})
	}
	switch line.Role {
	case "user":
		if isSub {
			return
		}
		text, at := cursorUserParts(items)
		if !at.IsZero() {
			*turnTS = at.UTC().Format(time.RFC3339Nano)
			if firstTurnAt.IsZero() {
				*firstTurnAt = at
			}
		}
		text = capMessageText(normalizeUserText(text))
		if text == "" {
			return
		}
		if p.Session.FirstUserMessage == "" {
			p.Session.FirstUserMessage = short(text, maxStoredFirstUserText)
		}
		emit("user", "message", text)
	case "assistant":
		for _, item := range items {
			switch item.Type {
			case "text":
				text := capMessageText(strings.TrimSpace(strings.ReplaceAll(item.Text, "[REDACTED]", "")))
				if text == "" {
					continue
				}
				if !isSub {
					p.Session.LastAgentMessage = short(text, maxStoredMessageText)
				}
				emit("assistant", "message", text)
			case "tool_use":
				if item.Name == "" {
					continue
				}
				tools[item.Name] = true
				cmd := first(str(item.Input["command"]), str(item.Input["cmd"]))
				path := first(str(item.Input["path"]), str(item.Input["file_path"]), str(item.Input["target_file"]), str(item.Input["notebook_path"]))
				if cmd != "" {
					appendSessionCommand(p, cmd)
					addPaths(files, cmd)
				}
				if path != "" {
					files[path] = true
				}
				if text := capMessageText(first(cmd, path, compactJSON(item.Input))); text != "" {
					emit("tool", item.Name, text)
				}
			}
		}
	}
}

// cursorSlugToDir maps a project slug back to a directory by trying every
// regrouping of its "-" separated parts into existing path segments (a "-"
// in a real directory name is indistinguishable from a "/"). Falls back to
// the all-slashes reading. Mirrors codexsessions.cursorSlugToPath; the two
// packages do not import each other.
func cursorSlugToDir(slug string) string {
	parts := strings.Split(slug, "-")
	var resolve func(base string, rest []string) (string, bool)
	resolve = func(base string, rest []string) (string, bool) {
		if len(rest) == 0 {
			return base, true
		}
		for n := 1; n <= len(rest); n++ {
			candidate := filepath.Join(base, strings.Join(rest[:n], "-"))
			if st, err := os.Stat(candidate); err != nil || (!st.IsDir() && n < len(rest)) {
				continue
			}
			if path, ok := resolve(candidate, rest[n:]); ok {
				return path, true
			}
		}
		return "", false
	}
	if path, ok := resolve("/", parts); ok {
		return path
	}
	return "/" + strings.ReplaceAll(slug, "-", "/")
}
