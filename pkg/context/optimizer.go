//go:build !windows

package ctx

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
)

// Optimizer is the facade that MCP tools call. It composes all internal
// components (Store, Sandbox, Chunker, Searcher, Indexer, Fetcher,
// BatchExecutor, Evictor) and exposes a clean public API.
type Optimizer struct {
	store    *Store
	sandbox  *Sandbox
	chunker  *Chunker
	searcher *Searcher
	indexer  *Indexer
	fetcher  *Fetcher
	batch    *BatchExecutor
	evictor  *Evictor
	logger   *slog.Logger
	cfg      config.ContextOptimizerConfig
}

// NewOptimizer creates all internal components, wires them together, and
// starts the background evictor goroutine.
func NewOptimizer(db *sql.DB, cfg config.ContextOptimizerConfig, logger *slog.Logger) (*Optimizer, error) {
	if logger == nil {
		logger = slog.Default()
	}

	store := NewStore(db, logger)
	if err := store.PrepareStatements(); err != nil {
		return nil, fmt.Errorf("prepare statements: %w", err)
	}

	chunker := NewChunker(4096)

	sandboxTimeout := time.Duration(cfg.SandboxTimeout) * time.Second
	if sandboxTimeout <= 0 {
		sandboxTimeout = 30 * time.Second
	}
	maxOutputBytes := int64(cfg.MaxOutputKB) * 1024
	if maxOutputBytes <= 0 {
		maxOutputBytes = 1 << 20
	}
	sandbox := NewSandbox(sandboxTimeout, maxOutputBytes, "")

	searcher := NewSearcher(store, logger)

	indexer := NewIndexer(store, chunker, db, cfg.DefaultTTL, logger)

	fetchCacheTTL := time.Duration(cfg.FetchCacheTTL) * time.Hour
	if fetchCacheTTL <= 0 {
		fetchCacheTTL = 24 * time.Hour
	}
	fetcher := NewFetcher(30*time.Second, fetchCacheTTL, logger)

	batch := NewBatchExecutor(sandbox, indexer, searcher, logger)

	lruCapBytes := int64(cfg.LRUCapMB) * 1024 * 1024
	evictor := NewEvictor(store, db, EvictorConfig{
		TTLDefaultHours:  cfg.DefaultTTL,
		LRUCapBytes:      lruCapBytes,
		EvictionInterval: 10 * time.Minute,
	}, logger)

	o := &Optimizer{
		store:    store,
		sandbox:  sandbox,
		chunker:  chunker,
		searcher: searcher,
		indexer:  indexer,
		fetcher:  fetcher,
		batch:    batch,
		evictor:  evictor,
		logger:   logger,
		cfg:      cfg,
	}

	ctx := context.Background()
	o.evictor.Start(ctx)

	return o, nil
}

// Execute runs code in the sandbox. If timeoutSec > 0, a child context with
// that timeout is created; otherwise the config default is used (already baked
// into the Sandbox).
func (o *Optimizer) Execute(ctx context.Context, code string, language string, timeoutSec int) (*ExecuteResult, error) {
	if timeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()
	}
	return o.sandbox.Execute(ctx, language, code)
}

// ExecuteFile runs user code in the sandbox after injecting a language-specific
// prelude that exposes FILE_PATH (and FILE_CONTENT for languages where reading
// is idiomatic). The user's code is concatenated after the prelude so it can
// reference these variables directly without doing its own I/O.
//
// For PHP the prelude is wrapped in <?php tags, so the user code must contain
// its own <?php opening tag if it expects to write PHP — same constraint as
// any other PHP CLI runner.
func (o *Optimizer) ExecuteFile(ctx context.Context, path string, language string, code string) (*ExecuteResult, error) {
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("file: %w", err)
	}

	prelude := FilePrelude(language, abs)
	o.logger.Debug("execute file", "path", abs, "language", language, "preludeBytes", len(prelude))

	return o.sandbox.Execute(ctx, language, prelude+code)
}

// IndexContent chunks markdown content and indexes it. Returns a sourceGroupID.
func (o *Optimizer) IndexContent(ctx context.Context, content string, source string, label string, contentType string, projectID string) (string, error) {
	return o.indexer.IndexContent(ctx, content, source, label, "", contentType, projectID)
}

