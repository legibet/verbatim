// Package store keeps the SQLite log of requests and the Responses ownership
// table (docs/usage-and-pricing.md, docs/affinity.md), and answers the
// queries behind the CLI and the web UI.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned by the single-row lookups when no row matches.
var ErrNotFound = errors.New("not found")

const (
	defaultQueueSize = 4096
	maxBatch         = 256
	sweepInterval    = time.Hour
)

// migrations are applied in order; PRAGMA user_version records how many ran.
var migrations = []string{
	`CREATE TABLE requests (
		id                    INTEGER PRIMARY KEY AUTOINCREMENT,
		ts                    INTEGER NOT NULL,
		request_id            TEXT    NOT NULL UNIQUE,
		upstream_request_id   TEXT,
		protocol              TEXT    NOT NULL,
		operation             TEXT    NOT NULL,
		client_model          TEXT    NOT NULL,
		route                 TEXT,
		provider              TEXT,
		upstream_model        TEXT,
		status                INTEGER NOT NULL,
		outcome               TEXT    NOT NULL,
		stream                INTEGER NOT NULL,
		attempts              INTEGER NOT NULL,
		attempts_ms           INTEGER NOT NULL,
		ttfb_ms               INTEGER,
		total_ms              INTEGER NOT NULL,
		routing_json          TEXT    NOT NULL,
		attempt_log           TEXT    NOT NULL,
		affinity_key          TEXT,
		input_tokens          INTEGER,
		cache_read_tokens     INTEGER,
		cache_write_tokens    INTEGER,
		cache_write_1h_tokens INTEGER,
		output_tokens         INTEGER,
		reasoning_tokens      INTEGER,
		usage_json            TEXT,
		cost_usd              REAL,
		price_key             TEXT,
		price_json            TEXT,
		error                 TEXT
	);
	CREATE INDEX requests_ts ON requests(ts);
	CREATE INDEX requests_provider_ts ON requests(provider, ts);
	CREATE INDEX requests_client_model_ts ON requests(client_model, ts);
	CREATE TABLE owned_responses (
		id             TEXT PRIMARY KEY,
		provider       TEXT NOT NULL,
		upstream_model TEXT NOT NULL,
		ts             INTEGER NOT NULL
	);
	CREATE INDEX owned_responses_ts ON owned_responses(ts);`,
	`ALTER TABLE requests ADD COLUMN input_image_tokens INTEGER;
	ALTER TABLE requests ADD COLUMN output_image_tokens INTEGER;`,
}

// Store is the request log and ownership table. Rows are written by a single
// goroutine fed through a bounded queue, so recording never delays a request.
type Store struct {
	w         *sql.DB // one connection: the writer goroutine, RecordOwner, and the sweep
	r         *sql.DB // readers
	retention time.Duration

	mu      sync.RWMutex // guards closed against concurrent Record and Close
	closed  bool
	queue   chan *Request
	done    chan struct{}
	dropped atomic.Int64
}

// Open opens or creates the database at path, creating parent directories,
// and starts the writer. Rows older than retention are deleted at start and
// hourly; retention <= 0 disables deletion (for read-only CLI use).
func Open(path string, retention time.Duration) (*Store, error) {
	return open(path, retention, defaultQueueSize)
}

func open(path string, retention time.Duration, queueSize int) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("log database %s: %w", path, err)
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("log database %s: %w", path, err)
	}
	w.SetMaxOpenConns(1)
	if err := migrate(w); err != nil {
		w.Close()
		return nil, fmt.Errorf("log database %s: %w", path, err)
	}
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("log database %s: %w", path, err)
	}
	s := &Store{
		w:         w,
		r:         r,
		retention: retention,
		queue:     make(chan *Request, queueSize),
		done:      make(chan struct{}),
	}
	go s.run()
	return s, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("schema version %d is newer than this binary (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return nil
}

// Close stops accepting rows, waits for the writer to drain the queue, and
// closes the database.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.queue)
	s.mu.Unlock()
	<-s.done
	return errors.Join(s.w.Close(), s.r.Close())
}

// Record enqueues a row without blocking. When the queue is full, or the
// store is closed, the row is dropped and Dropped grows.
func (s *Store) Record(r *Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		s.dropped.Add(1)
		return
	}
	select {
	case s.queue <- r:
	default:
		s.dropped.Add(1)
	}
}

