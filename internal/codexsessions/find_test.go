package codexsessions

import (
	"testing"
	"time"
)

func TestFindSessionsUnderstandsNaturalRecencyRequests(t *testing.T) {
	now := time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC)
	snapshot := &SessionSnapshot{
		GeneratedAt: now,
		Host:        "test-mac",
		Coverage:    defaultDiscoveryCoverage(),
		Sessions: []SessionSummary{
			{Provider: providerCodex, ThreadID: "finished-two", Title: "finished two minutes ago", Status: finishedSessionStatus, CompletionStatus: CompletionFinished, LastActiveAt: now.Add(-2 * time.Minute), CompletedAt: timePointer(now.Add(-2 * time.Minute))},
			{Provider: providerClaude, ThreadID: "unfinished-ten", Title: "still working", Status: activeSessionStatus, CompletionStatus: CompletionNotFinished, LastActiveAt: now.Add(-10 * time.Minute)},
			{Provider: providerCodex, ThreadID: "untouched-three-half", Title: "quiet unfinished work", Status: inactiveSessionStatus, CompletionStatus: CompletionNotFinished, LastActiveAt: now.Add(-210 * time.Minute)},
			{Provider: providerClaude, ThreadID: "finished-six", Title: "finished six minutes ago", Status: finishedSessionStatus, CompletionStatus: CompletionFinished, LastActiveAt: now.Add(-6 * time.Minute), CompletedAt: timePointer(now.Add(-6 * time.Minute))},
			{Provider: providerCodex, ThreadID: "unknown-newest", Title: "newest unknown", Status: inactiveSessionStatus, CompletionStatus: CompletionUnknown, LastActiveAt: now.Add(-30 * time.Second)},
		},
	}

	tests := []struct {
		query string
		want  []string
	}{
		{query: "This session just wrapped up two minutes ago.", want: []string{"finished-two"}},
		{query: "That session hasn't wrapped up.", want: []string{"unfinished-ten", "untouched-three-half"}},
		{query: "That session hasn't been touched in three or four hours.", want: []string{"untouched-three-half"}},
		{query: "That session finished up a few minutes ago.", want: []string{"finished-two", "finished-six"}},
		{query: "Which sessions were updated most recently?", want: []string{"unknown-newest", "finished-two", "finished-six", "unfinished-ten", "untouched-three-half"}},
	}
	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			report, err := FindSessions(snapshot, SessionFindOptions{Query: test.query, Limit: 20})
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Matches) != len(test.want) {
				t.Fatalf("matches=%v, want ids=%v; interpretation=%s filters=%+v", sessionIDs(report.Matches), test.want, report.Interpretation, report.Filters)
			}
			for index, want := range test.want {
				if report.Matches[index].ThreadID != want {
					t.Fatalf("matches=%v, want ids=%v", sessionIDs(report.Matches), test.want)
				}
			}
			if report.Interpretation == "" {
				t.Fatal("natural-language query must return its explicit interpretation")
			}
		})
	}
}

func TestFindSessionsStructuredFiltersOverrideNaturalQuery(t *testing.T) {
	now := time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC)
	snapshot := &SessionSnapshot{GeneratedAt: now, Sessions: []SessionSummary{
		{ThreadID: "active", Status: activeSessionStatus, CompletionStatus: CompletionNotFinished, LastActiveAt: now.Add(-30 * time.Minute)},
		{ThreadID: "idle", Status: idleSessionStatus, CompletionStatus: CompletionNotFinished, LastActiveAt: now.Add(-4 * time.Hour)},
	}}
	report, err := FindSessions(snapshot, SessionFindOptions{
		Query:       "latest sessions",
		States:      []string{"idle"},
		InactiveFor: 3 * time.Hour,
		Sort:        "status",
		Limit:       10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Matches) != 1 || report.Matches[0].ThreadID != "idle" || report.Sort != "status" {
		t.Fatalf("unexpected structured result: %+v", report)
	}
}

func TestFindSessionsRejectsInvalidFilters(t *testing.T) {
	snapshot := &SessionSnapshot{GeneratedAt: time.Now()}
	if _, err := FindSessions(snapshot, SessionFindOptions{States: []string{"made-up"}}); err == nil {
		t.Fatal("expected invalid state error")
	}
	if _, err := FindSessions(snapshot, SessionFindOptions{Completion: "maybe"}); err == nil {
		t.Fatal("expected invalid completion error")
	}
	if _, err := FindSessions(snapshot, SessionFindOptions{Sort: "random"}); err == nil {
		t.Fatal("expected invalid sort error")
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func sessionIDs(sessions []SessionSummary) []string {
	ids := make([]string, 0, len(sessions))
	for _, session := range sessions {
		ids = append(ids, session.ThreadID)
	}
	return ids
}