// IndexRaw indexes non-markdown content. Returns a sourceGroupID.
func (o *Optimizer) IndexRaw(ctx context.Context, content string, source string, label string, projectID string) (string, error) {
	return o.indexer.IndexRaw(ctx, content, source, label, "", projectID)
}

// Search searches indexed content.
func (o *Optimizer) Search(ctx context.Context, query string, maxResults int, contentType string, source string, projectID string) ([]ContentSearchResult, error) {
	return o.searcher.Search(ctx, query, SearchOpts{
		MaxResults:  maxResults,
		ContentType: contentType,
		Source:      source,
		ProjectID:   projectID,
	})
}

// FetchAndIndex fetches a URL, converts to markdown, and indexes the content.
// If force is true, the URL's cache entry is invalidated first.
func (o *Optimizer) FetchAndIndex(ctx context.Context, url string, source string, projectID string, force bool) (*FetchResult, error) {
	if force {
		o.fetcher.Invalidate(url)
	}

	result, err := o.fetcher.FetchAndConvert(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}

	if _, err := o.indexer.IndexContent(ctx, result.Markdown, source, url, "", "prose", projectID); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}

	return result, nil
}

// FetchAndIndexBatch fetches multiple URLs, converts each to markdown, and
// indexes them. concurrency=1 runs sequentially; concurrency>1 fans out across
// up to maxBatchConcurrency workers. Per-URL failures are reported in the
// returned entries (Error field) rather than aborting the batch.
func (o *Optimizer) FetchAndIndexBatch(ctx context.Context, requests []FetchRequest, concurrency int, projectID string, force bool) (*FetchBatchResult, error) {
	if len(requests) == 0 {
		return &FetchBatchResult{Entries: []FetchBatchEntry{}}, nil
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > maxBatchConcurrency {
		concurrency = maxBatchConcurrency
	}

	entries := make([]FetchBatchEntry, len(requests))
	run := func(idx int, req FetchRequest) {
		source := req.Source
		if source == "" {
			source = req.URL
		}
		entry := FetchBatchEntry{URL: req.URL, Source: source}
		if force {
			o.fetcher.Invalidate(req.URL)
		}
		result, err := o.fetcher.FetchAndConvert(ctx, req.URL)
		if err != nil {
			entry.Error = err.Error()
			entries[idx] = entry
			return
		}
		if _, ierr := o.indexer.IndexContent(ctx, result.Markdown, source, req.URL, "", "prose", projectID); ierr != nil {
			entry.Error = fmt.Errorf("index: %w", ierr).Error()
			entries[idx] = entry
			return
		}
		entry.Bytes = len(result.Markdown)
		entry.FetchedAt = result.FetchedAt.Format(time.RFC3339)
		entry.FromCache = result.FromCache
		entries[idx] = entry
	}

	if concurrency == 1 {
		for i, r := range requests {
			run(i, r)
		}
	} else {
		sem := make(chan struct{}, concurrency)
		var wg sync.WaitGroup
		for i, r := range requests {
			wg.Add(1)
			sem <- struct{}{}
			go func(idx int, req FetchRequest) {
				defer wg.Done()
				defer func() { <-sem }()
				run(idx, req)
			}(i, r)
		}
		wg.Wait()
	}

	return &FetchBatchResult{Entries: entries}, nil
}

// ExecuteBatch runs multiple commands and optionally searches indexed output.
// concurrency=1 (default) runs sequentially; concurrency>1 fans out across up
// to maxBatchConcurrency workers. Order of `Results` is preserved.
func (o *Optimizer) ExecuteBatch(ctx context.Context, commands []BatchCommand, queries []string, intent string, projectID string, concurrency int) (*BatchResult, error) {
	return o.batch.ExecuteBatch(ctx, commands, queries, "", intent, projectID, concurrency)
}

// Store exposes the underlying Store for session event operations.
func (o *Optimizer) Store() *Store {
	return o.store
}

// Close stops the background evictor. Safe to call multiple times.
func (o *Optimizer) Close() {
	o.evictor.Close()
}
