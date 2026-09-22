# Production readiness plan

Written 2026-09-22 from a full review of `main` at `741a636`. Build, vet, race tests and lint were green;
coverage is 65% or higher in most packages. The core lifecycle (state machine, storage, fan-out, retries,
sessions, judge isolation, container and k8s hardening) is solid. The gaps are at the trust edges and in
failure-recovery paths. Sized for one engineer; estimates are rough.

## Why it is not ready today

Two findings bypass the whole evaluation pipeline on their own:

- **The HTTP API has no authentication.** `POST /tasks` and `POST /runs/{id}/review` are mounted with no
  credential check (`internal/intake/http.go`), on the listener the GitHub webhook needs exposed, and the Helm
  Ingress exposes `/`. Anyone reaching the port can submit a write-capable task, approve or reject any run,
  and read every transcript.
- **Write-kind workers can push with the operator's git credentials.** `templates/code_fix/policy.json` grants
  `Bash(git *)` and the runner passes the host `HOME` through, so in local mode a worker can `git push` via
  the host's credential helper or SSH key, skipping the judge, review and delivery. The harness strips
  credential helpers for its own git calls (`internal/workspace/local.go`) but not for the worker.

Three more defeat a safety backstop:

- A failed `kubectl delete` is swallowed and the kill reports success (`internal/runner/k8s.go`), so a
  runaway Job keeps spending.
- The nightly golden gate blesses any run within 5% of baseline; one case of 21 is 4.8%, so the bar can drift
  down one case per night forever. The blessed baseline (76%) is already under the 80% floor.
- A lost lease can strand a run in `queued` with no job and no sweeper (`internal/orchestrator/pool.go`).

The full finding list is in the review; medium items are folded into the phases below.

## Phase 1 · Close the trust boundary

About a week. Blocks any use against real repositories.

1. **Authenticate the HTTP API.** Add `server.api_token_env` to config and a bearer-token middleware in
   `intake.Server`, next to the existing body limiter. Every route requires it except `/healthz` and
   `/webhooks/github`, which keeps HMAC. Refuse to start with no token unless the bind address is loopback.
   Tests: 401 without a token; the webhook still works on HMAC alone.
2. **Take git credentials away from workers.** Add `GIT_TERMINAL_PROMPT=0`, `GIT_CONFIG_NOSYSTEM=1` and
   `GIT_CONFIG_GLOBAL=/dev/null` (the trio `workspace/local.go` already uses) to the worker env in
   `runner.env`. Narrow `code_fix` and `code_fix_planned` from `Bash(git *)` to `Bash(git status *)`,
   `Bash(git diff *)`, `Bash(git log *)`, `Bash(git show *)`, matching `code_review`. Add a `templates` test
   asserting no write kind carries `Bash(git *)`. Run the golden suite once to confirm no pass-rate change.
3. **Delimit untrusted prompt input.** Fence issue title and body in the prompt templates with a line stating
   they are user content, not instructions. Make `github.trigger_label` required in `config.Validate` so any
   issue-opener cannot start a run.
4. **Close the same-class path bugs.** Validate golden case ids against `^[a-z0-9][a-z0-9-]*$` before they
   reach `filepath.Join` and `os.RemoveAll` (`internal/eval/golden/fixture.go`). `Clean` repo paths and check
   the prefix, not just a leading `../` (`case.go`). Anchor `repoHolding` on `data_root` instead of the config
   file's directory (`internal/config/config.go`).

**Exit:** the four high security findings are closed and `harness doctor` reports auth configured.

## Phase 2 · Make the backstops hold

About a week.

1. **k8s kill must confirm.** Return the `kubectl delete` error from `deleteJob`, propagate it from `Signal`,
   and mark the run killed only after the Job reads NotFound within `kill_timeout`. Otherwise dead-letter and
   page with the Job name. Test with a fake `kubectl` that fails.
2. **Survive lease loss.** Cancel the run context after repeated heartbeat failures. Treat `ErrLeaseLost`
   from `Nack` or `DeadLetter` as "another worker owns this" and leave run status alone. Add an
   `orchestrator.Periodic` sweeper that re-enqueues `queued` runs with no live job.
3. **Requeue cannot rewind.** Refuse `Requeue` when the dead run's phase differs from the task's current
   phase, and make `UpdateTaskPhase` a compare-and-swap like `AdvancePhase` (`internal/orchestrator/lifecycle.go`).
4. **Stop the gate ratchet.** Keep `-min-pass-rate 0.8` alongside `-max-drop` in `.github/workflows/golden.yml`.
   Bless a baseline only when pass rate is at or above the previous one. Run the nightly with `-repeat 3`.
   In `runSamples`, mark a case skipped and set `BudgetExhausted` when the cap cuts its samples short.
5. **Small runner fixes.**
   - Route the serve-error path in `cmd/harness/app.go` through the same drain as shutdown.
   - Join the stderr drain before `proc.Wait()` in `internal/runner/runner.go`.
   - Make `Progress.OnEvent` non-blocking (buffered channel or goroutine per publish).
   - Set the egress tunnel deadline on the connection being read, not written (`internal/egress/proxy.go`).

**Exit:** a chaos test per item, in the style of the existing dead-letter test: failed kubectl, lease expiry
mid-run, stale-phase requeue, capped golden run.

## Phase 3 · Prove it in a supervised rollout

About two weeks.

1. **Tests where coverage is thin.** Boot and SIGTERM drain for `cmd/harness` (2.7% today). S3 session
   snapshot, restore and copy against MinIO from the compose stack under the `docker` tag (`session/s3.go` has
   no tests and k8s mode requires it). Targets: 50% and 80%.
2. **Run it for real, narrowly.** Docker mode, two or three internal repos, `draft_prs: true`, every delivery
   human-reviewed, daily global budget around 20 USD.
3. **Wire alerts.** Prometheus rules for kill-failed, run stuck in `queued` longer than three leases, breaker
   open, gate blocked, DLQ growth. Fire the pager path once end to end.
4. **Hygiene.** Untrack `harness.yaml` or fix the README (it says to copy the example). Remove the root
   `task.json`. Require compose passwords from the environment instead of `minioadmin` / `admin`.
   Digest-pin the worker base images. Add `govulncheck` to CI.

**Exit:** one week with no orphaned workers, no stuck runs, no budget overshoot beyond one run's cost, and a
stable golden pass rate across three nightlies.

## Phase 4 · Scale topology

After Phase 3 holds.

1. Stand up Postgres, S3 sessions and k8s workers in staging. Confirm a retry restores its session on a
   different node.
2. Make the budget a hard ceiling: reserve the run's `max_cost_usd` at admission, settle on `Record`
   (`internal/budget`).
3. Add the `runs(task_id, created_at DESC, id DESC)` index to the SQLite migrations (Postgres has it) and
   batch the fan-in `LatestRun` calls.
4. Load-test at full configured concurrency and watch heartbeat latency; this is where the Phase 2 lease
   work gets exercised for real.

**Exit:** k8s mode passes the same one-week bar as Phase 3.

## Low-priority follow-ups

- Byte-index truncation in `deadletter.clip`, `golden.firstLine`, `audit.clipLine`, `tree.truncate` can split
  a UTF-8 rune; use a rune-aware cut.
- `FileSink.Open` comment says "truncates" but the flags append; fix the comment.
- The `needs_review → queued` edge in `task/state.go` is never exercised; document or remove it.
- The `Classify` subtype fallback in `internal/events` is unreachable against the pinned CLI; tie it to a
  fixture or remove it.
