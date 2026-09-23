package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tszaks/pallium/internal/codeindex"
	"github.com/tszaks/pallium/internal/db"
	"github.com/tszaks/pallium/internal/gitlog"
)

// Snapshot binds knowledge to a worktree, HEAD, parser and tracked file bytes.
// Untracked files are intentionally excluded. Missing files remain represented.
func Snapshot(root string) (string, error) {
	paths, err := codeindex.SourcePaths(root)
	if err != nil {
		return "", err
	}
	opts, err := codeindex.ReadProjectOptions(root)
	if err != nil {
		return "", err
	}
	paths = append(paths, opts.IncludeUntracked...)
	if _, err := os.Stat(filepath.Join(root, ".pallium", "modules.json")); err == nil {
		paths = append(paths, ".pallium/modules.json")
	}
	paths = sortedUnique(paths)
	sort.Strings(paths)
	h := sha256.New()
	abs, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	head, err := gitlog.CurrentCommit(root)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00", abs, codeindex.ParserVersion, head)
	// Hash names and content, including docs/config that can affect interpretation.
	for _, p := range paths {
		fmt.Fprintf(h, "%s\x00", p)
		info, err := os.Lstat(filepath.Join(root, p))
		if os.IsNotExist(err) {
			h.Write([]byte("missing\x00"))
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, e := os.Readlink(filepath.Join(root, p))
			if e != nil {
				return "", e
			}
			fmt.Fprintf(h, "link:%s\x00", target)
			continue
		}
		resolved, e := filepath.EvalSymlinks(filepath.Join(root, p))
		if e != nil {
			return "", e
		}
		rel, e := filepath.Rel(abs, resolved)
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("tracked source escapes workspace: %s", p)
		}
		if !info.Mode().IsRegular() {
			fmt.Fprintf(h, "mode:%s\x00", info.Mode())
			continue
		}
		f, err := os.Open(resolved)
		if err != nil {
			return "", err
		}
		fileHash := sha256.New()
		_, copyErr := io.Copy(fileHash, f)
		closeErr := f.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		h.Write(fileHash.Sum(nil))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Assess annotates stored historical documents without changing their provenance.
func Assess(store *db.Store, docs []db.KnowledgeDoc) error {
	snapshot, err := Snapshot(store.RepoRoot)
	if err != nil {
		return err
	}
	for i := range docs {
		d := &docs[i]
		if d.Kind == "decision" {
			d.Freshness = "authored_intent"
			d.Verified = false
			continue
		}
		d.Freshness = "current"
		if d.Evidence.Snapshot == "" {
			d.Freshness = "needs_rebuild"
		} else if d.Evidence.Snapshot != snapshot {
			d.Freshness = "stale"
		}
		if d.Evidence.State == "" {
			d.Evidence.State = "legacy"
		}
		// Retained only for old clients. It must never imply stale prose is verified.
		d.Verified = d.Freshness == "current" && d.Evidence.State == "audited_supported"
	}
	return nil
}

func stamp(doc *db.KnowledgeDoc, snapshot string) {
	doc.Evidence = db.Evidence{Snapshot: snapshot, ParserVersion: codeindex.ParserVersion, State: "structural"}
	if strings.Contains(doc.Generator, "model") {
		doc.Evidence.State = "citations_checked"
	}
	if len(doc.DroppedClaims) > 0 {
		doc.Evidence.State = "claims_removed"
	}
	if strings.Contains(doc.Generator, "failed") {
		doc.Evidence.State = "generation_failed"
	}
}
