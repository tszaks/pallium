package codexsessions

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	CompletionFinished    = "finished"
	CompletionNotFinished = "not_finished"
	CompletionUnknown     = "unknown"
)

var relativeDurationPattern = regexp.MustCompile(`\b(\d+|one|two|three|four|five|six|seven|eight|nine|ten)(?:\s+or\s+(\d+|one|two|three|four|five|six|seven|eight|nine|ten))?\s+(minute|hour|day)s?\b`)

type SessionFindOptions struct {
	Query          string
	States         []string
	Completion     string
	UpdatedWithin  time.Duration
	InactiveFor    time.Duration
	FinishedWithin time.Duration
	Sort           string
	Limit          int
}

type SessionFindReport struct {
	GeneratedAt    time.Time          `json:"generated_at"`
	Host           string             `json:"host"`
	Query          string             `json:"query,omitempty"`
	Interpretation string             `json:"interpretation"`
	Sort           string             `json:"sort"`
	Filters        SessionFindFilters `json:"filters"`
	TotalMatches   int                `json:"total_matches"`
	Matches        []SessionSummary   `json:"matches"`
	Coverage       DiscoveryCoverage  `json:"coverage"`
	Warnings       []string           `json:"warnings"`
}

type SessionFindFilters struct {
	States           []string `json:"states,omitempty"`
	Completion       string   `json:"completion,omitempty"`
	ActivityAgeMin   string   `json:"activity_age_min,omitempty"`
	ActivityAgeMax   string   `json:"activity_age_max,omitempty"`
	CompletionAgeMin string   `json:"completion_age_min,omitempty"`
	CompletionAgeMax string   `json:"completion_age_max,omitempty"`
}

type findCriteria struct {
	states           map[string]bool
	completion       string
	activityAgeMin   time.Duration
	activityAgeMax   time.Duration
	completionAgeMin time.Duration
	completionAgeMax time.Duration
	sort             string
	interpretation   []string
}

func FindSessions(snapshot *SessionSnapshot, opts SessionFindOptions) (*SessionFindReport, error) {
	if snapshot == nil {
		return nil, fmt.Errorf("session snapshot is required")
	}
	criteria, err := interpretSessionQuery(opts.Query)
	if err != nil {
		return nil, err
	}
	if len(opts.States) > 0 {
		criteria.states = map[string]bool{}
		for _, raw := range opts.States {
			for _, state := range strings.Split(raw, ",") {
				state = strings.ToLower(strings.TrimSpace(state))
				if state == "" {
					continue
				}
				if !validFindState(state) {
					return nil, fmt.Errorf("invalid session state %q", state)
				}
				criteria.states[state] = true
			}
		}
		criteria.interpretation = append(criteria.interpretation, "state is "+strings.Join(sortedStateKeys(criteria.states), " or "))
	}
	if opts.Completion != "" {
		completion := strings.ToLower(strings.TrimSpace(opts.Completion))
		if !validCompletion(completion) {
			return nil, fmt.Errorf("invalid completion %q; use finished, not_finished, or unknown", completion)
		}
		criteria.completion = completion
		criteria.interpretation = append(criteria.interpretation, "completion is "+completion)
	}
	if opts.UpdatedWithin > 0 {
		criteria.activityAgeMax = opts.UpdatedWithin
		criteria.interpretation = append(criteria.interpretation, "updated within "+opts.UpdatedWithin.String())
	}
	if opts.InactiveFor > 0 {
		criteria.activityAgeMin = opts.InactiveFor
		criteria.interpretation = append(criteria.interpretation, "inactive for at least "+opts.InactiveFor.String())
	}
	if opts.FinishedWithin > 0 {
		criteria.completion = CompletionFinished
		criteria.completionAgeMax = opts.FinishedWithin
		criteria.interpretation = append(criteria.interpretation, "finished within "+opts.FinishedWithin.String())
	}
	if opts.Sort != "" {
		criteria.sort = strings.ToLower(strings.TrimSpace(opts.Sort))
	}
	if criteria.sort == "" {
		criteria.sort = "updated"
	}
	if criteria.sort != "updated" && criteria.sort != "finished" && criteria.sort != "status" {
		return nil, fmt.Errorf("invalid session sort %q; use updated, finished, or status", criteria.sort)
	}

	matches := make([]SessionSummary, 0, len(snapshot.Sessions))
	for _, session := range snapshot.Sessions {
		if session.CompletionStatus == "" {
			session.CompletionStatus = CompletionUnknown
		}
		if sessionMatchesFind(session, criteria, snapshot.GeneratedAt) {
			matches = append(matches, session)
		}
	}
	sortFoundSessions(matches, criteria.sort)
	total := len(matches)
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	if len(matches) > limit {
		matches = matches[:limit]
	}
	interpretation := "all sessions ordered by most recently updated"
	if len(criteria.interpretation) > 0 {
		interpretation = strings.Join(uniqueStrings(criteria.interpretation), "; ")
	}
	warnings := append([]string(nil), snapshot.Warnings...)
	if strings.TrimSpace(opts.Query) != "" && len(criteria.interpretation) == 0 {
		warnings = append(warnings, "No supported completion or time phrase was recognized; returning all sessions ordered by most recent update.")
	}
	return &SessionFindReport{
		GeneratedAt:    snapshot.GeneratedAt,
		Host:           snapshot.Host,
		Query:          strings.TrimSpace(opts.Query),
		Interpretation: interpretation,
		Sort:           criteria.sort,
		Filters:        criteriaFilters(criteria),
		TotalMatches:   total,
		Matches:        matches,
		Coverage:       snapshot.Coverage,
		Warnings:       warnings,
	}, nil
}

