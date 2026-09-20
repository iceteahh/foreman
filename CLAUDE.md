# harness-loop-platform-go

Go harness that runs autonomous agent tasks by spawning Claude Code headless (`claude -p`).
Design: `claude-p-agent-harness-design.md`. Plan: `IMPLEMENTATION_PLAN.md`. Verified CLI behaviour: `docs/cli-contract.md`.

## Secrets
`.env` (gitignored, template `.env.example`) holds the worker credential (`ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token`; an OAuth token in `ANTHROPIC_API_KEY` gets 401), `GITHUB_TOKEN`, `GITHUB_WEBHOOK_SECRET`, `HARNESS_LOG`.
`bin/harness`, `make`, and `scripts/demo-m1.sh` load it from the working directory; existing environment variables win.

## Commands
- `make build` → `bin/harness`; `make test` (race, all packages); `make lint` (golangci-lint v2)
- `make live` runs build-tagged `live` tests that spawn the real CLI and spend tokens (runner + judge; needs a worker credential)
- `make golden-validate` (free) checks every golden case parses and has its fixture; `make golden` runs the whole
  suite under a cost cap; `make live-golden` runs a few cases through the real pipeline
- `make worker-image` builds the pinned worker image; `make docker-test` runs the build-tagged `docker` tests (real daemon, no tokens)
- `make postgres-test` starts a throwaway Postgres and runs the build-tagged `postgres` tests (store + queue, no tokens);
  `make k8s-validate` (free) lints and renders the Helm chart, validates every manifest and the worker Job the harness
  builds, and checks the binary accepts the rendered config; `make helm-config` regenerates `deploy/harness.k8s.yaml`
- `make compose-up` / `compose-down` run the ops stack in `deploy/` (harness, egress proxy, OTel collector, Prometheus, Grafana, MinIO)
- `bin/harness doctor` checks Go, git, the pinned CLI (host binary or worker image), docker and the internal network, credentials, Slack
- `bin/harness review -run <id> [-comment …] [-process] approve|reject|close` decides a `needs_review` run without Slack
- `bin/harness replay <run_id> [-v]` prints a run's event stream as a timeline; `-file <log.ndjson>` replays a fixture
- `bin/harness tree <task_id>` prints a fan-out parent, its children and every run as one tree
- `bin/harness requeue -list` shows the dead-letter queue; `requeue <run_id> [-note …] [-process]` retries with a fresh attempt counter
- `bin/harness budget` shows today's spend against the daily ceilings; `bin/harness egress` runs the worker allowlist proxy
- `bin/harness eval run evals/golden [-max-cost 8] [-tag cheap]` runs the golden suite (Layer 4, spends tokens);
  `eval gate -baseline … -report …` is the regression gate, `eval list` and `make golden-validate` are free,
  `eval import-reviews` turns human overrides into case skeletons
- `scripts/demo-m1.sh` (code_fix, offline or `REAL=1`), `scripts/demo-m2.sh` (plan → approve → implement), `scripts/demo-m3.sh` (dead letter → page → replay → requeue; `DOCKER=1` for a containerised worker behind the egress allowlist),
`scripts/demo-m4.sh` (golden suite → baseline → degraded worker → gate blocks; offline, `REAL=1` spends tokens),
`scripts/demo-m5.sh` (fan-out: planner → three parallel children → synthesizer; offline, `REAL=1` spends tokens)

## Layout
`cmd/harness` single binary (`serve | run-once | review | doctor | version`). Everything else under `internal/`:
`task` (types, phases, fan-out child spec, state machine), `store` + `queue` (interfaces; `sqlite/` and `postgres/` impls),
`fanout` (pure: planner output → child specs, children → synthesizer input), `workspace`, `runner`
(spawn/supervise `claude -p`), `events` (NDJSON decoder), `session` (transcript snapshot/restore),
`audit` (file + S3/MinIO sink, `replay`), `eval/checks` (Layer 1), `eval/judge` (Layer 2, ephemeral read-only
`claude -p` with a rubric schema), `eval/route` (Layer 3, pure function) + `eval/feedback` (retry prompt),
`review` (+ `review/slack`: Block Kit post, buttons, SLA escalation, progress thread, pager), `deliver`, `intake`,
`orchestrator` (pool, retries via `--resume`, phases, fan-out/fan-in, `review.Decider`, session sweeper, requeue,
`Periodic` singleton driver), `config`,
`container` (docker argv builder), `k8s` (Job manifest + kubectl argv builder), `egress` (CONNECT allowlist proxy),
`budget` (daily ceilings + circuit breaker),
`deadletter` (DLQ + paging), `obs` (OTel metrics, per-run traces, live progress),
`eval/golden` (Layer 4: cases, fixtures, scoring, report, regression gate, review-override import).
Worker image in `worker/Dockerfile`; ops stack and Grafana dashboard in `deploy/`; Helm chart for the scaled
topology in `deploy/k8s/` (its rendered config is checked in as `deploy/harness.k8s.yaml` and loaded by a test).
Task-kind templates in `templates/<kind>/`; multi-phase kinds add `templates/<kind>/phases/<n>/{phase.json,prompt.md.tmpl}`.
Task-kind fan-out templates add `templates/<kind>/child/{task.json,prompt.md.tmpl}`.
Kinds: `code_fix`, `code_fix_planned` (change kinds → PR/branch), `code_fix_fanout` (planner → N `code_fix` children →
synthesizer; the parent is read-only) and `code_review`, `report`, `triage`
(read-only, structured-output → `deliver.Report`); `deliver.ByKind` routes them.
Golden suite in `evals/golden/` (cases + `fixtures/`), reports in `evals/reports/`; see `evals/README.md`.
Live tests are build-tagged: `live` spends tokens, `docker` needs a daemon and the worker image but no tokens,
`postgres` needs `$HARNESS_TEST_POSTGRES_DSN` but no tokens.
Fixtures in `testdata/events/` were captured from CLI 2.1.243 (2.1.270 for the three dated 2026-09-15); the pin is
2.1.270 since 2026-09-17 (`docs/cli-contract.md` §7). Replay fixtures instead of spending tokens.

