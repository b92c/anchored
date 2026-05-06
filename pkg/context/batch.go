package ctx

import (
	"context"
	"log/slog"
	"strings"
	"sync"
)

const largeOutputThreshold = 5 * 1024 // 5 KB
const maxBatchConcurrency = 8

// BatchExecutor runs multiple sandbox commands sequentially, indexes combined
// output, and optionally searches the indexed content.
type BatchExecutor struct {
	sandbox  *Sandbox
	indexer  *Indexer
	searcher *Searcher
	logger   *slog.Logger
}

// NewBatchExecutor creates a BatchExecutor. If logger is nil, slog.Default() is used.
func NewBatchExecutor(sandbox *Sandbox, indexer *Indexer, searcher *Searcher, logger *slog.Logger) *BatchExecutor {
	if logger == nil {
		logger = slog.Default()
	}
	return &BatchExecutor{
		sandbox:  sandbox,
		indexer:  indexer,
		searcher: searcher,
		logger:   logger,
	}
}

// ExecuteBatch runs commands and indexes all combined output, optionally
// searching the indexed content.
//
// concurrency=1 (or <=0) runs commands sequentially. concurrency>1 fans them
// out across that many workers (capped at maxBatchConcurrency=8). Order of
// `results` always matches input order regardless of concurrency.
//
// A failing command (non-zero exit, timeout) is recorded in results but does
// NOT abort the batch.
//
// If queries is non-empty, each query is run against the Searcher with
// MaxResults=5 and results are deduplicated by ChunkID.
//
// If the combined output exceeds 5 KB and intent is non-empty, all output is
// indexed but only search results matching intent terms are returned.
func (be *BatchExecutor) ExecuteBatch(ctx context.Context, commands []BatchCommand, queries []string, sessionID string, intent string, projectID string, concurrency int) (*BatchResult, error) {
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > maxBatchConcurrency {
		concurrency = maxBatchConcurrency
	}

	results := make([]ExecuteResult, len(commands))
	be.runCommands(ctx, commands, results, concurrency)

	var totalBytes int64
	var combined strings.Builder
	for i, res := range results {
		totalBytes += int64(len(res.Stdout))
		if res.Stdout != "" {
			if combined.Len() > 0 {
				combined.WriteByte('\n')
			}
			combined.WriteString(res.Stdout)
		}
		if res.ExitCode != 0 || res.TimedOut {
			label := ""
			if i < len(commands) {
				label = commands[i].Label
			}
			be.logger.Warn("batch command failed",
				"label", label,
				"exit_code", res.ExitCode,
				"timed_out", res.TimedOut,
			)
		}
	}

	sourceID := ""
	if combined.Len() > 0 {
		id, err := be.indexer.IndexRaw(ctx, combined.String(), "batch", "batch-output", sessionID, projectID)
		if err != nil {
			be.logger.Error("batch index error", "error", err)
			return nil, err
		}
		sourceID = id
	}

	var searchResults []ContentSearchResult
	if len(queries) > 0 && be.searcher != nil {
		seen := make(map[string]bool)
		for _, q := range queries {
			if q == "" {
				continue
			}
			hits, err := be.searcher.Search(ctx, q, SearchOpts{MaxResults: 5, ProjectID: projectID})
			if err != nil {
				be.logger.Warn("batch search error", "query", q, "error", err)
				continue
			}
			for _, hit := range hits {
				if seen[hit.ChunkID] {
					continue
				}
				seen[hit.ChunkID] = true

				if combined.Len() > largeOutputThreshold && intent != "" {
					if !matchesIntent(hit.Snippet, intent) {
						continue
					}
				}

				searchResults = append(searchResults, hit)
			}
		}
	}

	return &BatchResult{
		Results:       results,
		SearchResults: searchResults,
		SourceID:      sourceID,
		TotalBytes:    totalBytes,
	}, nil
}

// runCommands executes the batch with the requested concurrency and writes
// each command's outcome at its original index in `results` so callers always
// see input order.
func (be *BatchExecutor) runCommands(ctx context.Context, commands []BatchCommand, results []ExecuteResult, concurrency int) {
	if concurrency <= 1 {
		for i, cmd := range commands {
			results[i] = be.runOne(ctx, cmd)
		}
		return
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, cmd := range commands {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, c BatchCommand) {
			defer wg.Done()
			defer func() { <-sem }()
			results[idx] = be.runOne(ctx, c)
		}(i, cmd)
	}
	wg.Wait()
}

// runOne executes a single batch command and converts infrastructure errors
// into a synthetic ExecuteResult so the caller sees a uniform shape.
func (be *BatchExecutor) runOne(ctx context.Context, cmd BatchCommand) ExecuteResult {
	res, err := be.sandbox.Execute(ctx, cmd.Language, cmd.Command)
	if err != nil {
		be.logger.Error("batch command infrastructure error",
			"label", cmd.Label,
			"error", err,
		)
		return ExecuteResult{
			Stdout:   "",
			Stderr:   err.Error(),
			ExitCode: 1,
			TimedOut: false,
		}
	}
	return *res
}

// matchesIntent returns true if the snippet contains at least one term from
// the intent string.
func matchesIntent(snippet string, intent string) bool {
	snippetLower := strings.ToLower(snippet)
	for _, term := range strings.Fields(strings.ToLower(intent)) {
		if strings.Contains(snippetLower, term) {
			return true
		}
	}
	return false
}
