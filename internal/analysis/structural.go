package analysis

import (
	"path/filepath"
	"strings"

	"github.com/tszaks/pallium/internal/db"
)

// StructuralLinks answers "what else is wired to this file": its test, its
// imports, and the files that import it.
//
// It used to compute that by reading every file in the working tree on every
// call. The content index makes the same answer four SQL queries, so the cost
// stops scaling with repo size. structuralLinksByScan is kept as the fallback
// for an index written before content indexing existed, or a directory that is
// not a git repo, and the two paths are held to the same output contract: same
// link kinds, same priority order, same reason strings.
func StructuralLinks(store *db.Store, targetPath string, limit int) ([]StructuralLink, error) {
	normalized, err := normalizeRepoPath(store.RepoRoot, targetPath)
	if err != nil {
		return nil, err
	}

	repo, err := store.Repo()
	if err != nil || !store.HasCodeIndex(repo.ID) {
		return structuralLinksByScan(store, normalized, limit)
	}

	links, err := structuralLinksIndexed(store, repo.ID, normalized, limit)
	if err != nil {
		return nil, err
	}
	return links, nil
}

// structuralEvidence is everything the index knows about one file's wiring,
// gathered up front so the candidate loop is pure map lookups.
type structuralEvidence struct {
	// importedGoDirs are the in-repo Go package directories the target imports.
	importedGoDirs map[string]struct{}
	// importedPaths are the in-repo files the target imports directly.
	importedPaths map[string]string
	// dependentPaths are the files that import the target file directly.
	dependentPaths map[string]string
	// goPackageDependents are files importing the target's Go package.
	goPackageDependents map[string]struct{}
	// targetRefs are the lowercased identifiers the target references.
	targetRefs map[string]struct{}
	// stemReferrers are files referencing an identifier matching the target's
	// own file stem, which is the reverse of the go-symbol heuristic.
	stemReferrers map[string]struct{}
}

func structuralLinksIndexed(store *db.Store, repoID int64, normalized string, limit int) ([]StructuralLink, error) {
	files, err := repoFiles(store.RepoRoot)
	if err != nil {
		return nil, err
	}

	evidence, err := gatherStructuralEvidence(store, repoID, normalized)
	if err != nil {
		return nil, err
	}

	targetDir := filepath.ToSlash(filepath.Dir(normalized))
	targetName := filepath.Base(normalized)
	targetStem := fileStem(targetName)
	targetIsTest := isTestFile(targetName)
	targetIsGo := strings.HasSuffix(targetName, ".go")

	out := make([]StructuralLink, 0)
	for _, candidate := range files {
		if candidate == normalized {
			continue
		}

		candidateName := filepath.Base(candidate)
		candidateDir := filepath.ToSlash(filepath.Dir(candidate))
		candidateStem := fileStem(candidateName)
		candidateIsTest := isTestFile(candidateName)
		candidateIsGo := strings.HasSuffix(candidateName, ".go")

		_, candidateInImportedGoDir := evidence.importedGoDirs[candidateDir]
		_, candidateImportedByTarget := evidence.importedPaths[candidate]
		_, candidateImportsTarget := evidence.dependentPaths[candidate]
		_, candidateImportsTargetPackage := evidence.goPackageDependents[candidate]
		_, targetRefsCandidateStem := evidence.targetRefs[strings.ToLower(candidateStem)]
		_, candidateRefsTargetStem := evidence.stemReferrers[candidate]

		switch {
		case targetStem != "" && candidateStem == targetStem && targetIsTest != candidateIsTest:
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "test-pair",
				Reason: "Shares the same source stem as the target file.",
			})
		case targetStem != "" && candidateStem == targetStem:
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "same-stem",
				Reason: "Shares the same file stem as the target file.",
			})
		case targetIsGo && candidateIsGo && candidateStem != "" && targetRefsCandidateStem:
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "go-symbol",
				Reason: "Target file references a symbol that matches this Go file's stem.",
			})
		case targetIsGo && candidateIsGo && candidateInImportedGoDir:
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "go-import",
				Reason: "Target file imports this Go package from the same repo.",
			})
		case targetIsGo && candidateIsGo && candidateRefsTargetStem:
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "go-dependent",
				Reason: "This Go file appears to reference the target file's symbol stem.",
			})
		case targetIsGo && candidateIsGo && candidateImportsTargetPackage:
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "go-package-dependent",
				Reason: "This Go file imports the target package from the same repo.",
			})
		case candidateImportedByTarget && evidence.importedPaths[candidate] == "js-import":
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "js-import",
				Reason: "Target file imports this JS/TS module with a relative path.",
			})
		case candidateImportsTarget && evidence.dependentPaths[candidate] == "js-import":
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "js-dependent",
				Reason: "This JS/TS file imports the target module with a relative path.",
			})
		case candidateImportedByTarget && evidence.importedPaths[candidate] == "py-import":
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "py-import",
				Reason: "Target file imports this Python module with a local import path.",
			})
		case candidateImportsTarget && evidence.dependentPaths[candidate] == "py-import":
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "py-dependent",
				Reason: "This Python file imports the target module with a local import path.",
			})
		case candidateDir == targetDir && candidateIsTest:
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "same-dir-test",
				Reason: "Test file in the same directory as the target file.",
			})
		case candidateDir == targetDir && filepath.Ext(candidateName) == filepath.Ext(targetName):
			out = append(out, StructuralLink{
				Path:   candidate,
				Kind:   "same-dir",
				Reason: "File in the same directory with the same extension.",
			})
		}
	}

	return uniqueStructuralLinks(out, limit), nil
}

