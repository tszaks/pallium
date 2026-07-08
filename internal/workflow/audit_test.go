package workflow

import "testing"

func TestVersionRequirementsCoverV1ThroughV7(t *testing.T) {
	reqs := VersionRequirements()
	seen := map[string]int{}
	for _, req := range reqs {
		seen[req.Version]++
		if req.ID == "" || req.Name == "" || req.Description == "" {
			t.Fatalf("incomplete requirement: %#v", req)
		}
	}
	for _, version := range []string{"v1", "v2", "v3", "v4", "v5", "v6", "v7"} {
		if seen[version] < 2 {
			t.Fatalf("expected multiple requirements for %s, got %d", version, seen[version])
		}
	}
}
