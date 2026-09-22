# foreman

A production harness that runs autonomous agent tasks by spawning Claude Code headless
(`claude -p`) as isolated worker subprocesses.

The design thesis: `claude -p` is already a stateless, composable unit of agent work with tool
use, file editing, bash, MCP and permission controls. This repo does not build an agent loop —
it builds the deterministic plumbing around one: durable queueing, session persistence,
sandboxing, budget enforcement, multi-layer evaluation, retry-with-feedback, a human review
gate and a replayable audit trail.

- System design: [claude-p-agent-harness-design.md](claude-p-agent-harness-design.md)
- Build plan and status: [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md)
- Verified CLI behaviour (the contract everything is built against): [docs/cli-contract.md](docs/cli-contract.md)
- Evaluation suite: [evals/README.md](evals/README.md)

## How a task flows

```
trigger (HTTP · GitHub webhook · cron · CLI)
  → intake normalises it into a Task
  → queue (durable, leased, dead-lettered)
  → orchestrator leases a job, provisions a fresh workspace
  → runner spawns `claude -p`, streams NDJSON events
  → evaluation: Layer 1 checks → Layer 2 judge → Layer 3 routing
        pass      → deliver (draft PR / branch / structured report)
        fail      → retry with feedback via `--resume`
        uncertain → human review (Slack Block Kit, or `harness review`)
```

## Run states

```
queued ──▶ running ──▶ evaluating ──┬──▶ passed ───────▶ delivered
                                    │
                                    ├──▶ failed ───┬──▶ queued (retry with feedback)
                                    │              └──▶ dead   (retries exhausted → DLQ)
                                    │
                                    └──▶ needs_review ─┬──▶ delivered (approve; or the next phase)
                                                       ├──▶ queued    (reject → retry with the comment as feedback)
                                                       └──▶ closed    (close)
```

| State | Meaning |
|---|---|
| `queued` | The run is in the durable queue waiting for a worker lease. |
| `running` | A worker is executing `claude -p`; the runner is streaming its NDJSON events. |
| `evaluating` | The worker exited; Layer 1 checks, then the Layer 2 judge, then Layer 3 routing. |
| `passed` | Evaluation accepted the work. |
| `failed` | Evaluation rejected it — a check failed, the judge scored below threshold, or the worker errored. |
| `needs_review` | Evaluation was `uncertain`; a human decides with `approve` / `reject` / `close` (Slack buttons or `harness review`). |
| `delivered` | The deliverable shipped: a draft PR, a branch, or a structured report. Terminal. |
| `dead` | Retries are exhausted or the budget tripped; the run sits in the dead-letter queue until `harness requeue`. Terminal. |
| `closed` | A reviewer closed the run; nothing ships and nothing retries. Terminal. |

Only these edges are legal — the state machine in [internal/task/state.go](internal/task/state.go)
rejects anything else, so an illegal transition is a bug, not a state.

**Every attempt is its own `Run`.** A retry does not mutate the previous run: it creates a new one
with `attempt` incremented (it counts within a phase, bounded by `policy.max_retries`) and
`retry_of` pointing at the run it follows, so the whole chain stays auditable. `harness replay
<run_id>` prints one run's event stream as a timeline; `harness tree <task_id>` prints the task,
its fan-out children and every run.

**The task carries the session id used as the resume pointer.** `task.session_id` is the id the
harness minted for the task's transcript — never the id echoed back on the CLI's `result` event.
A retry resumes it (`--resume`, session mode `continue`) when a snapshot exists, and otherwise
starts cold with a fresh UUID (`--session-id`, mode `new`). Fan-out children fork it
(`--resume <planner> --fork-session --session-id <new>`), and the judge runs ephemerally so it
never touches the task's session.

## Quick start

```bash
cp .env.example .env          # fill in a worker credential (see Credentials)
cp harness.example.yaml harness.yaml
make build                    # → bin/harness
bin/harness doctor            # checks Go, git, the pinned CLI, docker, credentials, Slack
```

Run the offline walking-skeleton demo — it uses a fake `claude` replaying a captured fixture, so
it spends no tokens:

```bash
scripts/demo-m1.sh            # REAL=1 uses the installed CLI and spends tokens
```

Submit one task and process it to completion:

```bash
bin/harness run-once -task task.json
```