## Rules
- No LLM calls outside `runner` and `eval/judge`.
- Never pass user-controlled text through a shell: always `exec.Command` argv. Acceptance commands are split with go-shellwords, not `sh -c`.
- Classify a `result` event on `is_error` + `terminal_reason`, never on `subtype` alone.
- `--session-id` XOR `--resume`, unless `--fork-session`. Session ids are never reused.
- Workers get an env allowlist only (`PATH`, `HOME`, `CLAUDE_CONFIG_DIR`, `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN`, plus `env:` from harness.yaml); never override `HOME`.
- Kill the worker's process group, not just the pid. Under docker, retry `docker kill` until the container is
  confirmed: `docker run` creates it asynchronously, so one kill can be a silent no-op.
- Secrets reach a container as `-e NAME` only (value from the docker client's environment), never in argv.
- Git stays on the host in docker mode; only the worker and acceptance commands run in containers.
- The task's resume pointer is the session id the harness minted (the snapshot is stored under it), never the
  id echoed on the result event.
- Interfaces first at every seam that changes across milestones (`store`, `store.Leases`, `queue`, `workspace`,
  `audit.Sink`, `deliver.Adapter`, `session.Store`, `review.Channel`, `runner.Launcher`, `budget.Store`,
  `deadletter.Store`/`Sink`/`Pager`, `obs.ProgressSink`, `golden.Executor`).
- Every step adds fixtures under `testdata/`; token-spending tests are `//go:build live`, daemon tests
  `//go:build docker`, database tests `//go:build postgres`.
- Each attempt is its own `Run` (`attempt` counts within a phase, `retry_of` links the chain); the task's `session_id` is the resume pointer. Retry = `continue` when a snapshot exists, else cold `new` with a fresh UUID.
- The judge never sees the worker transcript; a judge failure is `uncertain` (→ human review), never `pass`. `policy.max_retries` counts retries after the first attempt.
- `Policy.Merge`/`Acceptance.Merge` deep-copy: never `json.Unmarshal` an override into a struct copy that shares slices with the base.
- Read-only kinds (`code_review`, `report`, `triage`) get no write tool and no unrestricted `Bash(git *)`; their
  deliverable is `structured_output`, so their acceptance always sets a `json_schema` and `deliver.Report` ships it.
- SQLite timestamps use the fixed-width `sqlTime`, never `time.RFC3339Nano`: these columns are compared as TEXT and
  a trimmed fraction sorts wrong (`…53.000327Z` > `…53.000327852Z`), which makes a just-enqueued job invisible.
- A golden case id is the gate's join key: renaming one reads as deleting a case and adding another.
- A golden report that stopped on its cost cap never gates a change; a case the suite skipped is never scored.
- A fan-out child is its own task (`parent_id` + the parent's `children`, written in one transaction), with its own
  workspace, runs, retries and delivery. Subtasks are file-disjoint: two children claiming one file produce branches
  that clobber each other, so `fanout.Decode` refuses the plan and the planner goes to human review.
- The fan-in is a compare-and-swap on the parent's phase (`store.AdvancePhase`), so N children finishing at once
  enqueue exactly one synthesizer. A child in `needs_review` is not done: the fan-in waits for the human.
- Children fork the planner's session (`--resume <planner> --fork-session --session-id <new>`), which means the
  snapshot must be copied into the child's task namespace first (`session.Store.Copy`) — the runner restores by the
  task id it is running under.
- Everything periodic takes a named lease before doing anything (`orchestrator.Periodic`, and the cron gate): with
  stateless replicas an ungated nightly cron is N nightly tasks and N budgets. A lease that cannot be read means
  skip the tick, never run it anyway.
- Postgres stores `doc` as TEXT, not JSONB: JSONB normalises whitespace and key order and rejects `\u0000`, and
  `doc` carries the worker's own structured output.
- A worker Job gets its credential from `envFrom: secretRef` and no API token at all; the manifest carries no secret
  value, because it is submitted to the API server and logged by admission controllers.
- `worker.mode: k8s` requires `session.store: s3`: a Job may land on any node, so a transcript on one node's disk
  would make every retry a silent cold start.
- `data_root` is resolved to an absolute path at config load (against the config file's own directory), because
  the worker is spawned with its cwd set to the workspace: a relative root would resolve the checkout, config dir
  and session snapshot against the worker's checkout instead of the harness's directory. `-config harness.yaml` is
  the common case that used to leave it relative, since `filepath.Dir("harness.yaml")` is `.`.
- Keep `data_root` outside the repo the harness itself lives in. Workspaces are checked out under it, and the CLI
  walks parent directories for `CLAUDE.md` and `.claude/`, so a root inside the repo silently feeds the harness's
  own instructions to every worker.
