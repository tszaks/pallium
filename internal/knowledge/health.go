package knowledge

import (
	"github.com/tszaks/pallium/internal/codeindex"
	"github.com/tszaks/pallium/internal/db"
	"strings"
)

type Health struct {
	Documents       int `json:"documents"`
	Current         int `json:"current"`
	Stale           int `json:"stale"`
	NeedsRebuild    int `json:"needs_rebuild"`
	Audited         int `json:"audited"`
	IndexedFiles    int `json:"indexed_files"`
	HeuristicFiles  int `json:"heuristic_files"`
	PartialFiles    int `json:"partial_files"`
	OutdatedParsers int `json:"outdated_parsers"`
}

func HealthReport(s *db.Store, id int64) (Health, error) {
	var h Health
	docs, err := s.KnowledgeDocs(id)
	if err != nil {
		return h, err
	}
	if err := Assess(s, docs); err != nil {
		return h, err
	}
	h.Documents = len(docs)
	for _, d := range docs {
		switch d.Freshness {
		case "current":
			h.Current++
		case "stale":
			h.Stale++
		case "needs_rebuild":
			h.NeedsRebuild++
		}
		if d.Evidence.AuditedAt != "" {
			h.Audited++
		}
	}
	states, err := s.CodeFileStates(id)
	if err != nil {
		return h, err
	}
	h.IndexedFiles = len(states)
	for _, state := range states {
		if strings.HasPrefix(state.Parser, "scan") {
			h.HeuristicFiles++
		}
		if strings.Contains(state.Parser, "partial") {
			h.PartialFiles++
		}
		if !strings.Contains(state.Parser, "@"+codeindex.ParserVersion+"#") {
			h.OutdatedParsers++
		}
	}
	return h, nil
}
