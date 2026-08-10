package sessionmemory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type SyncOptions struct {
	Index      Options
	Include    []string
	NoEmbed    bool
	EmbedLimit int
	BatchSize  int
}

type SyncProgress struct {
	Phase     string `json:"phase"`
	Completed int    `json:"completed"`
	Total     int    `json:"total"`
	Message   string `json:"message"`
}

type SyncReport struct {
	Indexed          int    `json:"indexed"`
	Embedded         int    `json:"embedded"`
	EmbeddingBacklog int    `json:"embedding_backlog"`
	EmbeddingSkipped bool   `json:"embedding_skipped"`
	EmbeddingWarning string `json:"embedding_warning,omitempty"`
	FullReindex      bool   `json:"full_reindex"`
	LegacyBackfilled int    `json:"legacy_backfilled"`
	Stats            Stats  `json:"stats"`
	Duration         string `json:"duration"`
}

func Sync(ctx context.Context, opts SyncOptions, progress func(SyncProgress)) (SyncReport, error) {
	started := time.Now()
	report := SyncReport{FullReindex: opts.Index.Force}
	if opts.EmbedLimit <= 0 {
		opts.EmbedLimit = 1_000_000
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 64
	}
	if !opts.Index.Force {
		doctor, err := DoctorSessions(SessionDoctorOptions{DBPath: opts.Index.DBPath})
		if err == nil && doctor.DBExists {
			if (doctor.NoisyTitles > 0 || doctor.OutdatedCapsules > 0) && doctor.OversizedFirstMessages == 0 && doctor.SkippedLargeSessions == 0 && doctor.MissingCapsules == 0 {
				store, openErr := Open(opts.Index.DBPath)
				if openErr != nil {
					return report, openErr
				}
				rebuilt, rebuildErr := store.backfillLegacySessions()
				closeErr := store.Close()
				if rebuildErr != nil {
					return report, rebuildErr
				}
				if closeErr != nil {
					return report, closeErr
				}
				report.LegacyBackfilled += rebuilt
				doctor, err = DoctorSessions(SessionDoctorOptions{DBPath: opts.Index.DBPath})
			}
			if err == nil && (doctor.NoisyTitles > 0 || doctor.OversizedFirstMessages > 0 || doctor.SkippedLargeSessions > 0 || doctor.MissingCapsules > 0 || doctor.OutdatedCapsules > 0) {
				opts.Index.Force = true
				report.FullReindex = true
			}
		}
	}
	if progress != nil {
		progress(SyncProgress{Phase: "index", Message: "Indexing changed session transcripts."})
	}
	indexed, err := Index(ctx, opts.Index, opts.Include)
	if err != nil {
		return report, err
	}
	report.Indexed = indexed
	store, err := Open(opts.Index.DBPath)
	if err != nil {
		return report, err
	}
	rebuilt, err := store.backfillLegacySessions()
	report.LegacyBackfilled += rebuilt
	closeErr := store.Close()
	if err != nil {
		return report, err
	}
	if closeErr != nil {
		return report, closeErr
	}
	model := resolveEmbeddingModel(opts.Index.EmbeddingModel)
	backlog, err := EmbeddingBacklogPath(opts.Index.DBPath, model)
	if err != nil {
		return report, err
	}
	if opts.NoEmbed || backlog == 0 {
		report.EmbeddingSkipped = opts.NoEmbed
	} else if ready, reason := embeddingReady(); !ready {
		report.EmbeddingSkipped = true
		report.EmbeddingWarning = reason
	} else {
		if progress != nil {
			progress(SyncProgress{Phase: "embed", Total: min(backlog, opts.EmbedLimit), Message: "Embedding current continuity chunks."})
		}
		report.Embedded, err = EmbedSessionPath(ctx, opts.Index.DBPath, "", model, opts.EmbedLimit, opts.BatchSize, func(completed, total int) {
			if progress != nil {
				progress(SyncProgress{Phase: "embed", Completed: completed, Total: total, Message: "Embedding current continuity chunks."})
			}
		})
		if err != nil {
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			report.EmbeddingWarning = short(err.Error(), 500)
		}
	}
	report.EmbeddingBacklog, _ = EmbeddingBacklogPath(opts.Index.DBPath, model)
	report.Stats, err = StatsReadPath(opts.Index.DBPath)
	if err != nil {
		return report, err
	}
	report.Duration = time.Since(started).Round(time.Millisecond).String()
	if progress != nil {
		progress(SyncProgress{Phase: "complete", Completed: report.Indexed + report.LegacyBackfilled + report.Embedded, Message: fmt.Sprintf("Indexed %d sessions, rebuilt %d legacy sessions, and embedded %d chunks.", report.Indexed, report.LegacyBackfilled, report.Embedded)})
	}
	return report, nil
}

func embeddingReady() (bool, string) {
	settings := resolveEmbeddingSettings()
	if settings.configError != nil {
		return false, "Embedding skipped because the saved configuration is invalid: " + settings.configError.Error()
	}
	if settings.credentialError != nil {
		return false, "Embedding skipped because the saved credential could not be read: " + settings.credentialError.Error()
	}
	if settings.provider == "openai" && strings.Contains(settings.baseURL, "api.openai.com") && settings.apiKey == "" {
		return false, "Embedding skipped because no OpenAI key is configured. Lexical recall remains available; configure PALLIUM_EMBED_API_KEY or a local provider to enable semantic recall."
	}
	return true, ""
}
