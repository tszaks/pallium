package sessionmemory

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func parseRollout(path string) (ParsedSession, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ParsedSession{}, err
	}
	sha, _ := fileSHA(path)
	if info.Size() > maxParseRolloutBytes {
		return parseRolloutMetadataOnly(path, info, sha), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return ParsedSession{}, err
	}
	defer f.Close()
	p := ParsedSession{EventCounts: map[string]int{}}
	p.Session.RolloutPath = path
	p.Session.RolloutSHA256 = sha
	files := map[string]bool{}
	tools := map[string]bool{}
	reader := bufio.NewReader(f)
	lineNo := 0
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			var obj map[string]any
			if json.Unmarshal(line, &obj) == nil {
				typ := str(obj["type"])
				if typ == "" {
					typ = "unknown"
				}
				p.EventCounts[typ]++
				ts := isoAny(obj["timestamp"])
				if p.Session.CreatedAt == "" {
					p.Session.CreatedAt = ts
				}
				if ts != "" {
					p.Session.UpdatedAt = ts
				}
				payload, _ := obj["payload"].(map[string]any)
				ptype := str(payload["type"])
				if len(p.RawEvents) < maxStoredRawEventsPerSession {
					raw, _ := json.Marshal(obj)
					rawJSON := string(raw)
					if len(rawJSON) > maxStoredRawEventJSON {
						rawJSON = rawJSON[:maxStoredRawEventJSON] + fmt.Sprintf("\n...[truncated raw event from %d bytes]", len(rawJSON))
					}
					rawJSON = redact(rawJSON)
					p.RawEvents = append(p.RawEvents, RawEvent{lineNo, ts, typ, ptype, rawJSON})
				}
				switch typ {
				case "session_meta":
					p.Session.ID = str(payload["id"])
					p.Session.CWD = first(p.Session.CWD, str(payload["cwd"]))
					p.Session.Source = first(p.Session.Source, str(payload["source"]), str(payload["originator"]))
					p.Session.ModelProvider = first(p.Session.ModelProvider, str(payload["model_provider"]))
					p.Session.Model = first(p.Session.Model, str(payload["model"]))
					p.Session.CLIVersion = first(p.Session.CLIVersion, str(payload["cli_version"]))
				case "turn_context":
					p.Session.CWD = first(p.Session.CWD, str(payload["cwd"]))
				case "event_msg":
					handleEventMessage(&p, payload, lineNo, ts, files)
				case "response_item":
					handleResponseItem(&p, payload, lineNo, ts, files, tools)
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return p, err
		}
	}
	if p.Session.ID == "" {
		p.Session.ID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	for f := range files {
		p.Session.FilesTouched = append(p.Session.FilesTouched, f)
	}
	sort.Strings(p.Session.FilesTouched)
	for t := range tools {
		p.Session.ToolNames = append(p.Session.ToolNames, t)
	}
	sort.Strings(p.Session.ToolNames)
	p.Session.Title = short(first(p.Session.FirstUserMessage, p.Session.Title), 240)
	p.SearchBlob = truncate(strings.Join([]string{p.Session.Title, p.Session.CWD, strings.Join(p.Session.Commands, "\n"), strings.Join(p.Session.FilesTouched, "\n"), messagesText(p.Messages)}, "\n"), maxSearchBlobText)
	return p, nil
}

func parseRolloutMetadataOnly(path string, info os.FileInfo, sha string) ParsedSession {
	id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	p := ParsedSession{EventCounts: map[string]int{"skipped_large_rollout": 1}}
	p.Session.ID = id
	p.Session.RolloutPath = path
	p.Session.RolloutSHA256 = sha
	p.Session.UpdatedAt = info.ModTime().UTC().Format(time.RFC3339Nano)
	p.Session.CreatedAt = p.Session.UpdatedAt
	p.Session.Status = "skipped_large_rollout"
	p.Session.Errors = append(p.Session.Errors, fmt.Sprintf("skipped full parse: rollout is %d bytes (> %d byte safety limit)", info.Size(), maxParseRolloutBytes))
	p.Session.Title = id
	p.SearchBlob = strings.Join([]string{p.Session.Title, p.Session.RolloutPath, strings.Join(p.Session.Errors, "\n")}, "\n")
	return p
}

