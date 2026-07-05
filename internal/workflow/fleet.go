package workflow

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func MaxActiveRunsFromEnv() int {
	raw := strings.TrimSpace(os.Getenv("PALLIUM_WORKFLOW_MAX_ACTIVE_RUNS"))
	if raw == "" {
		return 0
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 0 {
		return 0
	}
	return limit
}

func CheckActiveRunCapacity(store *Store, limit int) error {
	if store == nil {
		return fmt.Errorf("workflow store is required")
	}
	if limit <= 0 {
		return nil
	}
	active, err := store.CountActiveRuns()
	if err != nil {
		return err
	}
	if active >= limit {
		return fmt.Errorf("workflow fleet limit reached: %d active runs (max %d)", active, limit)
	}
	return nil
}