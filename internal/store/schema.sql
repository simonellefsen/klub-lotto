-- klub-lotto Postgres schema.
--
-- Runs once at web/worker startup via store.Migrate. Idempotent — every
-- statement uses IF NOT EXISTS. The wiki/ files are an exported view of
-- this data, not the source of truth.

CREATE TABLE IF NOT EXISTS games (
    slug TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT ''
);

-- Seed the six games we know about. ON CONFLICT keeps the description
-- stable while letting future migrations update it.
INSERT INTO games (slug, name, description) VALUES
    ('quiz',         'Quiz',         'Daily 3-option multiple-choice question.'),
    ('ordknuden',    'Ordknuden',    '5-letter Danish word puzzle (Wordle-style).'),
    ('ordkloever',   'Ordkløver',    'Category + hint word puzzle on a grid.'),
    ('sudoku',       'Sudoku',       '9x9 Sudoku.'),
    ('krydsord',     'Krydsord',     'Danish crossword.'),
    ('blok-for-blok','Blok for Blok','8x8 block placement puzzle.')
ON CONFLICT (slug) DO NOTHING;

CREATE TABLE IF NOT EXISTS daily_ledger (
    id              BIGSERIAL PRIMARY KEY,
    date            DATE NOT NULL,
    game_slug       TEXT NOT NULL REFERENCES games(slug),
    prompt          TEXT NOT NULL DEFAULT '',
    answer          TEXT NOT NULL DEFAULT '',
    submitted       BOOLEAN NOT NULL DEFAULT FALSE,
    registered      BOOLEAN NOT NULL DEFAULT FALSE,
    notes           TEXT NOT NULL DEFAULT '',
    source_path     TEXT NOT NULL DEFAULT '',
    page_url        TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    result_image    BYTEA,
    UNIQUE (date, game_slug)
);

-- Idempotent addition for attaching cropped result screenshots (e.g. completed
-- Krydsord grid) directly into the ledger (Postgres is source of truth).
ALTER TABLE daily_ledger ADD COLUMN IF NOT EXISTS result_image BYTEA;


CREATE INDEX IF NOT EXISTS idx_daily_ledger_date ON daily_ledger(date DESC);

-- One row per LLM provider call for a given ledger row. Captures votes
-- from the parallel CompareAll so the web UI can show a transcript.
CREATE TABLE IF NOT EXISTS runs (
    id              BIGSERIAL PRIMARY KEY,
    ledger_id       BIGINT NOT NULL REFERENCES daily_ledger(id) ON DELETE CASCADE,
    provider        TEXT NOT NULL,
    vote_index      INT,
    vote_option     TEXT,
    confidence      TEXT,
    rationale       TEXT,
    latency_ms      INT,
    error           TEXT,
    raw_response    TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_runs_ledger ON runs(ledger_id);

-- Audit trail for MitID handoffs. The web UI shows "last login" and
-- "cookie status" based on this.
CREATE TABLE IF NOT EXISTS login_events (
    id              BIGSERIAL PRIMARY KEY,
    status          TEXT NOT NULL,  -- initiated | completed | failed | session-reused
    detail          TEXT NOT NULL DEFAULT '',
    cookie_expires_at TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_login_events_created ON login_events(created_at DESC);
