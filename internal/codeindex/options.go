package codeindex

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ProjectOptions is an explicit local opt-in; no untracked tree is swept.
type ProjectOptions struct {
	Boundaries       []string `json:"boundaries"`
	IncludeUntracked []string `json:"include_untracked"`
	Exclude          []string `json:"exclude"`
}

func ReadProjectOptions(root string) (ProjectOptions, error) {
	var o ProjectOptions
	path := filepath.Join(root, ".pallium", "modules.json")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return o, nil
	}
	if err != nil {
		return o, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64000 {
		return o, fmt.Errorf("invalid .pallium/modules.json")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return o, err
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return o, err
	}
	for _, list := range [][]string{o.Boundaries, o.IncludeUntracked, o.Exclude} {
		for _, p := range list {
			if p == "" || filepath.IsAbs(p) || filepath.ToSlash(filepath.Clean(p)) != p || p == ".." || strings.HasPrefix(p, "../") {
				return o, fmt.Errorf("invalid project source path %q", p)
			}
		}
	}
	return o, nil
}
func (o ProjectOptions) Excluded(path string) bool {
	for _, p := range o.Exclude {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// SourcePaths differs deliberately from the historical TrackedFiles helper,
// which also lists all untracked files. Knowledge only admits explicit opt-ins.
func SourcePaths(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--cached")
	b, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	o, err := ReadProjectOptions(root)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var paths []string
	for _, p := range append(strings.Split(string(b), "\x00"), o.IncludeUntracked...) {
		if p != "" && !seen[p] && !o.Excluded(p) {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths, nil
}