func parseClaudeTranscript(path string) (ParsedSession, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ParsedSession{}, err
	}
	sha, _ := fileSHA(path)
	if info.Size() > maxParseRolloutBytes {
		p := parseRolloutMetadataOnly(path, info, sha)
		p.Session.Source = "claude"
		p.Session.ModelProvider = "anthropic"
		return p, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return ParsedSession{}, err
	}
	defer f.Close()
	p := ParsedSession{EventCounts: map[string]int{}}
	p.Session.RolloutPath = path
	p.Session.RolloutSHA256 = sha
	p.Session.Source = "claude"
	p.Session.ModelProvider = "anthropic"
	p.Session.ID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	files := map[string]bool{}
	tools := map[string]bool{}
	reader := bufio.NewReader(f)
	lineNo := 0
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			var obj map[string]any
			if json.Unmarshal(line, &obj) == nil {
				handleClaudeLine(&p, obj, lineNo, files, tools)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return p, err
		}
	}
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

func handleClaudeLine(p *ParsedSession, obj map[string]any, lineNo int, files, tools map[string]bool) {
	typ := first(str(obj["type"]), "unknown")
	p.EventCounts[typ]++
	ts := isoAny(obj["timestamp"])
	if p.Session.CreatedAt == "" && ts != "" {
		p.Session.CreatedAt = ts
	}
	if ts != "" {
		p.Session.UpdatedAt = ts
	}
	p.Session.ID = first(str(obj["sessionId"]), p.Session.ID)
	p.Session.CWD = first(p.Session.CWD, str(obj["cwd"]))
	p.Session.CLIVersion = first(p.Session.CLIVersion, str(obj["version"]))
	p.Session.GitBranch = first(p.Session.GitBranch, str(obj["gitBranch"]))
	p.Session.Title = first(p.Session.Title, str(obj["aiTitle"]))
	if model := str(obj["model"]); model != "" {
		p.Session.Model = first(p.Session.Model, model)
	}
	if len(p.RawEvents) < maxStoredRawEventsPerSession {
		raw, _ := json.Marshal(redactObj(obj))
		rawJSON := string(raw)
		if len(rawJSON) > maxStoredRawEventJSON {
			rawJSON = rawJSON[:maxStoredRawEventJSON] + fmt.Sprintf("\n...[truncated raw event from %d bytes]", len(rawJSON))
		}
		p.RawEvents = append(p.RawEvents, RawEvent{lineNo, ts, typ, "", rawJSON})
	}
	switch typ {
	case "ai-title":
		p.Session.Title = first(str(obj["aiTitle"]), p.Session.Title)
	case "last-prompt", "queue-operation":
		text := capMessageText(first(str(obj["lastPrompt"]), str(obj["content"])))
		if text != "" && !isContextNoise(text) && p.Session.FirstUserMessage == "" {
			p.Session.FirstUserMessage = short(text, maxStoredMessageText)
		}
	case "user", "assistant", "system":
		handleClaudeMessage(p, obj, lineNo, ts, files, tools)
	}
}

func handleClaudeMessage(p *ParsedSession, obj map[string]any, lineNo int, ts string, files, tools map[string]bool) {
	message, _ := obj["message"].(map[string]any)
	role := first(str(message["role"]), str(obj["type"]))
	if role == "system" {
		return
	}
	if model := str(message["model"]); model != "" {
		p.Session.Model = first(p.Session.Model, model)
	}
	for i, item := range claudeContentItems(message["content"]) {
		messageLineNo := lineNo*1000 + i
		kind := str(item["type"])
		switch kind {
		case "", "text", "thinking":
			text := capMessageText(first(str(item["text"]), str(item["content"])))
			if text != "" && !isContextNoise(text) {
				if role == "user" && p.Session.FirstUserMessage == "" {
					p.Session.FirstUserMessage = short(text, maxStoredMessageText)
				}
				if role == "assistant" {
					p.Session.LastAgentMessage = short(text, maxStoredMessageText)
				}
				p.Messages = append(p.Messages, Message{messageLineNo, ts, role, first(kind, "message"), text})
			}
		case "tool_use":
			name := str(item["name"])
			if name != "" {
				tools[name] = true
			}
			input, _ := item["input"].(map[string]any)
			cmd := first(str(input["command"]), str(input["cmd"]))
			path := first(str(input["file_path"]), str(input["path"]), str(input["notebook_path"]))
			if path != "" {
				files[path] = true
			}
			if cmd != "" {
				p.Session.Commands = append(p.Session.Commands, cmd)
				addPaths(files, cmd)
			}
			text := capMessageText(first(cmd, path, compactJSON(item["input"])))
			if text != "" {
				p.Messages = append(p.Messages, Message{messageLineNo, ts, "tool", name, text})
			}
		case "tool_result":
			text := capMessageText(claudeToolResultText(item["content"]))
			addPaths(files, text)
			if regexp.MustCompile(`(?i)(error|traceback|failed|exception|permission denied)`).MatchString(text) {
				p.Session.Errors = append(p.Session.Errors, short(text, 500))
			}
			if text != "" {
				p.Messages = append(p.Messages, Message{messageLineNo, ts, "tool", "tool_result", text})
			}
		}
	}
}