func interpretSessionQuery(query string) (findCriteria, error) {
	lower := strings.ToLower(strings.TrimSpace(query))
	criteria := findCriteria{sort: "updated"}
	if lower == "" {
		return criteria, nil
	}

	unfinished := containsAnyText(lower, "hasn't wrapped", "has not wrapped", "not wrapped", "hasn't finished", "has not finished", "not finished", "unfinished")
	finished := !unfinished && containsAnyText(lower, "wrapped up", "finished", "completed")
	if unfinished {
		criteria.completion = CompletionNotFinished
		criteria.interpretation = append(criteria.interpretation, "completion is not_finished")
	} else if finished {
		criteria.completion = CompletionFinished
		criteria.sort = "finished"
		criteria.interpretation = append(criteria.interpretation, "completion is finished")
	}

	ageMin, ageMax, foundDuration := naturalDurationRange(lower)
	inactivity := containsAnyText(lower, "hasn't been touched", "has not been touched", "not been touched", "not touched", "inactive for", "untouched")
	if inactivity && foundDuration {
		criteria.activityAgeMin = ageMin
		criteria.activityAgeMax = ageMax
		criteria.interpretation = append(criteria.interpretation, durationRangeDescription("last activity age", ageMin, ageMax))
	} else if finished && foundDuration {
		criteria.completionAgeMin = ageMin
		criteria.completionAgeMax = ageMax
		criteria.interpretation = append(criteria.interpretation, durationRangeDescription("completion age", ageMin, ageMax))
	} else if containsAnyText(lower, "updated within", "touched within", "active within") && foundDuration {
		criteria.activityAgeMax = ageMax
		criteria.interpretation = append(criteria.interpretation, "updated within "+ageMax.String())
	}

	if finished && !foundDuration && strings.Contains(lower, "just") {
		criteria.completionAgeMax = 5 * time.Minute
		criteria.interpretation = append(criteria.interpretation, "completion age is at most 5m")
	}
	if containsAnyText(lower, "most recent", "latest", "updated most recently", "most recently updated") {
		criteria.sort = "updated"
		criteria.interpretation = append(criteria.interpretation, "ordered by most recently updated")
	}
	return criteria, nil
}

func naturalDurationRange(value string) (time.Duration, time.Duration, bool) {
	if strings.Contains(value, "few minutes") {
		return 0, 10 * time.Minute, true
	}
	match := relativeDurationPattern.FindStringSubmatch(value)
	if len(match) == 0 {
		return 0, 0, false
	}
	first, ok := naturalNumber(match[1])
	if !ok {
		return 0, 0, false
	}
	unit := time.Minute
	switch match[3] {
	case "hour":
		unit = time.Hour
	case "day":
		unit = 24 * time.Hour
	}
	if match[2] != "" {
		second, ok := naturalNumber(match[2])
		if !ok {
			return 0, 0, false
		}
		if first > second {
			first, second = second, first
		}
		return time.Duration(first) * unit, time.Duration(second) * unit, true
	}
	age := time.Duration(first) * unit
	if strings.Contains(value, "ago") {
		spread := unit
		minAge := max(time.Duration(0), age-spread)
		return minAge, age + spread, true
	}
	return age, 0, true
}

