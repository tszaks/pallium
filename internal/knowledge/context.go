package knowledge

import (
	"encoding/json"
	"github.com/tszaks/pallium/internal/db"
	"strings"
)

type ContextPack struct {
	Version   int      `json:"version"`
	Task      string   `json:"task"`
	Results   []Hit    `json:"results"`
	ReadNext  []string `json:"read_next"`
	Tests     []string `json:"related_tests"`
	Checks    []string `json:"suggested_checks"`
	Guidance  string   `json:"guidance"`
	Truncated bool     `json:"truncated"`
}

func Context(store *db.Store, id int64, task string) (ContextPack, error) {
	search, err := Search(store, id, task, 5, 8192)
	if err != nil {
		return ContextPack{}, err
	}
	pack := ContextPack{Version: ContractVersion, Task: search.Query, Results: search.Results, ReadNext: []string{}, Tests: []string{}, Checks: []string{}, Guidance: "Read source before edits. Structural facts may be heuristic; an audit is a bounded model assessment. Authored decisions express intent and may differ from implementation.", Truncated: search.Truncated}
	seen := map[string]bool{}
	for _, hit := range search.Results {
		for _, path := range hit.Paths {
			if !seen[path] {
				seen[path] = true
				pack.ReadNext = append(pack.ReadNext, path)
			}
		}
	}
	modules, err := Modules(store, id, ModuleOptions{})
	if err != nil {
		return pack, err
	}
	for _, m := range modules {
		for _, hit := range search.Results {
			if m.Slug != hit.Slug {
				continue
			}
			for _, path := range m.Files {
				if isTestPath(path) && len(pack.Tests) < 20 {
					pack.Tests = append(pack.Tests, path)
				}
			}
			for _, lang := range m.Languages {
				if lang == "go" && !seen["go-check"] {
					pack.Checks = append(pack.Checks, "go test ./...", "go vet ./...")
					seen["go-check"] = true
				}
			}
		}
	}
	for {
		b, _ := json.Marshal(pack)
		if len(b) <= 12000 {
			break
		}
		pack.Truncated = true
		if len(pack.Tests) > 0 {
			pack.Tests = pack.Tests[:len(pack.Tests)-1]
		} else if len(pack.ReadNext) > 0 {
			pack.ReadNext = pack.ReadNext[:len(pack.ReadNext)-1]
		} else if len(pack.Results) > 0 {
			pack.Results = pack.Results[:len(pack.Results)-1]
		} else {
			break
		}
	}

	return pack, nil
}
func Section(body, heading string) string {
	lines := strings.Split(body, "\n")
	var out []string
	active := false
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			if active {
				break
			}
			active = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(line, "## ")), heading)
		}
		if active {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
