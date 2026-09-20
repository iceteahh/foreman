-- Fan-out workflows (plan Step 21): a child task records the parent whose
-- planner run created it, so the fan-in can find its siblings without scanning
-- every task document.
ALTER TABLE tasks ADD COLUMN parent_id TEXT;
CREATE INDEX IF NOT EXISTS tasks_parent_idx ON tasks(parent_id, created_at);

-- Singleton leases (plan Step 22): the periodic jobs — SLA escalation, session
-- sweep, fan-in sweep — must run on exactly one orchestrator replica at a
-- time. The lease is leader-free: whoever wins the row owns the job until it
-- expires, and a crashed holder's lease is simply reclaimed when it does.
CREATE TABLE IF NOT EXISTS leases (
    name       TEXT PRIMARY KEY,
    owner      TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
