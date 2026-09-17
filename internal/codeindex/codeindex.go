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
//
// 3: resolver inputs are tagged and generic Go call references are indexed.
const ParserVersion = "3"

func parserTag(parser, key string) string {
	return parser + "@" + ParserVersion + "#" + key
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
	resolverKey, err := resolverKey(store.RepoRoot, paths)
	if err != nil {
		return Result{}, err
	}

	existing, err := store.CodeFileStates(repoID)
	if err != nil {
		return Result{}, err
	}

	resolver := NewResolver(store.RepoRoot, paths)

	result := Result{Files: len(candidates)}
	live := make(map[string]struct{}, len(candidates))

	for _, path := range candidates {
		live[path] = struct{}{}

		content, size, ok := readIndexable(store.RepoRoot, path)
		if !ok {
			if _, statErr := os.Lstat(filepath.Join(store.RepoRoot, filepath.FromSlash(path))); statErr == nil {
				result.Skipped++
			}
			// Listed by git but gone from disk: a deleted-but-staged file.
			// Drop whatever the index still holds for it.
			if _, ok := existing[path]; ok {
				if err := store.DeleteCodeFile(repoID, path); err != nil {
					return Result{}, err
				}
				result.Removed++
				delete(existing, path)
			}
			delete(live, path)
			continue
		}

		sum := sha256.Sum256(content)
		contentSHA := hex.EncodeToString(sum[:])
		if previous, ok := existing[path]; ok && previous.ContentSHA == contentSHA &&
			strings.HasSuffix(previous.Parser, "@"+ParserVersion+"#"+resolverKey) {
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
			SizeBytes:  size,
			ContentSHA: contentSHA,
			Parser:     parserTag(parsed.Parser, resolverKey),
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

	stats, err := store.Stats(repoID)
	if err != nil {
		return Result{}, err
	}
	result.Symbols = stats.Symbols
	result.Imports = stats.Imports
	result.Refs = stats.Refs
	return result, nil
}

func readIndexable(repoRoot, path string) ([]byte, int64, bool) {
	absolute, err := filepath.Abs(filepath.Join(repoRoot, filepath.FromSlash(path)))
	if err != nil {
		return nil, 0, false
	}
	root, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return nil, 0, false
	}
	entry, err := os.Lstat(absolute)
	if err != nil {
		return nil, 0, false
	}
	target := absolute
	if entry.Mode()&os.ModeSymlink != 0 {
		target, err = filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, 0, false
		}
		relative, err := filepath.Rel(root, target)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, 0, false
		}
	}
	info, err := os.Stat(target)
	if err != nil || info.IsDir() || info.Size() > maxParsedFileBytes {
		return nil, 0, false
	}
	content, err := os.ReadFile(target)
	if err != nil || looksGenerated(content) {
		return nil, 0, false
	}
	return content, info.Size(), true
}

func UnindexedCandidates(repoRoot string, indexed map[string]struct{}) ([]string, error) {
	paths, err := gitlog.TrackedFiles(repoRoot)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0)
	for _, path := range paths {
		if vendorish(path) || !Parseable(Lang(path)) {
			continue
		}
		if _, ok := indexed[path]; ok {
			continue
		}
		if _, _, ok := readIndexable(repoRoot, path); ok {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out, nil
}

func resolverKey(repoRoot string, paths []string) (string, error) {
	hasher := sha256.New()
	sorted := append([]string{}, paths...)
	sort.Strings(sorted)
	for _, path := range sorted {
		hasher.Write([]byte(path))
		hasher.Write([]byte{0})
	}
	for _, path := range sorted {
		base := filepath.Base(path)
		if base != "tsconfig.json" && base != "jsconfig.json" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(path)))
		if err != nil {
			return "", err
		}
		hasher.Write([]byte(path))
		hasher.Write([]byte{0})
		hasher.Write(content)
		hasher.Write([]byte{0})
	}
	content, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	hasher.Write(content)
	sum := hasher.Sum(nil)
	return hex.EncodeToString(sum)[:12], nil
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