```json
{
  "kind": "code_fix",
  "prompt": "Add a Farewell(name string) function and a test for it.",
  "workspace": { "type": "git", "repo": "org/service", "ref": "main" },
  "policy": {
    "max_turns": 15,
    "max_cost_usd": 0.5,
    "allowed_tools": ["Read", "Edit", "Write", "Glob", "Grep", "Bash(go test *)"],
    "judge": { "enabled": true, "samples": 1, "threshold": 7 }
  },
  "acceptance": { "commands": ["go test ./..."], "diff_scope": ["**/*.go"] },
  "requested_by": "me"
}
```

Or run the server (HTTP intake, cron scheduler, worker pool):

```bash
bin/harness serve -config harness.yaml
```

## Credentials

`.env` is gitignored and loaded from the working directory by `bin/harness`, `make` and the demo
scripts; existing environment variables win. Template: [.env.example](.env.example).

| Variable | Purpose |
|---|---|
| `CLAUDE_CODE_OAUTH_TOKEN` | Worker credential from `claude setup-token` (`sk-ant-oat01-…`) |
| `ANTHROPIC_API_KEY` | Alternative worker credential (`sk-ant-api03-…`) |
| `HARNESS_API_TOKEN` | Bearer token for the HTTP API; `serve` refuses to start without it unless `server.addr` is loopback |
| `GITHUB_TOKEN` | Repo clone and draft PRs |
| `GITHUB_WEBHOOK_SECRET` | Verifies `POST /webhooks/github` |
| `SLACK_BOT_TOKEN` / `SLACK_SIGNING_SECRET` | Review posts and button callbacks |
| `HARNESS_LOG` | `debug` \| `info` \| `warn` \| `error` |

One of the two worker credentials is required. An OAuth token placed in `ANTHROPIC_API_KEY`
returns 401 — the two variables are not interchangeable.

## Commands

```
harness serve      HTTP intake, cron scheduler and worker pool
harness run-once   submit one task from a JSON file and process it to completion
harness review     -run <id> [-comment …] [-process] approve|reject|close
harness egress     the worker egress allowlist proxy (worker.mode: docker)
harness replay     print a run's event stream as a timeline (-file replays a fixture)
harness tree       print a fan-out parent, its children and every run as one tree
harness requeue    re-enqueue a dead run with a fresh attempt counter (-list shows the DLQ)
harness budget     today's spend against the daily ceilings
harness doctor     check Go, git, the pinned CLI, docker, the internal network, credentials
harness eval       run the golden suite, gate a change against a baseline, or list cases
harness version    harness and pinned CLI versions
```

## HTTP API

```
GET  /healthz
POST /tasks                     submit a task (same shape as run-once -task)
GET  /tasks/{id}                task record
GET  /tasks/{id}/runs           every attempt
GET  /tasks/{id}/children       fan-out children
GET  /runs                      list runs
GET  /runs/{id}                 one run
GET  /runs/{id}/events          the NDJSON event stream
POST /runs/{id}/review          approve | reject | close
GET  /runs/{id}/decisions       the review audit trail
GET  /budget                    today's spend against the ceilings
POST /webhooks/github           issue/label triggers (HMAC-verified)
```

Every route needs `Authorization: Bearer $HARNESS_API_TOKEN` except `GET /healthz` (probes) and
`POST /webhooks/github`, which keeps its HMAC because GitHub cannot send a bearer token. The
token's variable is named by `server.api_token_env`. `serve` refuses to start without one unless
`server.addr` is loopback: this listener is the one the webhook needs exposed, and an open API
lets anyone submit a write-capable task or approve any run.

## Task kinds

Change kinds produce a branch or draft PR; read-only kinds get no write tool and deliver
`structured_output` validated against a JSON schema.

| Kind | Shape |
|---|---|
| `code_fix` | single phase → branch / draft PR |
| `code_fix_planned` | plan → human approval → implement, via session resume |
| `code_fix_fanout` | planner → N parallel `code_fix` children → synthesizer (parent is read-only) |
| `code_review`, `report`, `triage` | read-only, structured report |

Templates live in `templates/<kind>/`; multi-phase kinds add `phases/<n>/`, fan-out kinds add
`child/`. `deliver.ByKind` routes the result to the right surface.

## Evaluation

Four layers, cheapest first:

1. **Checks** (`internal/eval/checks`) — acceptance commands, diff scope, schema validation. Deterministic, free.
2. **Judge** (`internal/eval/judge`) — an ephemeral read-only `claude -p` scoring against a rubric. It never sees the worker transcript; a judge failure is `uncertain`, never `pass`.
3. **Routing** (`internal/eval/route`) — a pure function: pass → deliver, fail → retry with feedback, uncertain → human.
4. **Golden suite** (`internal/eval/golden`, cases in `evals/golden/`) — a scored regression gate over fixed cases.

