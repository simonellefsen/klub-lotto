// Package store is the Postgres data layer for klub-lotto.
//
// The DB is the source of truth in the k8s deployment: each game run
// inserts a daily_ledger row plus one runs row per provider. The wiki/
// markdown files are an exported view, regenerated on demand.
//
// We use jackc/pgx/v5 directly (no ORM) so the schema and the Go types
// stay close to the SQL. Connections come from a pgxpool.Pool which is
// safe for concurrent use from the web server.
package store

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// Store wraps a pgxpool.Pool plus the convenience methods the web UI
// needs. Construct with New(ctx, dsn).
type Store struct {
	Pool *pgxpool.Pool
}

// New opens a pool against dsn and runs Migrate. Returns the open store
// for use; caller closes via Close() on shutdown.
//
// dsn examples:
//
//	postgres://user:pass@host:5432/dbname?sslmode=disable
//	host=klub-lotto-pg-rw user=app password=*** dbname=klublotto sslmode=disable
func New(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	s := &Store{Pool: pool}
	if err := s.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Migrate runs the embedded schema.sql. Idempotent.
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.Pool.Exec(ctx, schemaSQL)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Close releases the underlying pool.
func (s *Store) Close() {
	if s != nil && s.Pool != nil {
		s.Pool.Close()
	}
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

// Game is one row of the games table.
type Game struct {
	Slug        string
	Name        string
	Description string
}

// LedgerEntry is one row of the daily_ledger table, denormalised with
// the game name for display.
type LedgerEntry struct {
	ID             int64
	Date           time.Time
	GameSlug       string
	GameName       string
	Prompt         string
	Answer         string
	Submitted      bool
	Registered     bool
	Notes          string
	SourcePath     string
	PageURL        string
	ResultImage    []byte // only populated in GetLedgerEntry (detail views); omitted from list queries
	HasResultImage bool   // populated in ListLedger via octet_length check
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Runs           []Run // populated by GetLedgerEntry; empty in list calls
}

// Run is one provider's vote on a ledger entry.
type Run struct {
	ID         int64
	LedgerID   int64
	Provider   string
	VoteIndex  *int // nullable
	VoteOption string
	Confidence string
	Rationale  string
	LatencyMs  int
	Error      string
	CreatedAt  time.Time
}

// LoginEvent is one MitID/cookie state transition.
type LoginEvent struct {
	ID              int64
	Status          string
	Detail          string
	CookieExpiresAt *time.Time
	CreatedAt       time.Time
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// ListGames returns all games ordered by slug.
func (s *Store) ListGames(ctx context.Context) ([]Game, error) {
	rows, err := s.Pool.Query(ctx, `SELECT slug, name, description FROM games ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Game
	for rows.Next() {
		var g Game
		if err := rows.Scan(&g.Slug, &g.Name, &g.Description); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListLedger returns ledger entries between (from, to) inclusive,
// newest first. If from is zero use a 30-day window.
func (s *Store) ListLedger(ctx context.Context, from, to time.Time) ([]LedgerEntry, error) {
	if from.IsZero() {
		from = time.Now().AddDate(0, 0, -30)
	}
	if to.IsZero() {
		to = time.Now().AddDate(0, 0, 1)
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT l.id, l.date, l.game_slug, g.name, l.prompt, l.answer,
		       l.submitted, l.registered, l.notes, l.source_path, l.page_url,
		       COALESCE(l.result_image IS NOT NULL, false),
		       l.created_at, l.updated_at
		FROM daily_ledger l
		JOIN games g ON g.slug = l.game_slug
		WHERE l.date >= $1 AND l.date <= $2
		ORDER BY l.date DESC, g.slug ASC
	`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		if err := rows.Scan(&e.ID, &e.Date, &e.GameSlug, &e.GameName, &e.Prompt, &e.Answer,
			&e.Submitted, &e.Registered, &e.Notes, &e.SourcePath, &e.PageURL,
			&e.HasResultImage,
			&e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetLedgerEntry returns one entry plus its runs.
func (s *Store) GetLedgerEntry(ctx context.Context, id int64) (*LedgerEntry, error) {
	row := s.Pool.QueryRow(ctx, `
		SELECT l.id, l.date, l.game_slug, g.name, l.prompt, l.answer,
		       l.submitted, l.registered, l.notes, l.source_path, l.page_url,
		       l.result_image,
		       l.created_at, l.updated_at
		FROM daily_ledger l
		JOIN games g ON g.slug = l.game_slug
		WHERE l.id = $1
	`, id)
	var e LedgerEntry
	if err := row.Scan(&e.ID, &e.Date, &e.GameSlug, &e.GameName, &e.Prompt, &e.Answer,
		&e.Submitted, &e.Registered, &e.Notes, &e.SourcePath, &e.PageURL,
		&e.ResultImage,
		&e.CreatedAt, &e.UpdatedAt); err != nil {
		return nil, err
	}
	e.HasResultImage = len(e.ResultImage) > 0
	rows, err := s.Pool.Query(ctx, `
		SELECT id, ledger_id, provider, vote_index, vote_option,
		       confidence, rationale, latency_ms, error, created_at
		FROM runs WHERE ledger_id = $1 ORDER BY id
	`, id)
	if err != nil {
		return &e, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.LedgerID, &r.Provider, &r.VoteIndex, &r.VoteOption,
			&r.Confidence, &r.Rationale, &r.LatencyMs, &r.Error, &r.CreatedAt); err != nil {
			return &e, err
		}
		e.Runs = append(e.Runs, r)
	}
	return &e, rows.Err()
}

// LatestLoginEvent returns the most recent login_events row, or (nil, nil)
// if there isn't one yet. Used by the web UI's "auth status" widget.
func (s *Store) LatestLoginEvent(ctx context.Context) (*LoginEvent, error) {
	row := s.Pool.QueryRow(ctx, `
		SELECT id, status, detail, cookie_expires_at, created_at
		FROM login_events ORDER BY id DESC LIMIT 1
	`)
	var e LoginEvent
	if err := row.Scan(&e.ID, &e.Status, &e.Detail, &e.CookieExpiresAt, &e.CreatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &e, nil
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

// UpsertLedger inserts or updates a (date, game_slug) row. Returns the id.
// runs is the list of provider votes; existing rows for this ledger are
// replaced.
func (s *Store) UpsertLedger(ctx context.Context, e LedgerEntry, runs []Run) (int64, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO daily_ledger
		    (date, game_slug, prompt, answer, submitted, registered, notes, source_path, page_url, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
		ON CONFLICT (date, game_slug) DO UPDATE
		   SET prompt = EXCLUDED.prompt,
		       answer = EXCLUDED.answer,
		       submitted = EXCLUDED.submitted,
		       registered = EXCLUDED.registered,
		       notes = EXCLUDED.notes,
		       source_path = EXCLUDED.source_path,
		       page_url = EXCLUDED.page_url,
		       updated_at = now()
		RETURNING id
	`, e.Date, e.GameSlug, e.Prompt, e.Answer, e.Submitted, e.Registered, e.Notes, e.SourcePath, e.PageURL).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert ledger: %w", err)
	}

	if len(runs) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM runs WHERE ledger_id = $1`, id); err != nil {
			return 0, err
		}
		for _, r := range runs {
			if _, err := tx.Exec(ctx, `
				INSERT INTO runs (ledger_id, provider, vote_index, vote_option,
				                  confidence, rationale, latency_ms, error)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			`, id, r.Provider, r.VoteIndex, r.VoteOption, r.Confidence, r.Rationale, r.LatencyMs, r.Error); err != nil {
				return 0, err
			}
		}
	}
	return id, tx.Commit(ctx)
}

// RecordLogin appends a login_events row.
func (s *Store) RecordLogin(ctx context.Context, ev LoginEvent) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO login_events (status, detail, cookie_expires_at)
		VALUES ($1, $2, $3)
	`, ev.Status, ev.Detail, ev.CookieExpiresAt)
	return err
}

// SetResultImage stores (or clears with nil/empty) a PNG/JPEG screenshot of the
// completed game board for a ledger entry. Used for visual games like Krydsord
// so the nice filled grid can be shown in the web UI detail view. The bytes
// are stored directly in Postgres (source of truth).
func (s *Store) SetResultImage(ctx context.Context, id int64, img []byte) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE daily_ledger
		SET result_image = $2, updated_at = now()
		WHERE id = $1
	`, id, img)
	if err != nil {
		return fmt.Errorf("set result image for ledger %d: %w", id, err)
	}
	return nil
}
