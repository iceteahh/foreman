-- Human review queue (plan Step 12): one post per needs_review run, every
-- decision recorded (feeds the golden suite, Step 19).
CREATE TABLE IF NOT EXISTS review_posts (
    run_id       TEXT PRIMARY KEY REFERENCES runs(id),
    task_id      TEXT NOT NULL,
    channel      TEXT NOT NULL,
    ref          TEXT NOT NULL,
    posted_at    TEXT NOT NULL,
    escalated_at TEXT,
    resolved_at  TEXT
);
CREATE INDEX IF NOT EXISTS review_posts_open_idx ON review_posts(resolved_at, escalated_at, posted_at);

CREATE TABLE IF NOT EXISTS review_decisions (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id        TEXT NOT NULL REFERENCES runs(id),
    task_id       TEXT NOT NULL,
    action        TEXT NOT NULL,
    decided_by    TEXT NOT NULL,
    comment       TEXT NOT NULL DEFAULT '',
    source        TEXT NOT NULL DEFAULT '',
    next_run_id   TEXT,
    decided_at    TEXT NOT NULL,
    judge_verdict TEXT NOT NULL DEFAULT '',
    checks_failed INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS review_decisions_run_idx ON review_decisions(run_id, id);
