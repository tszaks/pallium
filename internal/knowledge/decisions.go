package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/db"
)

// LinkDecision imports only explicitly supplied text. Session history is never
// swept into project knowledge. Authored intent remains distinct from code facts.
func LinkDecision(store *db.Store, id int64, title, body, source, supersedes string) (db.KnowledgeDoc, error) {
	if strings.TrimSpace(title) == "" || strings.TrimSpace(body) == "" || strings.TrimSpace(source) == "" {
		return db.KnowledgeDoc{}, fmt.Errorf("title, decision text and source reference are required")
	}
	if len(body) > 64000 || len(title) > 300 || len(source) > 2000 {
		return db.KnowledgeDoc{}, fmt.Errorf("decision exceeds size limit")
	}
	sum := sha256.Sum256([]byte(title + "\x00" + body + "\x00" + source))
	doc := db.KnowledgeDoc{Slug: "decision-" + hex.EncodeToString(sum[:])[:16], Kind: "decision", Title: title, Summary: truncate(body, 700), Body: body, Generator: "authored", GeneratedAt: time.Now().UTC(), Evidence: db.Evidence{State: "authored", SourceRef: source, Supersedes: supersedes}}
	err := store.WithTx(func(tx *db.Store) error {
		docs, err := tx.KnowledgeDocs(id)
		if err != nil {
			return err
		}
		found := supersedes == ""
		for _, old := range docs {
			if old.Kind != "decision" || old.Slug == doc.Slug {
				continue
			}
			if old.Slug == supersedes {
				found = true
				old.Evidence.State = "superseded"
				if err := tx.UpsertKnowledgeDoc(id, old); err != nil {
					return err
				}
				continue
			}
			if strings.EqualFold(old.Title, title) && old.Evidence.State != "superseded" {
				old.Evidence.State = "conflicting_intent"
				doc.Evidence.State = "conflicting_intent"
				if err := tx.UpsertKnowledgeDoc(id, old); err != nil {
					return err
				}
			}
		}
		if !found {
			return fmt.Errorf("superseded decision does not exist")
		}
		return tx.UpsertKnowledgeDoc(id, doc)
	})
	return doc, err
}