func claudeContentItems(v any) []map[string]any {
	switch x := v.(type) {
	case string:
		return []map[string]any{{"type": "text", "text": x}}
	case []any:
		items := make([]map[string]any, 0, len(x))
		for _, it := range x {
			if m, ok := it.(map[string]any); ok {
				items = append(items, m)
			}
		}
		return items
	case map[string]any:
		return []map[string]any{x}
	default:
		return nil
	}
}

func claudeToolResultText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, it := range x {
			if m, ok := it.(map[string]any); ok {
				parts = append(parts, first(str(m["text"]), str(m["content"])))
			}
		}
		return strings.Join(parts, "\n")
	default:
		return str(v)
	}
}

func compactJSON(v any) string {
	if v == nil {
		return ""
	}
	b, _ := json.Marshal(redactObj(v))
	return string(b)
}

func handleEventMessage(p *ParsedSession, payload map[string]any, lineNo int, ts string, files map[string]bool) {
	ptype := str(payload["type"])
	switch ptype {
	case "user_message":
		text := capMessageText(str(payload["message"]))
		if text != "" && !isContextNoise(text) {
			if p.Session.FirstUserMessage == "" {
				p.Session.FirstUserMessage = short(text, maxStoredMessageText)
			}
			p.Messages = append(p.Messages, Message{lineNo, ts, "user", "message", text})
		}
	case "agent_message":
		text := capMessageText(str(payload["message"]))
		if text != "" {
			p.Session.LastAgentMessage = short(text, maxStoredMessageText)
			p.Messages = append(p.Messages, Message{lineNo, ts, "assistant", "message", text})
		}
	case "task_complete":
		p.Session.Status = "complete"
		text := capMessageText(str(payload["last_agent_message"]))
		if text != "" {
			p.Session.LastAgentMessage = short(text, maxStoredMessageText)
			p.Messages = append(p.Messages, Message{lineNo, ts, "assistant", "task_complete", text})
		}
	case "turn_aborted":
		p.Session.Status = "aborted"
		if r := str(payload["reason"]); r != "" {
			p.Session.Errors = append(p.Session.Errors, "aborted: "+r)
		}
	case "patch_apply_end":
		addPaths(files, str(payload["stdout"]))
		if changes, ok := payload["changes"].(map[string]any); ok {
			for k := range changes {
				files[k] = true
			}
		}
	}
}

func handleResponseItem(p *ParsedSession, payload map[string]any, lineNo int, ts string, files, mapTools map[string]bool) {
	ptype := str(payload["type"])
	switch ptype {
	case "message":
		role := str(payload["role"])
		text := capMessageText(contentText(payload["content"]))
		if (role == "user" || role == "assistant") && text != "" && !isContextNoise(text) {
			if role == "user" && p.Session.FirstUserMessage == "" {
				p.Session.FirstUserMessage = short(text, maxStoredMessageText)
			}
			if role == "assistant" {
				p.Session.LastAgentMessage = short(text, maxStoredMessageText)
			}
			p.Messages = append(p.Messages, Message{lineNo, ts, role, "message", text})
		}
	case "function_call":
		name := str(payload["name"])
		mapTools[name] = true
		args := parseArgs(payload["arguments"])
		cmd := str(args["cmd"])
		if cmd == "" {
			cmd = str(args["command"])
		}
		if cmd != "" {
			p.Session.Commands = append(p.Session.Commands, cmd)
			p.Messages = append(p.Messages, Message{lineNo, ts, "tool", name, cmd})
		}
	case "custom_tool_call":
		name := str(payload["name"])
		mapTools[name] = true
		input := str(payload["input"])
		addPatchPaths(files, input)
		if input != "" {
			p.Messages = append(p.Messages, Message{lineNo, ts, "tool", name, input})
		}
	case "function_call_output", "custom_tool_call_output":
		out := str(payload["output"])
		addPaths(files, out)
		if regexp.MustCompile(`(?i)(error|traceback|failed|exception|permission denied)`).MatchString(out) {
			p.Session.Errors = append(p.Session.Errors, short(out, 500))
		}
	}
}
