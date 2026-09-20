# Implementation Plan — Agent Harness on `claude -p`

Source design: [claude-p-agent-harness-design.md](claude-p-agent-harness-design.md) (Draft v1.1, §0 corrections applied 2026-09-13).
Language: **Go** (repo is `foreman`; the design's Node snippets are reference only).
Verified against Claude Code CLI **2.1.243** on 2026-09-13. Session-management spike (Step 4 items 1, 2, 4) run the same day; results in [docs/cli-contract.md](docs/cli-contract.md), fixtures in [testdata/events/](testdata/events/).

---

## 0. Corrections to the design before building — **applied 2026-09-13 (design v1.1)**

Found by running `claude --help` and a real `claude -p --output-format stream-json` call. Every row below is now reflected in the design doc and in the steps in §2. One more row was added while applying them (cost mid-run).

| Design says | Reality (CLI 2.1.243) | Use instead |
|---|---|---|
| `--permission-prompts none` | Flag does not exist | `--permission-mode dontAsk` (non-allowlisted tools are denied, surfaced in `result.permission_denials`) |
| Orchestrator kills on cost breach | CLI has a native cap | `--max-budget-usd <n>` as first line; orchestrator kill stays as backstop |
| Parse `session_id` from output | CLI accepts a pre-assigned id | `--session-id <uuid>` — harness mints the UUID, so a crash before `result` still leaves a resumable id |
| `--resume` works from a fresh container | Session transcript lives at `$CLAUDE_CONFIG_DIR/projects/<cwd-slug>/<session_id>.jsonl`. **Verified:** resume looks the id up across all `projects/*/`, so cwd does not matter; a copied-back JSONL resumes fine; a missing one fails with `No conversation found` | **Decided:** snapshot the JSONL to the session store after every run, restore it before `--resume`. No session volume. Design §4.4. |
| Retry cold after a crash | **Verified:** SIGKILL mid tool call leaves a resumable transcript; `--resume` keeps the same id and context | Timeout and crash both retry from the same session; cold retry only when the transcript is missing/corrupt |
| Reuse the session id on cold retry | `--session-id X` with existing `X.jsonl` → `Session ID X is already in use`, exit 1 | Cold retry mints a new UUID |
| `--resume` + `--session-id` together | Rejected unless `--fork-session` | Args builder: mutually exclusive; fork = `--resume <parent> --fork-session --session-id <child>` |
| `HOME=<per-run dir>` isolates the worker | `CLAUDE_CONFIG_DIR` relocates `projects/`, `sessions/`, `.claude.json`; a `HOME` override would also break git/ssh config | `CLAUDE_CONFIG_DIR=<per-run dir>`; workers need `ANTHROPIC_API_KEY` (host login is Keychain-only) |
| `subtype` tells success from failure | Auth failure returns `subtype: "success"` with `is_error: true`, `terminal_reason: "api_error"` | Classify on `is_error` + `terminal_reason` |
| SIGKILL on the CLI pid is enough | Tool child processes survive (`sleep` kept running) | `Setpgid` + kill the process group |
| Worker inherits nothing | Worker inherits the host user's settings, MCP servers, hooks, plugins (init event listed 30+ user MCP tools) | `--setting-sources project` (or none) + `--strict-mcp-config` (+ `--mcp-config` if the task needs MCP). Evaluate `--bare` (strict `ANTHROPIC_API_KEY` auth, no hooks/plugins/keychain) with `--add-dir` to re-enable workspace `CLAUDE.md`. |
| `system/permission_denied` event | Denials appear on the `result` event as `permission_denials[]` | Read from `result`; also count any `system` subtypes seen in fixtures |
| Result carries `structured_output` | Only with `--json-schema`; plain runs put text in `result.result` | Branch on task `acceptance.json_schema` |
| Orchestrator reads running `total_cost_usd` mid-run | `assistant` events carry token usage only; cost appears only on `result` (cli-contract #18) | `--max-budget-usd` is the cap; mid-run kill estimates from tokens × pinned price table |

Observed event stream (one turn): `rate_limit_event` → `system/init` → `assistant` (×N) → `result/success`.
Result fields the parser needs: `is_error`, `subtype`, `num_turns`, `stop_reason`, `terminal_reason`, `session_id`, `total_cost_usd`, `duration_ms`, `duration_api_ms`, `permission_denials`, `api_error_status`, `result`, `usage`, `modelUsage`.

Local prerequisites: Go 1.27.1 and golangci-lint installed 2026-09-13 (Homebrew). Docker, Redis, Postgres, SQLite, `gh` are present.

---

## Status — M5 fan-out and scaled topology built 2026-09-16 (M4 golden suite 2026-09-16)

Steps 1–3, 5–22 are implemented and tested (`make test`, `make lint` green; 32 packages, race detector on).
`make postgres-test` passes against a real Postgres (store and queue), `make k8s-validate` passes offline (the
chart lints, renders, every manifest validates, and the binary accepts the rendered config), and
`scripts/demo-m5.sh` runs the whole fan-out loop offline — planner → three parallel children on their own branches
→ synthesizer — with no tokens. The **k8s deployment has never been run against a real cluster**, and the golden
suite has still never been priced; both are called out under "Still open" below.

| Step | State | Notes |
|---|---|---|
| 21 fan-out workflows | done (unpriced) | A planner phase marked `fan_out` in `templates/<kind>/phases/<n>/phase.json` decomposes its structured output into one **child task** per subtask. `internal/fanout` is the pure seam: `Decode` validates the plan, `Plan.Specs` renders `task.Spec`s from `templates/<kind>/child/{task.json,prompt.md.tmpl}`, and `Summaries` turns the children's recorded outcomes back into the synthesizer's input. Children fork the planner's session (`--resume <planner> --fork-session --session-id <new>`), which needed `session.Store.Copy` — the runner restores a transcript by the task id it is running under, and a child is a task of its own. The fan-in (`Pool.maybeFanIn`, plus `FanInSweeper` for crash recovery) is a compare-and-swap on the parent's phase, so N children finishing at once enqueue exactly one synthesizer. New kind `code_fix_fanout`: read-only parent (planner + synthesizer, delivered as a report), `code_fix` children that each deliver their own branch. `harness tree <task_id>` prints the whole family; `GET /tasks/{id}/children` is the API equivalent. |
| 22 scaled topology | done (no cluster) | `internal/store/postgres` and `internal/queue/postgres` on pgx: the same interfaces, `timestamptz` instead of text timestamps, `FOR UPDATE SKIP LOCKED` for leasing, migrations behind an advisory lock so replicas starting together do not race. `internal/session/s3.go` snapshots transcripts to object storage (server-side copy for a fork), which is what removes the PVC from the retry path. `internal/k8s` builds the worker Job manifest and the kubectl argv; `runner.K8sLauncher` drives it through the unchanged `runner.Launcher` contract. Leader-free leasing (`store.Leases`, `orchestrator.Periodic`, `intake.Cron.Gate`) keeps the SLA escalator, both sweepers and every cron entry singleton across replicas. Helm chart in `deploy/k8s/`; `database.driver`, `session.store` and `worker.mode: k8s` are the only config changes a single-node install needs to become a fleet. |

Decisions made while building M5 (confirm or change):

- **A fan-out child is a task, not a run.** The design says "N parallel workers"; making each one a task gives it its
  own workspace, retries, judge, budget line and delivery — and it is what lets a child fail, be reviewed or be
  requeued without touching its siblings. The parent's phase is the join point.
- **Subtasks must be file-disjoint, and `fanout.Decode` enforces it.** Two children editing one file in two
  checkouts produce two branches that silently clobber each other. A plan that overlaps is refused and the planner
  goes to human review rather than starting work that cannot be merged.
- **A child waiting in `needs_review` blocks the fan-in.** Synthesising over it would deliver a merge report for a
  change nobody approved. `FanOutPending` reports that separately, so `run-once` and the golden executor stop and
  say a human owes a decision instead of spinning to their deadline.
- **The synthesizer sees the harness's record of each child, never a child's transcript** — the same evidence a
  human reviewer gets (status, checks, artifacts, structured output). It is a fresh phase of the parent, so it
  inherits the planner's session and the parent's own prompt.
- **The queue is our own table with `SKIP LOCKED`, not River.** The plan said "River or SQS"; one migration path,
  one lease model and one set of semantics shared with the SQLite queue is worth more than the library, and
  `SKIP LOCKED` is the mechanism River itself is built on. The `queue.Queue` interface is unchanged either way.
- **Postgres stores documents as TEXT, not JSONB.** JSONB normalises whitespace and key order and rejects
  `\u0000` inside a string. `doc` carries the worker's own `structured_output`, so JSONB would silently rewrite a
  deliverable on one backend and fail an otherwise successful run on the other. Found by the round-trip test.
- **Workers reach their checkout through a ReadWriteMany volume, not a git clone inside the Job.** Git stays in the
  orchestrator, which is what keeps the repo token out of a worker pod (the docker-mode rule, unchanged). The cost
  is a real requirement on the cluster: `doctor` warns when the claim is not RWX, because ReadWriteOnce pins every
  Job to the orchestrator's node and only fails once you scale out.
- **kubectl, not client-go.** The runner already supervises "a process with stdout, stderr, a signal and an exit
  code", which is exactly what a kubectl subprocess is. It keeps the harness off the client-go version treadmill.
  The cost: a pod has one output stream, so the launcher demultiplexes NDJSON from stderr by line, and
  `worker.prompt_via_stdin` is rejected in k8s mode.
- **A worker Job gets no service-account token** (`automountServiceAccountToken: false`) and its credential only as
  `envFrom: secretRef`. A manifest is submitted to the API server and logged by admission controllers, so nothing
  sensitive may appear in it.
- **`worker.mode: k8s` requires `session.store: s3`,** enforced in `config.Validate`. A Job may land on any node; a
  transcript on one node's disk would turn every retry into a cold start that nothing reports.
- **The rendered cluster config is checked in** (`deploy/harness.k8s.yaml`) and loaded by a unit test. A chart that
  renders a key the loader rejects otherwise only fails at rollout, as a crash-looping pod.

Found and fixed while building M5 (pre-existing, outside Step 21/22):

- **A fan-in that claimed the parent's phase and then failed to queue the synthesizer stranded the parent.** The
  claim is a compare-and-swap, so the retry path has to hand it back; and because the failed attempt still left a
  run row for the next phase, the fan-in now finds its planner by phase rather than taking the task's newest run.
  Regression test: `TestFanInReleasesItsClaimWhenTheEnqueueFails`.
- **Nothing periodic was safe to run twice.** The SLA escalator, the session sweeper and every cron entry ran
  unconditionally, which is correct for one process and wrong the moment the design's "stateless orchestrator
  replicas" exist: a nightly cron would submit N tasks and spend N budgets. All of them now take a named lease
  (`orchestrator.Periodic`, `intake.Cron.Gate`), which is a no-op on a single node.

Still open after M5:

- **The k8s deployment has never run against a cluster.** Everything that can be checked without one is checked in
  CI: the chart lints and renders, kubeconform validates every manifest including the worker Job the harness builds
  at runtime, and the binary loads the rendered config. What is not proven is the behaviour that only a cluster
  shows — RWX subPath mounts, `kubectl logs --follow` reattaching after a node restart, and Job deletion as the
  kill path. The plan's "done when the golden suite passes on the k8s deployment" is therefore **not met**.
- The golden suite still has no priced baseline (unchanged from M4), so the gate can only enforce its absolute
  floor. The new `code-fix-fanout-three-packages` case is the most expensive in the suite: one planner, three
  children and a synthesizer.
- The fan-out prompts have never been tried on a real planner. The property most likely to be wrong first is
  file-disjointness: a model asked to split work will happily give two subtasks the same file, and the harness
  answers that with human review rather than a bad merge.
- `session.S3` and `runner.K8sLauncher` have unit coverage only (keys, URIs, manifests, argv). Neither has been run
  against MinIO or a cluster; a `minio`-tagged test for the first would be cheap and is not written.

---

## Status — M4 golden suite and regression gate built 2026-09-16 (M3 ops hardening 2026-09-15)

Steps 1–3, 5–20 are implemented and tested (`make test`, `make lint` green; 26 packages, race detector on).
`make golden-validate` passes (20 cases, every fixture present); `scripts/demo-m4.sh` runs the whole Layer 4
loop offline — baseline → degraded worker → blocked gate — with no tokens. The suite has **not yet been run
against the real CLI**: `make golden` and `make live-golden` are wired and the CI workflow is in place, but the
first paid run is still outstanding, so no baseline report exists yet.

| Step | State | Notes |
|---|---|---|
| 19 golden suite | done (unpriced) | `internal/eval/golden`: `Case`/`Suite` loaded from `evals/golden/*.json` with unknown fields rejected and the fixture checked at load time, `Fixtures.Materialize` turning a `repo.dir` tree or a `repo.bundle` into a throwaway bare origin per case, a pure `Score` from expectations to a `CaseResult`, `Report` with the design §5.4 metrics (pass rate, retry rate, avg turns/cost/duration, judge/human agreement) and `Gate`. The orchestrator is reached through one seam, `golden.Executor`, implemented by `appExecutor` in `cmd/harness`: every case goes through the real submitter, pool, checks, judge, router and delivery, so the suite evaluates the system that ships. `harness eval run\|gate\|list\|import-reviews`; `-max-cost` stops the suite mid-way and marks the report `budget_exhausted`, which the gate then refuses. 20 cases over 5 fixture repos (Go, Python, JavaScript) cover all five kinds; **17 expect a pass, 1 a dead letter, 1 human review**, and the "gaming" cases pin the behaviours most worth protecting (fix the code not the test, stay in scope, stay read-only, admit what you cannot know). |
| 20 regression gate + kinds 2 & 3 | done (unpriced) | `.github/workflows/golden.yml`: a free `validate` job on every PR, and a paid `gate` job on the nightly schedule and on PRs touching `templates/`, `CLAUDE.md`, `evals/`, `internal/eval/`, the pinned CLI or the worker image. The gate blocks on a pass-rate drop over the delta (default 5 points), on any case that passed in the baseline and now fails, and on any baseline case missing from the run. New kinds `code_review`, `report` and `triage`: read-only policies (no write tool, only inspecting `git` subcommands), a `json_schema` acceptance that *is* the deliverable, and prompts that push back on the failure modes that make each kind worthless (inventing findings, inventing numbers, confidently mis-triaging). `deliver.ByKind` routes them to the new `deliver.Report` adapter (JSON + Markdown on disk, optional Slack post) instead of `BranchPush`, which would have failed every one of them with "nothing to deliver". |

Decisions made while building M4 (confirm or change):

- **A golden case may carry its fixture as a directory, not only as a git bundle.** The design says bundle; a
  directory is reviewable and diffable in the pull request that changes it, which matters more for a hand-written
  suite. `repo.bundle` is still supported and is the right choice for a case that needs real history;
  `scripts/golden-bundle.sh` builds one. The fixture on disk is never itself a git repository — it is copied and
  committed into a throwaway origin per case.
- **The suite drives the orchestrator through an `Executor` interface, not a copy of the pipeline.** A suite that
  reimplemented routing would drift from the harness and start passing runs the harness would reject. The cost is
  that `appExecutor` lives in `cmd/harness`, where the wiring is; only the live test covers it.
- **A skipped case is reported, never dropped.** Skips (a `skip` note, a filter, the cost cap) appear in the report
  as rows and are excluded from every rate. A case that quietly vanished would make the pass rate look better.
- **The gate blocks on a swapped pass even at an unchanged rate**, and on a baseline case missing from the run.
  Either would otherwise be a way to go green without fixing anything.
- **`expect.max_attempts` is part of the expectation.** A case that starts needing a retry to reach the same
  outcome is a regression in the prompt, even though the run still delivers.
- **Read-only kinds deliver a report, not a branch.** `deliver.ByKind` picks the adapter by kind. `deliver.Report`
  writes the structured output as JSON and Markdown under `<data_root>/reports/<run_id>.{json,md}` and, when Slack
  is configured, posts it; a failed notification returns the artifacts anyway, so an outage cannot lose a paid run.
- **`github.kind` now accepts any known kind**, so an issue label can open a review or a triage, not only a fix.
- **Imported override cases land skipped on purpose.** The fixture repository cannot be reconstructed from a review
  decision — the workspace is gone and the upstream ref has moved — so `eval import-reviews` writes a skeleton with
  a `skip` note naming the repo and ref to capture. An unfinished case that announces itself beats one that scores
  as a regression.

Found and fixed while building M4 (pre-existing, outside Step 19/20):

- **SQLite timestamps were written with `time.RFC3339Nano`, which trims trailing zeros.** These columns are
  compared as TEXT, so `"…53.000327Z" > "…53.000327852Z"` (`'Z'` > `'8'`) and a job enqueued microseconds before a
  lease failed `available_at <= now` and went invisible until the clock moved past it. It surfaced as a flaky
  `TestProvisionFailureRequeues` under parallel load, but it also delayed real leases and weakened every other
  time comparison in the store (lease reclaim, SLA escalation, retention sweeps, budget day boundaries). Both `ts`
  helpers now use a fixed-width `sqlTime`; reads still accept the old rows. Regression test:
  `TestLeaseSeesAJobWithATrimmableFraction`.

Still open after M4:

- The suite has never been priced. Run `make golden` once with a credential to produce
  `evals/reports/baseline.json`, then commit it: until then the gate can only enforce its absolute floor.
- The expectations were written from the fixtures, not from observed runs. The first paid run will show which
  cases are wrong about what a worker actually does (most likely the `output_schema` constraints on the read-only
  kinds, which are the strictest assertions in the suite).
- 20 cases is the bottom of the design's 20–50 band. The review-override importer is the intended way to grow it.

---

## Status — M3 ops hardening built 2026-09-15 (M2 evaluation 2026-09-15, M1 walking skeleton 2026-09-13)

Steps 1–3, 5–18 are implemented and tested (`make test`, `make lint` green; 25 packages, race detector on).
`make docker-test` passes against a real daemon (worker image + egress allowlist, no tokens).
`scripts/demo-m3.sh` passes offline; `make compose-up` brings up the full ops stack.

| Step | State | Notes |
|---|---|---|
| 14 containers | done | `internal/container` is a pure argv builder (`--rm --init --cap-drop ALL --security-opt no-new-privileges`, workspace and per-run `CLAUDE_CONFIG_DIR` bind-mounted, resource caps, labels); `runner.Launcher` seam with `LocalLauncher` (host CLI, process group) and `DockerLauncher` (one container per run). Credentials cross as `-e NAME` only, so no secret is ever in argv or on disk. `workspace.Docker` runs every acceptance command in its own container while git stays on the host, so the repo token is never visible inside one. `worker/Dockerfile` pins Node + `@anthropic-ai/claude-code@2.1.243` + Go, non-root uid 1000. `internal/egress` is the CONNECT allowlist proxy (`harness egress`); workers sit on an `--internal` docker network whose only route out is that proxy. **Verified on a real daemon** (`make docker-test`): the image reports the pinned CLI, `--network none` has no egress, an allowlisted host tunnels through while `example.com` is refused 403, a worker on the internal network has no direct route out, and the proxy logged both decisions. |
| 15 budgets | done | `internal/budget.Ledger` over `budget_spend` (migration 0003): per-kind and global daily ceilings in UTC days. Intake refuses a saturated kind with **429 + Retry-After** and the reason; the pool intersects `leasableKinds` with the ledger so a saturated kind is never leased; a global breach latches a circuit breaker (closing itself at the UTC day boundary) and pages once. Spend is recorded from `result.total_cost_usd` before routing, with the judge booked to its own `judge` key. `GET /budget` and `harness budget` report spend, ceiling, remaining and state; `harness.budget.spend_today` / `harness.budget.limit` are separate gauges so an alert can compare them. A ledger read error **fails open** (the per-run ceiling still bounds each worker). |
| 16 observability | done | **Verified on the running stack:** a real containerised run produced the six-span trace (`run` → queue, worker, checks, route, deliver) at the collector, `harness_runs_total{status="delivered"}` and the stage/cost/spend series at both `/metrics` and Prometheus, the provisioned Grafana dashboard queried them, and `harness replay` read the run's log back out of MinIO (`s3://harness-audit/runs/<run>.ndjson`, 51 KiB). `internal/obs`: OTel metrics (runs by status, duration p50/p95, turns, cost per run and per kind per day, queue depth and oldest-ready age, permission-denial rate, retry rate, judge verdicts, judge/human disagreement, rate limits, dead letters, workers in flight, deliveries) exported to OTLP and/or a Prometheus endpoint; one trace per run with a child span per stage (queue → worker → checks → judge → route → deliver) and `trace_id` on every log line. `obs.Progress` turns the event stream into a live timeline (tool calls, files edited, last command, ticking cost estimate), throttled, published to a Slack message edited in place (`review/slack.ProgressChannel`) or the log. `deploy/` has the compose stack (harness, egress, OTel collector, Prometheus, Grafana, MinIO), the provisioned Grafana dashboard (16 panels) and Prometheus alert rules. Everything degrades to a no-op without a collector. |
| 17 audit to S3 | done | `audit.S3Sink` (MinIO/S3 via minio-go) spools each run's NDJSON locally and uploads on close, so a crash leaves the log readable instead of losing a half-written multipart; a failed upload **keeps the spool file** and `Reader` still serves it. Retention and WORM are bucket policies (the compose stack enables Object Lock + a 365-day lifecycle). `harness replay <run_id>` renders any log (file, spool or object) as a timeline: init, tool calls with their target, api_retry rows, and a classified result summary; `-file` replays a fixture without a database. |
| 18 DLQ and paging | done | `internal/deadletter`: every `dead` run is recorded in `dead_letters` (migration 0003) with the failing checks, their verbatim evidence, the judge's reasoning and the event-log pointer, then paged once (Slack via `review/slack.Pager`, else the log) with replay and requeue instructions. `harness requeue -list` shows the queue; `harness requeue <run_id> [-note …] [-process]` re-enqueues with a **fresh attempt counter** (a human decided it deserves another chance), resuming the session when a snapshot survives and carrying the evidence plus the note as feedback. Rate limits (429/529 `api_retry`, or a non-allowed `rate_limit_event`) back off exponentially in the job's attempt count, capped by `MaxBackoff`, and **shed concurrency**: leasing pauses for the same window. Chaos test: a task that fails every attempt dead-letters, pages with the evidence, and requeues to `delivered`. |

Decisions made while building M3 (confirm or change):

- **The resume pointer is the minted session id, not `result.session_id`.** The transcript is snapshotted under the id the harness passed, so trusting the echoed id would lose the pointer if the two ever differed. The real CLI always echoes it (cli-contract #2); a mismatch is now a warning, not a silent switch. Found by the M3 demo, whose fake CLI replays a fixture with a different id.
- **A single `docker kill` is not enough.** `docker run` creates the container asynchronously, so a timeout that fires just after start kills nothing and the worker runs unbounded. `container.Stop` retries until docker confirms the container existed or the client exits; both the runner and the acceptance-command path use it, and both have a regression test.
- **Git stays on the host in docker mode.** Only `Exec` (acceptance commands) and the worker itself cross into a container, so the repository token is never visible from inside one and `GIT_ASKPASS` keeps working unchanged.
- **The container's `HOME` is set to `/home/worker`**, which is not a violation of the "never override HOME" rule: that rule protects the host worker's git/ssh config. Inside the image, `--user <uid>` with no passwd entry leaves `HOME=/`, which git and npm cannot write to.
- **`--allowedTools` is not the sandbox; the network is.** The CLI auto-approves its built-in read-only shell commands (cli-contract #22), so the internal network plus the egress allowlist is the real boundary. The proxy never decrypts traffic: it allows or refuses a CONNECT tunnel by host and port and logs the decision.
- **Budget ceilings are checked, not reserved.** A run that starts under the ceiling may cross it, exactly as `--max-budget-usd` may be exceeded by the turn that crosses it. The ceiling stops the *next* run, and the global breaker stops everything.
- **The spend ledger fails open.** Refusing every task because the bookkeeping is unavailable is worse than briefly overspending when the per-run cap still applies.
- **Map-valued config keys merge with the defaults** (`budgets.daily_usd`, `concurrency.per_kind`, `env`) rather than replacing them, which is yaml.v3 behaviour. Set a key to 0 to remove a ceiling. Worth revisiting if it surprises operators.
- **A containerised harness must be told the host path of its data root** (`worker.docker.host_data_root`, `$VAR` resolved from the environment). Worker containers are siblings started through the host daemon, which resolves every bind-mount source on the host, so the in-container path would mount an empty directory and every run would fail with an empty diff. `make compose-up` sets it; startup warns when the harness looks containerised and it is unset.
- **Audit logs spool locally before upload.** Streaming a 15-minute multipart upload would lose everything on a crash; the spool file is also what `harness replay` reads for a run in flight.

---

## Status — M2 evaluation pipeline built 2026-09-15 (M1 walking skeleton 2026-09-13)

Steps 1–3, 5–13 are implemented and tested (`make test`, `make lint` green; 20 packages, race detector on). `make live` passes the runner and judge live tests with the real CLI; `scripts/demo-m1.sh` and `scripts/demo-m2.sh` pass offline with a fake CLI.
Step 4: the three missing fixtures (`--json-schema`, `--max-turns`, permission denial) were captured on CLI 2.1.270 on 2026-09-15 (cli-contract §5). Still open: `--bare` check and the 429 shape (both need an `ANTHROPIC_API_KEY`).

| Step | State | Notes |
|---|---|---|
| 9 judge | done | `internal/eval/judge`: rubric schema embedded, prompt = task spec + `git diff <base>` + acceptance outputs + check summary (+ the worker's final message, labelled as a claim to verify), never the transcript. Runs through the shared `runner.Runner` as an ephemeral, `Read`-only task with `--json-schema`, so it gets the same env allowlist, process-group kill, budget backstop and audit log (`<run_id>-judge-<n>.ndjson`). `structured_output` re-validated against the rubric; `gamed_checks` → fail; `samples > 1` → majority verdict (ties uncertain), averaged scores; any failure (crash, budget, bad JSON, spawn error) → `uncertain`. **Live 2026-09-15 (haiku):** known-good diff → pass 10/10/10/10 (0.053 USD); known-bad diff (wrong text, skipped test, unrelated edits) → fail, `gamed_checks: true` (0.031 USD). |
| 10 routing | done | `internal/eval/route.Decide` pure function; 336-row exhaustive table test over checks × verdict × gamed × attempt × max_retries × threshold × score. Review phases (§6) route to `HumanReview` when they pass. |
| 11 retry/resume | done | Each attempt is its own `Run` (`attempt` per phase, `retry_of` chain, `prompt` override); `failed` run + new `continue` run whose prompt is `feedback.Render` (verbatim check evidence + judge reasoning + reviewer note). Cold retry (`new`, fresh UUID, original prompt prepended) when no snapshot exists or the CLI reports the transcript lost. Integration tests (a) seeded test failure → resumed retry passes in a fresh workspace, (b) SIGKILL mid tool call → resumed from the same id, (c) snapshot deleted → cold retry, all green. `orchestrator.SessionSweeper` deletes snapshots of tasks terminal for longer than `retention.sessions_days`. |
| 12 review queue | done | `internal/review` (Channel / Store / Decider seams, `LogChannel` fallback), `internal/review/slack` (Block Kit post with Approve / Reject… / Close, `views.open` modal for the reject comment, `chat.update` after the decision, signed `POST /slack/actions` with 5-minute replay window). `review_posts` + `review_decisions` tables (migration 0002); `review.Escalator` re-posts past `review.sla_hours` with `review.escalation_mention`. Non-Slack paths: `POST /runs/{id}/review`, `GET /runs/{id}/decisions`, `harness review -run … approve\|reject\|close [-process]`. Slack itself is untested end to end (no workspace/token in this environment); the Web API calls are covered against a fake server. |
| 13 multi-phase | done | Kind `code_fix_planned`; `templates/<kind>/phases/<n>/{phase.json,prompt.md.tmpl}` merged onto the kind policy at submission into `task.phases[]` (fully materialised per phase), `task.phase` + `run.phase`. Phase 1 is `Read/Glob/Grep`-only with a plan JSON schema and `expect_changes: false`; its `structured_output` is stored on the run and shown in the review post. Approve → `run.phase+1` as a `continue` run whose prompt is the phase template rendered with the plan and the reviewer's note. `scripts/demo-m2.sh` runs plan → `harness review approve -process` → implement → judge → branch push. |

Decisions made while building M2 (confirm or change):

- **`max_retries` counts retries after the first attempt** (`attempt <= max_retries` → retry): a policy of 2 allows three runs. The design's §5.3 snippet (`attempt < max_retries`) would make `max_retries: 1` mean "never retry"; the M1 tests already encoded the retry-count reading. Per phase for multi-phase tasks.
- **A retry is a new run record**, not the same run cycling `failed → queued`; the old run stays `failed` (or goes `closed` when a human rejected it) with a `retry:<run_id>` artifact, so run ids, audit logs and workspaces stay one-to-one. The `failed → queued` edge is still used for the api_error requeue.
- **Human reject → old run `closed`** (reason records who and why, `review_decisions` keeps the record) and a new `continue` run with the comment as feedback. A human retry is not bounded by `max_retries`; the router applies the bound again on that run's own failure.
- **Workspaces of `needs_review` runs are kept on disk** until the decision; approve reopens them (`workspace.Manager.Open`) to commit and push. A host restart keeps them (they live under `data_root/workspaces`), a cleaned tmp does not → approve fails with a clear error and the run stays reviewable (close it).
- **The judge sees the worker's final message** (`result.result`), labelled as a claim to verify against the diff. It still never sees the event stream. Drop it if sycophancy shows up in the golden suite.
- **Judge failures never pass:** no judge configured, spawn error, crash, budget, invalid rubric output all become `uncertain` → human review. Layer 2 is skipped entirely when Layer 1 fails (§5 short-circuit) or `policy.judge.enabled` is false (the plan phase).
- **`gamed_checks` majority rule** with `samples > 1`: more than half the valid samples must flag it; a single sample flagging it is decisive when `samples == 1`.
- **Session snapshots double as the audit artifact** until `retention.sessions_days` elapses after the task is terminal; the sweeper then deletes the task's live directory. No separate archive copy.
- **`Policy.Merge` / `Acceptance.Merge` deep-copy through JSON.** The M1 version decoded the override into a struct copy that shared the base's slice headers, so a phase override rewrote the kind policy's `allowed_tools` in place (found by the multi-phase test).
- **CLI 2.1.270 findings** (cli-contract §5): `structured_output` is an object; `--max-turns 1` reports `num_turns: 2`; `dontAsk` denials land in `permission_denials[]` with the run still `completed`; **`Bash(echo hi)` runs even with only `Read` allowed** (built-in safe-command auto-approval), so `--allowedTools` is not a read barrier.

---

## Status — M1 walking skeleton built 2026-09-13

Steps 1, 2, 3, 5, 6, 7, 8 were implemented and tested on 2026-09-13.

| Step | State | Notes |
|---|---|---|
| 1 scaffold | done | Go 1.27.1 + golangci-lint 2.13 installed via Homebrew. Module `github.com/100xteam-ai/foreman`. `harness doctor` checks Go, git, pinned CLI, API key, GitHub token, webhook secret, data root. |
| 2 domain types | done | `internal/task`: Task/Run/Policy/Acceptance/Spec, ULID + UUIDv4 ids, exhaustive state-machine table test, round-trip tests against the design's §3 documents. |
| 3 store | done | `internal/store/sqlite` (modernc, WAL, embedded migrations, `run_events` history). Illegal transitions rejected at the store boundary. |
| 4 spike | partial | unchanged; see §0 / docs/cli-contract.md §4. |
| 5 runner | done | Args builder golden tests (all four session modes, both illegal combos). Decoder replays every fixture. Price table reproduces the CLI's `costUSD` exactly on all four result fixtures. Process-group kill verified (tool child dies). Snapshot in `defer` verified after SIGKILL. **Live test passed 2026-09-13** with `CLAUDE_CODE_OAUTH_TOKEN` (haiku, 1 turn, cost 0.0162, token estimate 0.0151). Runner aborts on the first `system/api_retry` 401/403 instead of waiting out the CLI's ten retries (fixture `stream_auth_failed_401.ndjson`). |
| 6 checks | done | All seven §5.1 rows. Acceptance commands are split with go-shellwords and **refuse shell operators** (`;`, `|`, `>`): run a script instead. Read-only kinds fail `diff_sanity` if they change files. |
| 7 workspace | done | `git clone --depth 50 --single-branch`, bot branch `harness/<task_id>`, token via `GIT_ASKPASS` + env only (never on disk). Push takes an explicit `--force-with-lease=<branch>:<remote sha>` because single-branch clones have no tracking ref for the bot branch. |
| 8 M1 | done (local) | SQLite queue with leases/heartbeat, pool with global + per-kind caps and drain-on-shutdown, HTTP intake (`POST /tasks`, GitHub `issues` webhook with HMAC, `GET /runs/{id}`, `/events` NDJSON stream, `/healthz`), robfig cron, GitHub PR upsert (go-github v80) with branch-push fallback for non-GitHub repos. `scripts/demo-m1.sh` passes offline with a fake CLI. **`REAL=1` passed 2026-09-13**: real haiku worker added `Farewell` + test, 6 turns, 16 s, cost 0.073, all seven checks pass, branch pushed to the local origin, run `delivered`. `REPO=org/name` + `GITHUB_TOKEN` opens a draft PR instead. **GitHub path exercised 2026-09-15** on sandbox `iceteahh/test-harness` (fine-grained PAT for `iceteahh`, `CLAUDE_CODE_OAUTH_TOKEN` worker): `run-once` → draft PR #1; a real issue labelled `harness`, delivered to a local `harness serve` as a signed `issues.labeled` payload → PR #3 (bad signature → 401). Only GitHub's own delivery to a public endpoint remains untested. Fixes from that run: git credential helpers bypassed (`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`, `-c credential.helper=`) so the token, not the operator's Keychain login, pushes; `task.Title` (issue title) drives PR/commit titles; PR body gets `Closes #N`; config `*bool` defaults no longer share one pointer (`check_version: false` used to disable draft PRs). |

Decisions made while building (confirm or change):

- **`HOME` is passed through unchanged** to workers alongside `PATH`, `CLAUDE_CONFIG_DIR`, `ANTHROPIC_API_KEY` and `env:` from harness.yaml. The design's allowlist omits `HOME`; git/ssh inside tool calls need it, and `CLAUDE_CONFIG_DIR` is what relocates CLI state. Never overridden.
- **`api_error` with zero cost** → run goes `failed → queued` on the same run (no new attempt) and the job is nacked with `queue.api_error_backoff_seconds`; after `queue.max_job_attempts` it dead-letters (`dead`). Same path for provision failures.
- **Checks fail in M1** → `failed`; when `attempt > max_retries` → `dead`. Retry-with-feedback is Step 11.
- **Delivery failure** → run stays `passed`, workspace is kept on disk and recorded as `workspace:<path>` artifact for an operator; job acked. Step 18 adds `harness requeue`.
- `--max-turns` treated as reached when `num_turns >= max_turns` or `terminal_reason == max_turns` (**verified 2026-09-15**: the string is `max_turns`, `subtype` is `error_max_turns`, and `num_turns` is one above the cap).
- Per-kind concurrency is enforced with in-process semaphores plus a kind filter on `Lease`; one orchestrator process only until Step 22.
- **CLI drift:** the host CLI auto-updated to **2.1.270** on 2026-09-15. Three live runs (two issue fixes, one trivial prompt) behaved exactly per the 2.1.243 contract (same flags accepted, same stream shape, `result` fields unchanged). **Resolved 2026-09-17:** the session spike was re-run in full on 2.1.270 and the pin was bumped (docs/cli-contract.md §7). The worker image still has to be rebuilt on the new pin before docker mode is trusted.
- **Webhook tasks have no acceptance commands** (`templates/code_fix/acceptance.json` is language-agnostic), so `acceptance_commands` is `n/a` for them and only the worker's own test run guards correctness. Follow-up: repo-level `.harness.yaml` (acceptance commands, diff scope) read at intake.
- Webhook-created tasks use the template policy, which sets no `model`, so they run on the CLI's default model (PR #3 cost 0.115 vs 0.046 for the haiku run). Set `policy.model` in the template or per repo if cost matters.

---

## 1. Step map

Steps are ordered so every one ends with something runnable and testable. Milestones from the design map as: M1 = Steps 1–8, M2 = 9–13, M3 = 14–18, M4 = 19–20, M5 = 21–22.

```
S1 scaffold ─▶ S2 domain types ─▶ S3 store ─▶ S4 runner spike ─▶ S5 runner ─▶ S6 checks
                                                                     │
S7 workspace ─▶ S8 queue+orchestrator+intake+PR delivery  ◀──────────┘   ══ M1 walking skeleton
S9 judge ─▶ S10 routing ─▶ S11 retry/resume ─▶ S12 review queue ─▶ S13 multi-phase   ══ M2
S14 container workers ─▶ S15 budgets ─▶ S16 observability ─▶ S17 audit→S3 ─▶ S18 DLQ/paging  ══ M3
S19 golden suite ─▶ S20 regression gate + task kinds 2,3   ══ M4
S21 fan-out ─▶ S22 scaled topology   ══ M5
```

---

## 2. Steps

### Step 1 — Repo scaffold and toolchain — **done 2026-09-13**
**Goal:** `make test` passes on an empty module.
- Install Go (1.23+). `go mod init github.com/<org>/foreman`.
- Layout:
  ```
  cmd/harness/           single binary: `harness serve | run-once | eval`
  internal/task          Task/Run types, state machine
  internal/store         persistence interface + sqlite impl (postgres later)
  internal/queue         queue interface + sqlite impl (redis/river later)
  internal/workspace     clone/provision/destroy
  internal/runner        spawn + supervise `claude -p`
  internal/events        NDJSON event types + parser
  internal/audit         append-only event log sink
  internal/eval/checks   deterministic checks
  internal/eval/judge    LLM judge
  internal/eval/route    verdict routing
  internal/deliver       PR / report / Slack adapters
  internal/review        human review queue
  internal/intake        HTTP API, webhooks, cron
  internal/orchestrator  worker pool, retries, budgets
  internal/config        harness.yaml
  templates/<kind>/      prompt.md.tmpl, policy.json, acceptance.json
  evals/golden/          golden task set
  testdata/events/       captured NDJSON fixtures
  ```
- Makefile (`build`, `test`, `lint` with golangci-lint), `.golangci.yml`, CI workflow, `CLAUDE.md` for this repo.
- Pin CLI version in `internal/runner/version.go`; `harness doctor` checks `claude --version` matches.
- **Done when:** CI green, `harness doctor` reports Go + CLI + git.

### Step 2 — Domain types and run state machine — **done 2026-09-13**
**Goal:** the design's §3 schemas as Go types with validation and a tested state machine.
- `task.Task`, `task.Policy`, `task.Acceptance`, `task.Run`, `task.RunStatus` (`queued running evaluating passed failed needs_review dead delivered closed`).
- `task.Transition(from, to) error` encoding the §3.2 diagram exactly; table-driven test for every legal and illegal edge.
- Policy defaults per kind loaded from `templates/<kind>/policy.json`; per-task override merge.
- ULID ids (`tsk_`, `run_`). Session id = UUIDv4 minted at run creation, never reused. `run.SessionID`, `run.SessionMode` (`new | continue | fork | ephemeral`), `task.SessionID` (resume pointer, updated when a run completes or forks).
- **Done when:** JSON round-trip tests match the design's example documents.

### Step 3 — Store (SQLite) — **done 2026-09-13**
**Goal:** durable tasks, runs, events index.
- `store.Store` interface: `CreateTask`, `CreateRun`, `UpdateRunStatus` (enforces `task.Transition`), `GetRun`, `ListRunsByStatus`, `RecordMetrics`, `RecordEval`.
- SQLite via `modernc.org/sqlite` (no cgo). Migrations embedded with `embed`. WAL mode.
- Keep the interface Postgres-ready: no SQLite-specific SQL outside the driver file.
- **Done when:** integration tests on a temp DB; a status transition test proves illegal edges are rejected at the store boundary.

### Step 4 — Runner spike (throwaway, 1 day) — **session part done 2026-09-13**
**Goal:** answer the open CLI questions before writing the real runner. Answers live in `docs/cli-contract.md`.
1. ~~Capture fixtures~~ **Done 2026-09-15** (`testdata/events/`): success, resumed, forked, `--max-budget-usd` breach, not-logged-in, SIGKILL mid tool call, three stderr errors, and (on CLI 2.1.270) `--max-turns` exhaustion, permission denial, `--json-schema` structured output, plus the `Bash(echo hi)` safe-command surprise (cli-contract §5).
2. ~~Confirm `--resume` from a restored transcript~~ **Done.** Works from any cwd, under any slug; `--fork-session` (+ `--session-id`) works for fan-out.
3. Confirm `--bare --add-dir <ws>` still loads workspace `CLAUDE.md`; if not, use `--setting-sources project --strict-mcp-config` instead. **Blocked on an `ANTHROPIC_API_KEY`** (`--bare` disables Keychain auth).
4. ~~Confirm `--session-id` + crash leaves a resumable transcript~~ **Done.** It does, same id.
5. Measure stderr content on 429 / auth failure for the rate-limit detector. Auth failure captured (`result_not_logged_in.json`); 429 still open.
- **Done when:** every row of §0 has a verified answer and a fixture (three rows remain).

### Step 5 — Runner — **done 2026-09-13** (live test not run)
**Goal:** `runner.Run(ctx, task, run) (Result, error)` spawning `claude -p` per the corrected contract.
- Args builder (`internal/runner/args.go`) as a pure function with golden tests:
  ```
  claude -p <prompt> --output-format stream-json --verbose --include-partial-messages
    --allowedTools <csv> --max-turns N --max-budget-usd X
    --permission-mode dontAsk
    --setting-sources project --strict-mcp-config [--json-schema <schema>] [--model <m>]
    + one of (by run.SessionMode):
      new:        --session-id <uuid>
      continue:   --resume <uuid>
      fork:       --resume <parent> --fork-session --session-id <uuid>
      ephemeral:  --session-id <uuid> --no-session-persistence
  ```
  `--max-budget-usd` = `worker.budget_flag_ratio` × policy ceiling (the CLI checks the cap after the turn that crosses it). Prompt passed via stdin or positional; never via shell string interpolation. Golden tests include the two illegal combinations (`--session-id` with `--resume`, id reuse) being rejected by the builder.
- `session.Store` interface: `Restore(ctx, taskID, sessionID, configDir) error`, `Snapshot(ctx, taskID, sessionID, configDir) error`, `Delete(ctx, taskID)`. `session.Local` writes `<root>/<task_id>/<session_id>.jsonl`; restore copies into `<configDir>/projects/<slug>/`. Runner calls `Restore` before spawn (continue/fork) and `Snapshot` in a `defer` so crashes and timeouts still persist the transcript.
- `events.Decoder`: line reader with max line size, tolerant of non-JSON lines, typed `Event` union (`system`, `assistant`, `user`, `stream_event`, `rate_limit_event`, `result`). Unknown types kept as raw JSON, never dropped.
- Supervision: `exec.CommandContext` with `SysProcAttr{Setpgid: true}`; wall-clock deadline → SIGTERM to the **process group** (`syscall.Kill(-pgid, …)`), grace period, SIGKILL. Cost backstop: `assistant.message.usage` has tokens only, so estimate spend as tokens × a pinned per-model price table (`internal/runner/pricing.go`) and kill the group if the estimate exceeds policy; `result.total_cost_usd` is the authoritative figure. Detect "exited without `result` event" as a crash. Classify results on `is_error` + `terminal_reason`, not `subtype`. Empty stdout + one stderr line = CLI usage/session error (fixtures `stderr_*.txt`); `No conversation found` marks the session lost.
- Env: minimal allowlist (`PATH`, `CLAUDE_CONFIG_DIR`=empty per-run dir, `ANTHROPIC_API_KEY`), nothing inherited from the orchestrator process. Never override `HOME`.
- `audit.Sink` interface; `audit.FileSink` writes `<run_id>.ndjson` locally (S3 in Step 17).
- **Done when:** unit tests replay every Step 4 fixture through the decoder; one live integration test (build-tagged) runs a trivial prompt end to end.

### Step 6 — Deterministic checks (Layer 1) — **done 2026-09-13**
**Goal:** `checks.Run(ctx, ws, task, result) checks.Report` with each §5.1 row as its own `Check`.
- Checks: `ExitCode`, `TurnExhaustion`, `PermissionDenials` (policy lists task-critical tools), `AcceptanceCommands` (run in workspace, capture output to `eval/`), `DiffScope` (`git diff --name-only` vs globs via `doublestar`), `DiffSanity` (empty diff on change task; deleted/weakened `*_test.*` files), `SchemaValidation` (`santhosh-tekuri/jsonschema`).
- Each check returns `pass | fail | n/a` + evidence text (this becomes retry feedback in Step 11).
- **Done when:** table-driven tests on fixture workspaces (small git repos built in `t.TempDir()`).

### Step 7 — Workspace manager (local) — **done 2026-09-13**
**Goal:** `workspace.Provision(task) (Workspace, error)` and `Destroy`.
- `git clone --depth 50 <repo> --branch <ref>` into `<root>/<run_id>/`; bot branch `harness/<task_id>` created.
- Credentials: token injected via `GIT_ASKPASS` helper script, never written to the workspace or `.git/config`.
- `Capture()` returns diff, changed files, and acceptance outputs before `Destroy()`.
- Container isolation deferred to Step 14; interface designed for it (`Exec(ctx, cmd)` is how checks run commands).
- **Done when:** tests against a local bare repo.

### Step 8 — Queue, orchestrator, intake, PR delivery (M1 complete) — **done 2026-09-13**, live webhook→PR not yet exercised
**Goal:** webhook → queue → one worker → checks → PR, on SQLite only.
- `queue.Queue` interface (`Enqueue`, `Lease(ctx, kinds) (*Job)`, `Ack`, `Nack(delay)`, `DeadLetter`). SQLite impl using lease columns (`leased_until`, `attempts`); Redis/River later.
- `orchestrator.Pool`: N goroutines pulling jobs, honouring `concurrency.global` and `per_kind`; graceful shutdown drains in-flight runs.
- `intake`: `POST /tasks` (validated task record), `POST /webhooks/github` (renders prompt from `templates/code_fix/prompt.md.tmpl`), `GET /runs/{id}`, `GET /runs/{id}/events` (streams the audit file). Cron via `robfig/cron` reading `harness.yaml`.
- `deliver.GitHubPR`: idempotent upsert keyed by branch `harness/<task_id>` (find open PR by head branch → update, else create) via `go-github`. Draft PR when checks pass; body includes check report.
- Layer 2/3 stubbed: checks pass → `passed` → deliver; checks fail → `failed` (no retry yet).
- **Done when:** `harness serve` + a real webhook on a sandbox repo produces a PR. Demo script in `scripts/demo-m1.sh`.

### Step 9 — LLM judge (Layer 2) — **done 2026-09-15**
**Goal:** `judge.Evaluate(ctx, ws, task, checks) (Verdict, error)`.
- Fresh `claude -p` in the workspace: `--output-format json --allowedTools Read --permission-mode dontAsk --json-schema <rubric>` `--max-turns` small, `--max-budget-usd` small. Model configurable per kind (`judge.model`).
- Prompt assembled from task spec, `git diff <base>`, acceptance outputs, check summary. Never the worker transcript.
- Parse `structured_output`; validate against rubric schema independently; `gamed_checks: true` → fail.
- `samples > 1` → run sequentially or in parallel, majority verdict, average scores.
- Judge failures (timeout, bad JSON) → verdict `uncertain`, never `pass`.
- **Done when:** fixture-driven tests with a fake runner; one live test on a known-good and a known-bad diff.

### Step 10 — Routing (Layer 3) — **done 2026-09-15**
**Goal:** `route.Decide(task, run, checks, judge) Decision` as a pure function, exactly §5.3.
- Decisions: `Deliver`, `Retry{Feedback}`, `DeadLetter`, `HumanReview`.
- Orchestrator applies the decision and transitions run status through the store.
- **Done when:** exhaustive table test over `checks × verdict × attempt × threshold`.

### Step 11 — Retry with feedback via `--resume` — **done 2026-09-15**
**Goal:** failed run → new run (attempt+1) resuming the same session with the evidence as prompt.
- `feedback.Render(checks, judge)`: verbatim check logs + judge reasoning + "fix these issues".
- Session persistence: `session.Store` snapshot/restore from Step 5 (design §4.4). No session volume, no stable workspace path required; each attempt gets a fresh workspace and a fresh `CLAUDE_CONFIG_DIR`.
- Timeout or crash without `result` → `continue` from the same session (verified resumable). `Restore` failure or `No conversation found` → cold retry with a new UUID; log the lost session.
- Session lifecycle: `task.SessionID` updated from `result.session_id` after every run; on a terminal task state the orchestrator calls `session.Store.Delete` after `retention.sessions_days`, keeping the last snapshot as an audit artifact.
- **Done when:** integration tests: (a) run 1 fails a seeded test, run 2 resumes in a new workspace and passes; (b) run 1 is SIGKILLed mid-run, run 2 resumes; (c) transcript deleted between runs → cold retry with a new id.

### Step 12 — Human review queue (Slack) — **done 2026-09-15** (Slack end-to-end not exercised)
**Goal:** `needs_review` runs post to Slack; approve/reject/close drive the state machine.
- `review.Slack`: Block Kit message with diff summary, judge reasoning, scores, buttons. Interactive endpoint `POST /slack/actions` verifies signature.
- Approve → `delivered` via delivery adapter; Reject (with comment) → `Retry{Feedback: comment}`; Close → `closed`.
- SLA escalation job: unanswered past `review.sla_hours` re-posts with @-mention.
- Every override logged to `review_decisions` table (feeds Step 19).
- **Done when:** local test with Slack request fixtures; manual end-to-end in a test channel.

### Step 13 — Multi-phase workflow (plan → approve → implement) — **done 2026-09-15**
**Goal:** the §6 pattern on top of Steps 11–12.
- Task kind `code_fix_planned`: phase 1 policy `--json-schema plan.json`, tools `Read` only; result stored as artifact and posted to review.
- Approve → enqueue phase 2 run with `--resume <session>` and prompt "Plan approved — implement step by step."; phase 2 flows through Steps 6–10.
- `task.Phase` field and `templates/<kind>/phases/` layout.
- **Done when:** demo script runs both phases against the sandbox repo.

### Step 14 — Containerised workers — **done 2026-09-15**
**Goal:** one container per run, fresh clone inside, network allowlist.
- `worker/Dockerfile`: pinned Node + pinned `@anthropic-ai/claude-code`, git, language toolchains per kind; non-root user.
- `workspace.Docker` implementing the Step 7 interface: run container with the workspace mounted and an empty per-run `CLAUDE_CONFIG_DIR` (tmpfs or bind mount the runner can read back for `Snapshot`), `--network` on a restricted bridge (egress allowlist via proxy or iptables), no secrets on disk (API key via env only, repo token via `GIT_ASKPASS` inside container).
- Runner spawns `docker run … claude -p …` (or `docker exec`) and consumes stdout the same way.
- **Done when:** M1 demo passes with `worker.mode: docker`; a test proves `curl` to a non-allowlisted host fails inside the worker.

### Step 15 — Budgets — **done 2026-09-15**
**Goal:** the three ceilings in §8.
- Per-run: `--max-budget-usd` + runner backstop (already in Step 5).
- Per-kind daily and global daily: `budget.Ledger` in the store; intake rejects (429 + reason) and orchestrator stops leasing a kind when its ceiling is hit; global breach opens a circuit breaker and pages.
- **Done when:** tests with a fake clock; alert fires in Step 16 metrics.

### Step 16 — Observability — **done 2026-09-15**
**Goal:** metrics, traces, structured logs, live progress.
- `slog` JSON logs with `run_id`/`task_id` everywhere.
- OTel metrics: runs by status, queue depth/age, duration p50/p95, turns, cost per run and per kind per day, permission-denial rate, retry rate, judge/human disagreement.
- One trace per run spanning intake → queue → worker → checks → judge → route → deliver.
- Progress emitter: parsed events → Slack thread per run (tool calls, files edited, cost ticking).
- Grafana dashboard JSON committed under `deploy/grafana/`.
- **Done when:** local docker-compose with OTel collector + Grafana shows a run.

### Step 17 — Audit to object storage — **done 2026-09-15**
**Goal:** `audit.S3Sink` with retention.
- Stream NDJSON to S3 (or MinIO locally) under `runs/<run_id>.ndjson`; bucket policy for WORM/retention `audit_ndjson_days`.
- `harness replay <run_id>` prints a human-readable timeline from the log.
- **Done when:** MinIO integration test; replay works on a Step 4 fixture.

### Step 18 — Dead-letter handling and paging — **done 2026-09-15**
**Goal:** exhausted runs are visible and actionable.
- `dead` runs → DLQ table + Slack/PagerDuty alert with last feedback and replay link.
- `harness requeue <run_id>` re-enqueues with a fresh attempt counter (operator action).
- Rate-limit detection (429 in events/stderr) → exponential backoff + temporary global concurrency shed.
- **Done when:** chaos test: fail a task 3× and observe DLQ + alert.

### Step 19 — Golden suite (Layer 4) — **done 2026-09-16** (suite not yet run against the real CLI)
**Goal:** `harness eval run evals/golden` produces a scored report.
- 20–50 `evals/golden/*.json`: task + fixture repo (as a git bundle) + expected outcome (`pass|fail|needs_review`, expected files touched).
- Runner executes them with the real pipeline (containers on), records pass rate, retry rate, avg turns/cost/duration, judge/human agreement.
- Script to convert `review_decisions` overrides into new golden cases.
- **Done when:** suite runs in CI nightly on a budget cap and writes `evals/reports/<date>.json`.

### Step 20 — Regression gate and task kinds 2 & 3 — **done 2026-09-16** (gate exercised offline, not yet in a paid CI run)
- Gate: PRs touching `templates/`, `CLAUDE.md`, tool policies, or the pinned CLI/model must pass the golden suite with pass-rate drop ≤ configured delta.
- Add `code_review` (read-only, structured-output report) and `report`/`triage` kinds with their templates, policies, acceptance, and golden cases.
- **Done when:** a deliberately bad prompt change is blocked by CI.

### Step 21 — Fan-out workflows — **done 2026-09-16** (never run against a real planner)
- Planner run (`--json-schema subtasks.json`) → orchestrator enqueues N child runs (`new`, or `fork`: `--resume <planner> --fork-session --session-id <child>`, each restored from the planner's snapshot) → synthesizer run merges results. `task.ParentID`, `task.Children` in the store; parent completes when all children terminal.
- **Done when:** demo decomposes a 3-file refactor into 3 parallel workers and one synthesizer. → `scripts/demo-m5.sh`, offline.

### Step 22 — Scaled topology — **done 2026-09-16** (no cluster: the "done when" is not met)
- Postgres store (`pgx`), queue on River or SQS behind the Step 8 interface, workers as Kubernetes Jobs (one per run) with `session.S3` for transcript snapshot/restore (no PVC), stateless orchestrator replicas with leader-free leasing.
- Helm chart under `deploy/k8s/`.
- **Done when:** golden suite passes on the k8s deployment. → **not met.** The suite has no priced baseline and no cluster has run it. `make postgres-test` and `make k8s-validate` are what stands in for it today; see the M5 status section.

---

## 3. Cross-cutting rules

- **No LLM calls in orchestration code.** Only `runner` and `judge` spawn `claude`.
- **Interfaces first at every seam** that changes across milestones: `store`, `queue`, `workspace`, `audit.Sink`, `deliver.Adapter`, `review.Channel`.
- **Every step adds fixtures** under `testdata/` so later steps test without spending tokens; live tests are build-tagged `//go:build live`.
- **Pin and check**: CLI version, model id, worker image digest. `harness doctor` fails on drift.
- **Never** pass user-controlled text through a shell; always `exec.Command` argv.

## 4. Open decisions to confirm with the team

1. Go confirmed as the implementation language (repo name says so; design says Node/Python).
2. ~~Session persistence strategy for `--resume` across containers~~ **Resolved 2026-09-13:** snapshot/restore of the JSONL via `session.Store` (design §4.4). Remaining knob: `retention.sessions_days` default (proposed 30).
3. ~~Queue technology for M3~~ **Deferred to Step 22:** the SQLite queue behind `queue.Queue` carried M3 unchanged, so the choice (River vs Redis/asynq vs SQS) only matters for the scaled topology.
4. Judge model per task kind and whether `samples=3` is default for `code_fix`.
5. Whether workers run under `--bare` (stronger isolation) or `--setting-sources project` (keeps CLAUDE.md discovery). Needs an `ANTHROPIC_API_KEY` to test: `--bare` disables Keychain auth, and the host login is Keychain-only. **Less urgent since Step 14:** a container with an empty `CLAUDE_CONFIG_DIR` already inherits nothing from the host user.
6. Worker image size is 1.36 GB (Node + Go + git). Split per task kind, or accept it? A pull happens once per host.
7. `retention.audit_ndjson_days` is enforced by the bucket policy, not the harness. The compose stack sets Object Lock COMPLIANCE 365d + a matching lifecycle rule; confirm that matches the real retention requirement before production.
6. ~~Worker credential for local development~~ **Resolved 2026-09-13:** both work under an isolated `CLAUDE_CONFIG_DIR`. `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`, prefix `sk-ant-oat01-`) verified live: completed, `total_cost_usd` 0.016. Putting that token in `ANTHROPIC_API_KEY` yields HTTP 401 `authentication_failed`, which the CLI retries 10× (~3 min) before a `result` with `is_error: true`; the runner now aborts on the first 401/403 `system/api_retry` (`KillAuth`). Config: `worker.oauth_token_env`.