```bash
make golden-validate                      # free: every case parses and has its fixture
make golden                               # the whole suite under a cost cap (spends tokens)
bin/harness eval gate -baseline … -report …   # block a regression
```

## Deployment modes

| `worker.mode` | What runs the CLI |
|---|---|
| `local` | the host `claude` binary |
| `docker` | one container per run, on an internal network behind the egress allowlist proxy |
| `k8s` | one Job per run; requires `session.store: s3`, since a Job may land on any node |

Git always stays on the host — only the worker and acceptance commands run in containers.
Secrets reach a container as `-e NAME` only, and a Job takes its credential from
`envFrom: secretRef`, never from argv or the manifest.

```bash
make worker-image     # build the pinned worker image
make compose-up       # harness, egress proxy, OTel collector, Prometheus, Grafana, MinIO
make compose-down
make k8s-validate     # free: lint, render and validate the Helm chart in deploy/k8s
```

Store and queue are interfaces with SQLite (single node) and Postgres (stateless replicas)
implementations. Anything periodic takes a named lease first, so N replicas still run one
nightly cron.

## Development

```bash
make build            # bin/harness
make test             # race, all packages, no tokens
make lint             # golangci-lint v2
make live             # spawns the real CLI and spends tokens (needs a worker credential)
make docker-test      # needs a daemon and the worker image; no tokens
make postgres-test    # throwaway Postgres; no tokens
```

Tests that cost something or need infrastructure are build-tagged: `live` spends tokens,
`docker` needs a daemon, `postgres` needs `$HARNESS_TEST_POSTGRES_DSN`. Everything else runs
against fixtures in `testdata/events/`, captured from the pinned CLI — replay a fixture instead
of spending tokens.

`doctor` fails when the host `claude` drifts from the pinned version. Rather than downgrade the
CLI you use interactively, install the pinned one beside it and point the worker at that copy:

```bash
npm install --prefix ~/.local/share/foreman/cli-<version> @anthropic-ai/claude-code@<version>
# harness.yaml: worker.claude_bin: ~/.local/share/foreman/cli-<version>/node_modules/.bin/claude
```

### Layout

```
cmd/harness/        the single binary
internal/
  task/             types, phases, fan-out specs, run state machine
  store/ queue/     interfaces + sqlite/ and postgres/ implementations
  workspace/        provision and destroy an isolated checkout per run
  runner/           spawn and supervise `claude -p` (own process group)
  events/           NDJSON decoder
  session/          transcript snapshot and restore
  orchestrator/     pool, retries, phases, fan-out/fan-in, review, leases
  eval/             checks, judge, route, feedback, golden
  review/           human gate (+ Slack Block Kit, SLA escalation, pager)
  deliver/ intake/  PR / branch / report delivery; HTTP + webhook + cron triggers
  container/ k8s/   docker argv and Job manifest builders
  egress/           CONNECT allowlist proxy for workers
  budget/           daily ceilings and circuit breaker
  deadletter/ obs/  DLQ + paging; OTel metrics, per-run traces, live progress
  audit/ fanout/ config/
templates/          per-kind prompts and policies
deploy/             compose stack, Grafana dashboard, Helm chart (deploy/k8s)
evals/golden/       golden cases and fixtures
scripts/demo-m*.sh  end-to-end demos, offline by default
```

### Invariants worth knowing before you edit

- No LLM calls outside `runner` and `eval/judge`.
- Never pass user-controlled text through a shell — always `exec.Command` argv.
- Classify a `result` event on `is_error` + `terminal_reason`, never on `subtype` alone.
- `--session-id` XOR `--resume`, unless `--fork-session`; session ids are never reused.
- Kill the worker's process group, not just the pid.
- Fan-out children must be file-disjoint, or `fanout.Decode` refuses the plan.
- The HTTP API needs a bearer token on every route but `/healthz` and the HMAC-verified webhook, and
  `serve` refuses to start open on a non-loopback address.
- Workers never see the operator's git config, and no task kind carries an unrestricted `Bash(git *)` —
  otherwise a worker could push through the operator's credential helper and skip the whole evaluation
  pipeline.
- `data_root` lives outside this repo; `config.Load` refuses a root inside it, because the CLI would
  otherwise walk up and feed the harness's own `CLAUDE.md` to every worker.

The full list is in [CLAUDE.md](CLAUDE.md).
