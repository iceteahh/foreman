CREATE TABLE IF NOT EXISTS tasks (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    priority     INTEGER NOT NULL DEFAULT 0,
    requested_by TEXT NOT NULL DEFAULT '',
    session_id   TEXT,
    created_at   TEXT NOT NULL,
    doc          TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
    id           TEXT PRIMARY KEY,
    task_id      TEXT NOT NULL REFERENCES tasks(id),
    attempt      INTEGER NOT NULL,
    session_id   TEXT NOT NULL,
    session_mode TEXT NOT NULL,
    status       TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    started_at   TEXT,
    finished_at  TEXT,
    doc          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS runs_status_idx ON runs(status, created_at);
CREATE INDEX IF NOT EXISTS runs_task_idx   ON runs(task_id, attempt);

CREATE TABLE IF NOT EXISTS run_events (
    run_id     TEXT NOT NULL REFERENCES runs(id),
    seq        INTEGER NOT NULL,
    at         TEXT NOT NULL,
    from_status TEXT,
    to_status  TEXT NOT NULL,
    reason     TEXT,
    PRIMARY KEY (run_id, seq)
);
