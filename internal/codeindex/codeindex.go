// Package codeindex builds Pallium's content index: the symbols, imports and
// references that live in the files themselves.
//
// Everything else in Pallium's repo intelligence is derived from git history,
// which answers "what moves together" but never "what is this". This package
// is the other half. It runs as part of `pallium index`, is keyed by content
// hash so a second run only reparses what changed, and writes to the same
// sqlite file, so a question that used to cost a full working-tree scan
// becomes a SQL lookup.
package codeindex

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/gitlog"
)

// maxParsedFileBytes caps what the parsers will look at. A file past this is
// recorded as known but unparsed: a 3MB generated client has thousands of
// symbols nobody will ever search for, and parsing it costs more than every
// hand-written file in the repo combined.
const maxParsedFileBytes = 512 * 1024

// ParserVersion invalidates the content index when the parsers themselves
// change. A file's content hash cannot notice that Pallium got better at
// reading it: after an upgrade every hash matches, nothing reparses, and the
// index stays as wrong as it was. Bump this whenever a parser or an import
// resolver changes what it would produce for unchanged input.
//
// 2: TypeScript ESM specifiers ("../server/auth.js") now resolve to their .ts
//
//	sources, which an entire class of Node repo depends on.
const ParserVersion = "2"

func parserTag(parser string) string {
	return parser + "@" + ParserVersion
}

// ParsedFile is one file's contribution to the index.
type ParsedFile struct {
	Lang    string
	Parser  string
	Symbols []db.CodeSymbol
	Imports []db.CodeImport
	Refs    []db.CodeRef
}

// Result reports what a run did. Reparsed versus Unchanged is the number worth
// watching: on a warm index a normal edit session should reparse a handful of
// files, and a number close to Files means the hashing is not working.
type Result struct {
	Files     int `json:"files"`
	Reparsed  int `json:"reparsed"`
	Unchanged int `json:"unchanged"`
	Removed   int `json:"removed"`
	Skipped   int `json:"skipped"`
	Symbols   int `json:"symbols"`
	Imports   int `json:"imports"`
	Refs      int `json:"refs"`
}

// Run indexes the repo's content into store. The caller supplies repoID
// because the git index has already upserted the repo row in the same
// transaction.
func Run(store *db.Store, repoID int64, indexedAt time.Time) (Result, error) {
	paths, err := gitlog.TrackedFiles(store.RepoRoot)
	if err != nil {
		return Result{}, fmt.Errorf("list repo files: %w", err)
	}

	candidates := make([]string, 0, len(paths))
	for _, path := range paths {
		if vendorish(path) {
			continue
		}
		if !Parseable(Lang(path)) {
			continue
		}
		candidates = append(candidates, path)
	}
	sort.Strings(candidates)

	existing, err := store.CodeFileStates(repoID)
	if err != nil {
		return Result{}, err
	}

	resolver := NewResolver(store.RepoRoot, paths)

	result := Result{Files: len(candidates)}
	live := make(map[string]struct{}, len(candidates))

	for _, path := range candidates {
		live[path] = struct{}{}

		absolute := filepath.Join(store.RepoRoot, filepath.FromSlash(path))
		info, statErr := os.Stat(absolute)
		if statErr != nil {
			// Listed by git but gone from disk: a deleted-but-staged file.
			// Drop whatever the index still holds for it.
			if _, ok := existing[path]; ok {
				if err := store.DeleteCodeFile(repoID, path); err != nil {
					return Result{}, err
				}
				result.Removed++
			}
			delete(live, path)
			continue
		}
		if info.Size() > maxParsedFileBytes {
			result.Skipped++
			delete(live, path)
			continue
		}

		content, readErr := os.ReadFile(absolute)
		if readErr != nil {
			result.Skipped++
			delete(live, path)
			continue
		}
		if looksGenerated(content) {
			result.Skipped++
			delete(live, path)
			continue
		}

		sum := sha256.Sum256(content)
		contentSHA := hex.EncodeToString(sum[:])
		if previous, ok := existing[path]; ok && previous.ContentSHA == contentSHA && strings.HasSuffix(previous.Parser, "@"+ParserVersion) {
			result.Unchanged++
			continue
		}

		parsed, ok := Parse(path, content, resolver)
		if !ok {
			result.Skipped++
			delete(live, path)
			continue
		}

		file := db.CodeFile{
			Path:       path,
			Lang:       parsed.Lang,
			SizeBytes:  info.Size(),
			ContentSHA: contentSHA,
			Parser:     parserTag(parsed.Parser),
		}
		if err := store.ReplaceCodeFile(repoID, file, parsed.Symbols, parsed.Imports, parsed.Refs, indexedAt); err != nil {
			return Result{}, err
		}
		result.Reparsed++
		result.Symbols += len(parsed.Symbols)
		result.Imports += len(parsed.Imports)
		result.Refs += len(parsed.Refs)
	}

	// Anything the index still holds that is no longer a live candidate has
	// been deleted, renamed, or newly excluded. Without this the index grows
	// stale symbols forever and `callers` starts citing files that are gone.
	for path := range existing {
		if _, ok := live[path]; ok {
			continue
		}
		if err := store.DeleteCodeFile(repoID, path); err != nil {
			return Result{}, err
		}
		result.Removed++
	}

	return result, nil
}

// Parse dispatches to the right parser for a file. Exported so a caller can
// parse a single buffer (an unsaved edit, a patch under review) without
// touching the index.
func Parse(path string, content []byte, resolver *Resolver) (ParsedFile, bool) {
	lang := Lang(path)
	if lang == "go" {
		return parseGo(path, content, resolver)
	}
	return parseScan(path, lang, content, resolver)
}