func gatherStructuralEvidence(store *db.Store, repoID int64, normalized string) (structuralEvidence, error) {
	evidence := structuralEvidence{
		importedGoDirs:      map[string]struct{}{},
		importedPaths:       map[string]string{},
		dependentPaths:      map[string]string{},
		goPackageDependents: map[string]struct{}{},
		targetRefs:          map[string]struct{}{},
		stemReferrers:       map[string]struct{}{},
	}

	outgoing, err := store.ImportsFrom(repoID, normalized)
	if err != nil {
		return structuralEvidence{}, err
	}
	for _, imp := range outgoing {
		if imp.ToPath == "" {
			continue
		}
		if imp.Kind == "go-import" {
			// A Go import names a package directory, so it expands to every
			// file in that directory rather than to one file.
			evidence.importedGoDirs[imp.ToPath] = struct{}{}
			continue
		}
		evidence.importedPaths[imp.ToPath] = imp.Kind
	}

	incoming, err := store.ImportsTo(repoID, normalized)
	if err != nil {
		return structuralEvidence{}, err
	}
	for _, imp := range incoming {
		if imp.Kind == "go-import" {
			continue
		}
		evidence.dependentPaths[imp.FromPath] = imp.Kind
	}

	targetDir := filepath.ToSlash(filepath.Dir(normalized))
	packageDependents, err := store.ImportsTo(repoID, targetDir)
	if err != nil {
		return structuralEvidence{}, err
	}
	for _, imp := range packageDependents {
		if imp.Kind != "go-import" || imp.FromPath == normalized {
			continue
		}
		evidence.goPackageDependents[imp.FromPath] = struct{}{}
	}

	refs, err := store.RefNamesLower(repoID, normalized)
	if err != nil {
		return structuralEvidence{}, err
	}
	evidence.targetRefs = refs

	targetStem := fileStem(filepath.Base(normalized))
	if targetStem != "" {
		referrers, err := store.PathsReferencingLower(repoID, []string{strings.ToLower(targetStem)})
		if err != nil {
			return structuralEvidence{}, err
		}
		for _, paths := range referrers {
			for _, path := range paths {
				if path == normalized {
					continue
				}
				evidence.stemReferrers[path] = struct{}{}
			}
		}
	}

	return evidence, nil
}