func naturalNumber(value string) (int, bool) {
	if number, err := strconv.Atoi(value); err == nil {
		return number, number >= 0
	}
	numbers := map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10}
	number, ok := numbers[value]
	return number, ok
}

func sessionMatchesFind(session SessionSummary, criteria findCriteria, now time.Time) bool {
	if len(criteria.states) > 0 && !criteria.states[session.Status] {
		return false
	}
	if criteria.completion != "" && session.CompletionStatus != criteria.completion {
		return false
	}
	if criteria.activityAgeMin > 0 || criteria.activityAgeMax > 0 {
		if session.LastActiveAt.IsZero() {
			return false
		}
		age := nonNegativeAge(now, session.LastActiveAt)
		if criteria.activityAgeMin > 0 && age < criteria.activityAgeMin {
			return false
		}
		if criteria.activityAgeMax > 0 && age > criteria.activityAgeMax {
			return false
		}
	}
	if criteria.completionAgeMin > 0 || criteria.completionAgeMax > 0 {
		if session.CompletedAt == nil {
			return false
		}
		age := nonNegativeAge(now, *session.CompletedAt)
		if criteria.completionAgeMin > 0 && age < criteria.completionAgeMin {
			return false
		}
		if criteria.completionAgeMax > 0 && age > criteria.completionAgeMax {
			return false
		}
	}
	return true
}

func sortFoundSessions(sessions []SessionSummary, order string) {
	sort.SliceStable(sessions, func(i, j int) bool {
		left, right := sessions[i], sessions[j]
		switch order {
		case "finished":
			leftAt, rightAt := time.Time{}, time.Time{}
			if left.CompletedAt != nil {
				leftAt = *left.CompletedAt
			}
			if right.CompletedAt != nil {
				rightAt = *right.CompletedAt
			}
			if !leftAt.Equal(rightAt) {
				return leftAt.After(rightAt)
			}
		case "status":
			if left.Status != right.Status {
				return liveStatusRank(left.Status) < liveStatusRank(right.Status)
			}
		}
		if !left.LastActiveAt.Equal(right.LastActiveAt) {
			return left.LastActiveAt.After(right.LastActiveAt)
		}
		return left.ThreadID < right.ThreadID
	})
}

func criteriaFilters(criteria findCriteria) SessionFindFilters {
	filters := SessionFindFilters{States: sortedStateKeys(criteria.states), Completion: criteria.completion}
	if criteria.activityAgeMin > 0 {
		filters.ActivityAgeMin = criteria.activityAgeMin.String()
	}
	if criteria.activityAgeMax > 0 {
		filters.ActivityAgeMax = criteria.activityAgeMax.String()
	}
	if criteria.completionAgeMin > 0 {
		filters.CompletionAgeMin = criteria.completionAgeMin.String()
	}
	if criteria.completionAgeMax > 0 {
		filters.CompletionAgeMax = criteria.completionAgeMax.String()
	}
	return filters
}

func validFindState(state string) bool {
	switch state {
	case activeSessionStatus, waitingSessionStatus, blockedSessionStatus, stuckSessionStatus, finishedSessionStatus, idleSessionStatus, inactiveSessionStatus:
		return true
	default:
		return false
	}
}

func validCompletion(completion string) bool {
	return completion == CompletionFinished || completion == CompletionNotFinished || completion == CompletionUnknown
}

func sortedStateKeys(states map[string]bool) []string {
	result := make([]string, 0, len(states))
	for state := range states {
		result = append(result, state)
	}
	sort.Strings(result)
	return result
}

func durationRangeDescription(label string, minAge, maxAge time.Duration) string {
	switch {
	case minAge > 0 && maxAge > 0:
		return fmt.Sprintf("%s is between %s and %s", label, minAge, maxAge)
	case minAge > 0:
		return fmt.Sprintf("%s is at least %s", label, minAge)
	default:
		return fmt.Sprintf("%s is at most %s", label, maxAge)
	}
}

func nonNegativeAge(now, timestamp time.Time) time.Duration {
	if timestamp.After(now) {
		return 0
	}
	return now.Sub(timestamp)
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func containsAnyText(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
