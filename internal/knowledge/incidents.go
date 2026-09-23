package knowledge

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tszaks/pallium/internal/db"
)

// incidentSubjects are the words a commit uses when it is cleaning up after
// something that went wrong. Mining these is the cheapest honest version of
// Greptile's scar list: it cannot invent an incident, because every entry is a
// real commit with a real SHA and a real file list.
//
// Deliberately narrow. "fix" alone matches most of a repo's history and would
// produce a list nobody reads; these are the words that mean production hurt.
var incidentSubjects = []string{
	"revert",
	"hotfix",
	"rollback",
	"roll back",
	"regression",
	"outage",
	"incident",
	"postmortem",
	"data loss",
	"emergency",
	"p0",
	"sev1",
	"sev 1",
	"broke prod",
	"broken prod",
}

func buildIncidentDoc(store *db.Store, repoID int64, sourceCommit string) (db.KnowledgeDoc, error) {
	commits, err := store.CommitsMatchingSubjects(repoID, incidentSubjects, 60)
	if err != nil {
		return db.KnowledgeDoc{}, err
	}

	var builder strings.Builder
	builder.WriteString("# Incident candidates and reverts\n\n")
	builder.WriteString("Keyword matches from commit subjects. Each SHA is real; whether it represents an actual incident is unconfirmed.\n\n")

	cited := make([]string, 0)
	if len(commits) == 0 {
		builder.WriteString("No commits in the indexed history announce a revert, rollback, hotfix or outage.\n")
	} else {
		fmt.Fprintf(&builder, "%d commit(s), newest first.\n\n", len(commits))
		for _, commit := range commits {
			files, err := store.FilesForCommit(repoID, commit.SHA)
			if err != nil {
				return db.KnowledgeDoc{}, err
			}
			fmt.Fprintf(&builder, "### %s `%s`\n\n", commit.CommittedAt.Format("2006-01-02"), shortSHA(commit.SHA))
			fmt.Fprintf(&builder, "%s\n\n", strings.TrimSpace(commit.Subject))
			if author := strings.TrimSpace(commit.AuthorName); author != "" {
				fmt.Fprintf(&builder, "Author: %s\n\n", author)
			}
			if body := strings.TrimSpace(commit.Body); body != "" {
				fmt.Fprintf(&builder, "%s\n\n", truncate(body, 400))
			}
			if len(files) > 0 {
				builder.WriteString("Touched:\n")
				for _, path := range capStrings(files, 10) {
					fmt.Fprintf(&builder, "- `%s`\n", path)
				}
				if len(files) > 10 {
					fmt.Fprintf(&builder, "- ...and %d more\n", len(files)-10)
				}
				builder.WriteString("\n")
				cited = append(cited, files...)
			}
		}
	}

	sort.Strings(cited)
	return db.KnowledgeDoc{
		Slug:         "incidents",
		Kind:         "incident",
		Title:        "Incident candidates and reverts",
		Summary:      fmt.Sprintf("%d commit(s) in history announce a revert, rollback, hotfix or outage.", len(commits)),
		Body:         builder.String(),
		CitedPaths:   sortedUnique(cited),
		CitedSymbols: []string{},
		SourceCommit: sourceCommit,
		Generator:    "structural",
		Verified:     true,
		GeneratedAt:  time.Now().UTC(),
	}, nil
}
