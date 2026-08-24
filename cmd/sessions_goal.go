package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/tszaks/pallium/internal/sessionmemory"
)

// goalAttachmentPattern matches goal-objective attachment paths as they
// appear in Codex rollout transcripts, e.g.
// /Users/tyler/.codex/attachments/<uuid>/goal-objective.md
var goalAttachmentPattern = regexp.MustCompile(`[A-Za-z0-9_./~-]*\.codex/attachments/[0-9a-fA-F-]+/[A-Za-z0-9_.-]+\.md`)

type sessionGoalResult struct {
	SessionID       string   `json:"sessionId"`
	RolloutPath     string   `json:"rolloutPath"`
	GoalPath        string   `json:"goalPath"`
	Exists          bool     `json:"exists"`
	SizeBytes       int64    `json:"sizeBytes"`
	ModifiedAt      string   `json:"modifiedAt"`
	OtherCandidates []string `json:"otherCandidates"`
}

func runSessionsGoal(out io.Writer, args []string, jsonOutput bool) error {
	if hasHelpArg(args) {
		printSessionsHelp(out)
		return nil
	}
	fs := newSessionFlagSet("sessions goal")
	pathOnly := fs.Bool("path-only", false, "")
	if err := parseSessionFlags(fs, args, nil, map[string]struct{}{"path-only": {}}); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: pallium sessions goal <session-id>")
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("unexpected sessions goal argument: %s", fs.Arg(1))
	}
	id := fs.Arg(0)

	rolloutPath, resolvedID, err := resolveSessionRolloutPath(id)
	if err != nil {
		return err
	}

	goalPath, otherCandidates, err := findGoalAttachment(rolloutPath)
	if err != nil {
		return err
	}
	if goalPath == "" {
		return fmt.Errorf("no goal file reference found in session %s", id)
	}
	goalPath = expandTilde(goalPath)
	for i, c := range otherCandidates {
		otherCandidates[i] = expandTilde(c)
	}

	result := sessionGoalResult{
		SessionID:       resolvedID,
		RolloutPath:     rolloutPath,
		GoalPath:        goalPath,
		OtherCandidates: otherCandidates,
	}
	if info, statErr := os.Stat(goalPath); statErr == nil {
		result.Exists = true
		result.SizeBytes = info.Size()
		result.ModifiedAt = info.ModTime().Local().Format("2006-01-02T15:04:05Z07:00")
	}

	if *pathOnly {
		_, err := fmt.Fprintln(out, result.GoalPath)
		return err
	}
	if jsonOutput {
		return json.NewEncoder(out).Encode(result)
	}

	fmt.Fprintf(out, "%s\n", result.GoalPath)
	if result.Exists {
		fmt.Fprintf(out, "exists: yes  size: %d bytes  modified: %s\n", result.SizeBytes, result.ModifiedAt)
	} else {
		fmt.Fprintln(out, "exists: no")
	}
	if !result.Exists {
		return nil
	}
	content, err := os.ReadFile(result.GoalPath)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, strings.Repeat("-", 40))
	fmt.Fprintln(out, string(content))
	return nil
}

// resolveSessionRolloutPath resolves a session id (or prefix) to its rollout
// transcript path, mirroring how `sessions show` resolves ids via the
// sessionmemory index, and falling back to a direct filename glob over
// ~/.codex/sessions for sessions that were never indexed.
func resolveSessionRolloutPath(id string) (rolloutPath string, resolvedID string, err error) {
	sess, _, showErr := sessionmemory.Show(id, false)
	if showErr == nil && strings.TrimSpace(sess.RolloutPath) != "" {
		if _, statErr := os.Stat(sess.RolloutPath); statErr == nil {
			return sess.RolloutPath, sess.ID, nil
		}
	}
	matches, globErr := globRolloutFiles(id)
	if globErr != nil {
		return "", "", globErr
	}
	switch len(matches) {
	case 0:
		if showErr != nil {
			return "", "", fmt.Errorf("no session found for %q: %w", id, showErr)
		}
		return "", "", fmt.Errorf("no rollout file found for session %q", id)
	case 1:
		return matches[0], id, nil
	default:
		sort.Strings(matches)
		return matches[len(matches)-1], id, nil
	}
}

func globRolloutFiles(id string) ([]string, error) {
	codexHome := sessionmemory.DefaultCodexHome()
	pattern := filepath.Join(codexHome, "sessions", "*", "*", "*", "*"+id+"*.jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	return matches, nil
}

// findGoalAttachment streams a (potentially very large) rollout JSONL file
// line by line looking for references to a
// .codex/attachments/<uuid>/<name>.md path. It returns the most recently
// referenced path (last occurrence wins) plus any other unique candidates
// seen along the way.
func findGoalAttachment(rolloutPath string) (goalPath string, otherCandidates []string, err error) {
	f, err := os.Open(rolloutPath)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 1<<20)
	seenOrder := []string{}
	seen := map[string]bool{}
	last := ""

	for {
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			matches := goalAttachmentPattern.FindAllString(line, -1)
			for _, m := range matches {
				if !seen[m] {
					seen[m] = true
					seenOrder = append(seenOrder, m)
				}
				last = m
			}
		}
		if readErr != nil {
			break
		}
	}

	if last == "" {
		return "", nil, nil
	}
	for _, m := range seenOrder {
		if m != last {
			otherCandidates = append(otherCandidates, m)
		}
	}
	return last, otherCandidates, nil
}

func expandTilde(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}
