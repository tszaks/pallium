package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/tszaks/pallium/internal/codeindex"
	"github.com/tszaks/pallium/internal/db"
)

// Staleness detection for the knowledge base.
//
// Both build and audit read line numbers, signatures and doc comments out of
// the content index and hand them to a model as fact. If a file has changed
// since it was indexed, those line numbers point at the wrong code, and the
// model reasons confidently about whatever happens to sit there now.
//
// This was not hypothetical. An audit of internal/db was shown WithTx's body
// under the label *Store.Repo, because db.go had grown 28 lines since the last
// `pallium index`. It then refuted true claims, precisely and in detail, on
// evidence that was misaligned rather than wrong. A stale index does not
// degrade this feature gracefully; it makes it lie.
//
// So staleness is a gate, not a warning.
func staleModuleSlugs(store *db.Store, repoID int64, repoRoot string, modules []Module) (map[string]struct{}, error) {
	stored, err := store.CodeFileSHAs(repoID)
	if err != nil {
		return nil, err
	}

	// Hash each file once even when several modules share it.
	current := make(map[string]string, len(stored))
	hashOf := func(path string) string {
		if hash, ok := current[path]; ok {
			return hash
		}
		content, readErr := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(path)))
		if readErr != nil {
			current[path] = ""
			return ""
		}
		sum := sha256.Sum256(content)
		hash := hex.EncodeToString(sum[:])
		current[path] = hash
		return hash
	}

	stale := make(map[string]struct{})
	for _, module := range modules {
		for _, path := range module.Files {
			if hashOf(path) != stored[path] {
				stale[module.Slug] = struct{}{}
				break
			}
		}
	}
	indexed := make(map[string]struct{}, len(stored))
	for path := range stored {
		indexed[path] = struct{}{}
	}
	candidates, err := codeindex.UnindexedCandidates(repoRoot, indexed)
	if err != nil {
		return nil, err
	}
	dirs := make([]string, 0, len(modules))
	for _, module := range modules {
		dirs = append(dirs, module.Dir)
	}
	owner := newOwnerIndex(dirs)
	for _, path := range candidates {
		dir := owner.moduleFor(path)
		if dir == "" {
			for _, module := range modules {
				stale[module.Slug] = struct{}{}
			}
			continue
		}
		stale[slugForDir(dir)] = struct{}{}
	}
	return stale, nil
}
