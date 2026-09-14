package workflow

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
)

const maxCodexEventBytes = 4 * 1024 * 1024

type boundedTailBuffer struct {
	data []byte
}

func (b *boundedTailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n >= maxErrorOutputBytes {
		b.data = append(b.data[:0], p[n-maxErrorOutputBytes:]...)
		return n, nil
	}
	if overflow := len(b.data) + n - maxErrorOutputBytes; overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *boundedTailBuffer) String() string { return string(b.data) }

func addCodexUsage(total map[string]any, line []byte) map[string]any {
	var event struct {
		Type  string         `json:"type"`
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal(line, &event) != nil || event.Type != "turn.completed" || event.Usage == nil {
		return total
	}
	if total == nil {
		total = map[string]any{"provider": "codex", "billing_basis": "tokens_only", "cost_status": "unknown"}
	}
	for _, key := range []string{"input_tokens", "cached_input_tokens", "output_tokens"} {
		if n, ok := event.Usage[key].(float64); ok && n >= 0 {
			prev, _ := total[key].(float64)
			total[key] = prev + n
		}
	}
	return total
}

func consumeCodexEvents(reader io.Reader, onLine func([]byte)) (string, map[string]any, error) {
	var tail boundedTailBuffer
	var usage map[string]any
	buffered := bufio.NewReaderSize(reader, 64*1024)
	line := make([]byte, 0, 64*1024)
	oversized := false
	for {
		chunk, readErr := buffered.ReadSlice('\n')
		_, _ = tail.Write(chunk)
		if !oversized && len(line)+len(chunk) <= maxCodexEventBytes {
			line = append(line, chunk...)
		} else {
			oversized = true
		}
		if readErr == bufio.ErrBufferFull {
			continue
		}
		if !oversized && len(line) > 0 {
			line = bytes.TrimSuffix(line, []byte{'\n'})
			line = bytes.TrimSuffix(line, []byte{'\r'})
			usage = addCodexUsage(usage, line)
			if onLine != nil {
				onLine(line)
			}
		}
		line = line[:0]
		oversized = false
		if readErr == io.EOF {
			return tail.String(), usage, nil
		}
		if readErr != nil {
			return tail.String(), usage, readErr
		}
	}
}

// codexUsage reads only completed-turn accounting events, never tool output.
// Output tokens already include reasoning. Dollar cost is not provided by the
// Codex CLI and remains absent, including for subscription-backed execution.
func codexUsage(stream string) map[string]any {
	var total map[string]any
	scanner := bufio.NewScanner(strings.NewReader(stream))
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	for scanner.Scan() {
		total = addCodexUsage(total, scanner.Bytes())
	}
	return total
}

func writeCodexUsageMap(path string, usage map[string]any) {
	if usage != nil {
		if raw, err := json.Marshal(usage); err == nil {
			_ = os.WriteFile(path, raw, 0o600)
		}
	}
}

func writeCodexUsage(path, stream string) {
	writeCodexUsageMap(path, codexUsage(stream))
}
