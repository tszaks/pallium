package codeindex

import "strings"

// maskSource preserves byte offsets and newlines while removing comments and,
// optionally, quoted literals. It is a lexical safeguard, not a grammar.
func maskSource(text string, literals bool) string {
	b := []byte(text)
	blank := func(a, z int) {
		for i := a; i < z; i++ {
			if b[i] != '\n' && b[i] != '\r' {
				b[i] = ' '
			}
		}
	}
	for i := 0; i < len(text); {
		start := i
		if i+1 < len(text) && text[i:i+2] == "//" {
			for i < len(text) && text[i] != '\n' {
				i++
			}
			blank(start, i)
			continue
		}
		if i+1 < len(text) && text[i:i+2] == "/*" {
			i += 2
			depth := 1
			for i < len(text) && depth > 0 {
				if i+1 < len(text) && text[i:i+2] == "/*" {
					depth++
					i += 2
				} else if i+1 < len(text) && text[i:i+2] == "*/" {
					depth--
					i += 2
				} else {
					i++
				}
			}
			blank(start, i)
			continue
		}
		if text[i] == '\'' || text[i] == '"' || text[i] == '`' {
			quote := text[i]
			i++
			for i < len(text) {
				if text[i] == '\\' {
					i += 2
					if i > len(text) {
						i = len(text)
					}
					continue
				}
				if text[i] == quote {
					i++
					break
				}
				i++
			}
			if literals {
				blank(start, i)
			}
			continue
		}
		i++
	}
	return string(b)
}

// scanEnd supplies bounded implementation spans for brace-based declarations.
func scanEnd(text string, start int, offsets lineOffsets) int {
	tail := text[start:]
	open := strings.IndexByte(tail, '{')
	if open < 0 || strings.Contains(tail[:open], ";") || strings.Count(tail[:open], "\n") > 8 {
		return offsets.line(start)
	}
	depth := 0
	for i := start + open; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return offsets.line(i)
			}
		}
	}
	return offsets.line(start)
}
