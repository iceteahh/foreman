-- Daily spend ledger (plan Step 15, design §8): one row per (key, UTC day),
-- where key is a task kind or 'global'. Intake refuses a saturated kind, the
-- orchestrator stops leasing it, and a global breach opens the circuit breaker.
CREATE TABLE IF NOT EXISTS budget_spend (
    key        TEXT NOT NULL,
    day        TEXT NOT NULL,          -- YYYY-MM-DD, UTC
    usd        REAL NOT NULL DEFAULT 0,
    runs       INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (key, day)
);
CREATE INDEX IF NOT EXISTS budget_spend_day_idx ON budget_spend(day, key);

-- Dead-letter queue (plan Step 18): every run that exhausted its retries or
-- its job attempts, with the evidence an operator needs to requeue or close it.
CREATE TABLE IF NOT EXISTS dead_letters (
    run_id      TEXT PRIMARY KEY REFERENCES runs(id),
    task_id     TEXT NOT NULL,
    kind        TEXT NOT NULL,
    reason      TEXT NOT NULL,
    attempt     INTEGER NOT NULL DEFAULT 0,
    phase       INTEGER NOT NULL DEFAULT 0,
    feedback    TEXT NOT NULL DEFAULT '',
    event_log   TEXT NOT NULL DEFAULT '',
    dead_at     TEXT NOT NULL,
    paged_at    TEXT,
    requeued_at TEXT,
    requeue_run TEXT
);
CREATE INDEX IF NOT EXISTS dead_letters_open_idx ON dead_letters(requeued_at, dead_at);