// Dropped is the number of rows dropped because the queue was full.
func (s *Store) Dropped() int64 { return s.dropped.Load() }

func (s *Store) run() {
	defer close(s.done)
	var tick <-chan time.Time
	if s.retention > 0 {
		s.sweep(time.Now().Add(-s.retention))
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case r, ok := <-s.queue:
			if !ok {
				return
			}
			batch := []*Request{r}
		fill:
			for len(batch) < maxBatch {
				select {
				case r, ok := <-s.queue:
					if !ok {
						break fill
					}
					batch = append(batch, r)
				default:
					break fill
				}
			}
			s.insert(batch)
		case now := <-tick:
			s.sweep(now.Add(-s.retention))
		}
	}
}

const insertSQL = `INSERT INTO requests (
	ts, request_id, upstream_request_id, protocol, operation, client_model, route,
	provider, upstream_model, status, outcome, stream, attempts, attempts_ms, ttfb_ms,
	total_ms, routing_json, attempt_log, affinity_key, input_tokens, cache_read_tokens,
	cache_write_tokens, cache_write_1h_tokens, output_tokens, reasoning_tokens,
	input_image_tokens, output_image_tokens, usage_json, cost_usd, price_key, price_json, error
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

// insert writes a batch in one transaction; a row that fails is logged and
// skipped so it cannot take the rest of the batch with it.
func (s *Store) insert(batch []*Request) {
	tx, err := s.w.Begin()
	if err != nil {
		slog.Warn("log write failed", "rows", len(batch), "err", err)
		return
	}
	stmt, err := tx.Prepare(insertSQL)
	if err != nil {
		tx.Rollback()
		slog.Warn("log write failed", "rows", len(batch), "err", err)
		return
	}
	for _, r := range batch {
		_, err := stmt.Exec(
			r.TS.UnixMilli(), r.RequestID, r.UpstreamRequestID, r.Protocol, r.Operation, r.ClientModel, r.Route,
			r.Provider, r.UpstreamModel, r.Status, r.Outcome, r.Stream, r.Attempts, r.AttemptsMS, r.TTFBMS,
			r.TotalMS, r.RoutingJSON, r.AttemptLog, r.AffinityKey, r.InputTokens, r.CacheReadTokens,
			r.CacheWriteTokens, r.CacheWrite1hTokens, r.OutputTokens, r.ReasoningTokens,
			r.InputImageTokens, r.OutputImageTokens, r.UsageJSON, r.CostUSD, r.PriceKey, r.PriceJSON, r.Error,
		)
		if err != nil {
			slog.Warn("log row not written", "request_id", r.RequestID, "err", err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		slog.Warn("log write failed", "rows", len(batch), "err", err)
	}
}

// sweep deletes request and ownership rows older than cutoff.
func (s *Store) sweep(cutoff time.Time) {
	for _, table := range []string{"requests", "owned_responses"} {
		if _, err := s.w.Exec(`DELETE FROM `+table+` WHERE ts < ?`, cutoff.UnixMilli()); err != nil {
			slog.Warn("log retention sweep failed", "table", table, "err", err)
		}
	}
}

// RecordOwner records which target produced a response id (docs/affinity.md).
// It writes synchronously: the client's next turn may arrive milliseconds
// after the response completes.
func (s *Store) RecordOwner(id, provider, upstreamModel string) error {
	_, err := s.w.Exec(`INSERT INTO owned_responses (id, provider, upstream_model, ts) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET provider = excluded.provider, upstream_model = excluded.upstream_model, ts = excluded.ts`,
		id, provider, upstreamModel, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("record owner of %q: %w", id, err)
	}
	return nil
}

// Owner returns the target that produced a response id; ok is false when the
// id is unknown.
func (s *Store) Owner(id string) (provider, upstreamModel string, ok bool, err error) {
	err = s.r.QueryRow(`SELECT provider, upstream_model FROM owned_responses WHERE id = ?`, id).Scan(&provider, &upstreamModel)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("owner of %q: %w", id, err)
	}
	return provider, upstreamModel, true, nil
}
