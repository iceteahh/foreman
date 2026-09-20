-- Postgres schema for the scaled topology (plan Step 22). It mirrors the
-- SQLite schema one table at a time, with two deliberate differences:
--
--   * every timestamp is `timestamptz`, not text. The SQLite store has to
--     format a fixed-width string because those columns are compared as TEXT;
--     Postgres compares instants, so the whole class of trimmed-fraction
--     ordering bugs cannot happen here.
--   * `doc` stays TEXT, exactly as in SQLite. JSONB is tempting — an operator
--     could query into a run without decoding it — but it *normalises* what it
--     stores: whitespace and key order change, and a `\u0000` inside a string
--     is rejected outright. `doc` carries the worker's own structured output,
--     so JSONB would silently rewrite a deliverable on one backend and fail an
--     otherwise successful run on the other. An operator who wants to query
--     into a document casts: `doc::jsonb -> 'kind'`.
CREATE TABLE IF NOT EXISTS tasks (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    priority     INTEGER NOT NULL DEFAULT 0,
    requested_by TEXT NOT NULL DEFAULT '',
    session_id   TEXT,
    parent_id    TEXT,
    created_at   TIMESTAMPTZ NOT NULL,
    doc          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS tasks_parent_idx ON tasks(parent_id, created_at) WHERE parent_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS tasks_kind_idx ON tasks(kind, created_at);

CREATE TABLE IF NOT EXISTS runs (
    id           TEXT PRIMARY KEY,
    task_id      TEXT NOT NULL REFERENCES tasks(id),
    attempt      INTEGER NOT NULL,
    session_id   TEXT NOT NULL,
    session_mode TEXT NOT NULL,
    status       TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    doc          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS runs_status_idx ON runs(status, created_at);
CREATE INDEX IF NOT EXISTS runs_task_idx   ON runs(task_id, attempt);
-- The session sweeper and the fan-in both ask for a task's newest run.
CREATE INDEX IF NOT EXISTS runs_task_latest_idx ON runs(task_id, created_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS run_events (
    run_id      TEXT NOT NULL REFERENCES runs(id),
    seq         INTEGER NOT NULL,
    at          TIMESTAMPTZ NOT NULL,
    from_status TEXT,
    to_status   TEXT NOT NULL,
    reason      TEXT,
    PRIMARY KEY (run_id, seq)
);

CREATE TABLE IF NOT EXISTS review_posts (
    run_id       TEXT PRIMARY KEY REFERENCES runs(id),
    task_id      TEXT NOT NULL,
    channel      TEXT NOT NULL,
    ref          TEXT NOT NULL,
    posted_at    TIMESTAMPTZ NOT NULL,
    escalated_at TIMESTAMPTZ,
    resolved_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS review_posts_open_idx ON review_posts(posted_at)
    WHERE resolved_at IS NULL AND escalated_at IS NULL;

CREATE TABLE IF NOT EXISTS review_decisions (
    id            BIGSERIAL PRIMARY KEY,
    run_id        TEXT NOT NULL REFERENCES runs(id),
    task_id       TEXT NOT NULL,
    action        TEXT NOT NULL,
    decided_by    TEXT NOT NULL,
    comment       TEXT NOT NULL DEFAULT '',
    source        TEXT NOT NULL DEFAULT '',
    next_run_id   TEXT,
    decided_at    TIMESTAMPTZ NOT NULL,
    judge_verdict TEXT NOT NULL DEFAULT '',
    checks_failed BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS review_decisions_run_idx ON review_decisions(run_id, id);
CREATE INDEX IF NOT EXISTS review_decisions_at_idx ON review_decisions(decided_at DESC);

CREATE TABLE IF NOT EXISTS budget_spend (
    key        TEXT NOT NULL,
    day        TEXT NOT NULL,          -- YYYY-MM-DD, UTC
    usd        DOUBLE PRECISION NOT NULL DEFAULT 0,
    runs       INTEGER NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (key, day)
);
CREATE INDEX IF NOT EXISTS budget_spend_day_idx ON budget_spend(day, key);

CREATE TABLE IF NOT EXISTS dead_letters (
    run_id      TEXT PRIMARY KEY REFERENCES runs(id),
    task_id     TEXT NOT NULL,
    kind        TEXT NOT NULL,
    reason      TEXT NOT NULL,
    attempt     INTEGER NOT NULL DEFAULT 0,
    phase       INTEGER NOT NULL DEFAULT 0,
    feedback    TEXT NOT NULL DEFAULT '',
    event_log   TEXT NOT NULL DEFAULT '',
    dead_at     TIMESTAMPTZ NOT NULL,
    paged_at    TIMESTAMPTZ,
    requeued_at TIMESTAMPTZ,
    requeue_run TEXT
);
CREATE INDEX IF NOT EXISTS dead_letters_open_idx ON dead_letters(dead_at) WHERE requeued_at IS NULL;

-- Singleton leases: the periodic jobs run on exactly one replica at a time.
CREATE TABLE IF NOT EXISTS leases (
    name       TEXT PRIMARY KEY,
    owner      TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
