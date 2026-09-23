package codeindex

import (
	"github.com/tszaks/pallium/internal/db"
	"path/filepath"
	"strings"
)

// Documents are searchable source text, not inferred code declarations.
func parseDocument(path, lang string, content []byte) ParsedFile {
	text := string(content)
	if len(text) > 16000 {
		text = text[:16000]
	}
	return ParsedFile{Lang: lang, Parser: "document", Symbols: []db.CodeSymbol{{Path: path, Name: filepath.Base(path), Kind: "document", Doc: text, StartLine: 1, EndLine: strings.Count(text, "\n") + 1}}}
}
