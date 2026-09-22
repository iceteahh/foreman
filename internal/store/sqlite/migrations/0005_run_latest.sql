-- LatestRun and the fan-in read a task's newest run by (created_at DESC, id DESC).
-- runs_task_idx is (task_id, attempt), which does not serve that order: SQLite
-- scans every run of the task and sorts. Postgres has carried this index since
-- 0001_init; SQLite did not, so the single-node install was the slow one.
CREATE INDEX IF NOT EXISTS runs_task_latest_idx ON runs(task_id, created_at DESC, id DESC);
